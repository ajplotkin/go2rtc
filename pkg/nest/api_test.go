package nest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeGoogle stands in for the OAuth token endpoint and the SDM API.
// Each token request hands out the next token: tok1, tok2, ...
// SDM requests made with a token in revoked get a 401, even though that
// token's clock expiry is still an hour away.
type fakeGoogle struct {
	tokenRequests atomic.Int32
	tokenFail     atomic.Bool  // the token endpoint answers 500
	expiresIn     atomic.Int32 // seconds, 0 means 3600

	mu       sync.Mutex
	revoked  map[string]bool
	status   []int    // SDM statuses to return before the normal answer, one per request
	bearers  []string // the token on every SDM request, in order
	onSDM401 func()   // called before an SDM 401 is written
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{revoked: map[string]bool{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		n := g.tokenRequests.Add(1)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.PostForm.Get("grant_type"))
		require.Equal(t, "refresh", r.PostForm.Get("refresh_token"))
		if g.tokenFail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		expiresIn := g.expiresIn.Load()
		if expiresIn == 0 {
			expiresIn = 3600
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok%d", n),
			"expires_in":   expiresIn,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/sdm/", func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		g.mu.Lock()
		g.bearers = append(g.bearers, bearer)
		revoked := g.revoked[bearer]
		status := 0
		if len(g.status) > 0 {
			status, g.status = g.status[0], g.status[1:]
		}
		on401 := g.onSDM401
		g.mu.Unlock()

		if revoked {
			if on401 != nil {
				on401()
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": map[string]any{
				"answerSdp":      "answer",
				"expiresAt":      time.Now().Add(5 * time.Minute),
				"mediaSessionId": "session",
			},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldToken, oldSDM, oldDelay := tokenURL, sdmURL, busyRetryDelay
	tokenURL = srv.URL + "/token"
	sdmURL = srv.URL + "/sdm/"
	busyRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { tokenURL, sdmURL, busyRetryDelay = oldToken, oldSDM, oldDelay })

	cacheMu.Lock()
	cache = map[string]*API{}
	cacheMu.Unlock()

	return g
}

func (g *fakeGoogle) revoke(token string) {
	g.mu.Lock()
	g.revoked[token] = true
	g.mu.Unlock()
}

func (g *fakeGoogle) setStatus(status ...int) {
	g.mu.Lock()
	g.status = status
	g.mu.Unlock()
}

func (g *fakeGoogle) sdmBearers() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.bearers...)
}

func cachedToken(t *testing.T) string {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	api := cache["client:secret:refresh"]
	require.NotNil(t, api, "no cache entry")
	return api.Token
}

func newTestAPI(t *testing.T) *API {
	api, err := NewAPI("client", "secret", "refresh")
	require.NoError(t, err)
	return api
}

// Google revokes a token before its clock expiry. A 401 must get a new token
// right away, not the cached one back, and the retry must succeed without the
// 409/429 backoff.
func TestExchangeSDP401GetsNewToken(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)
	require.Equal(t, "tok1", api.Token)
	g.revoke("tok1")

	answer, err := api.ExchangeSDP("project", "device", "offer")
	require.NoError(t, err)
	require.Equal(t, "answer", answer)
	require.Equal(t, "tok2", api.Token)
	require.Equal(t, int32(2), g.tokenRequests.Load())
	require.Equal(t, []string{"tok1", "tok2"}, g.sdmBearers())
}

// The new token replaces the shared cache entry, so the next camera to dial
// starts with it instead of the revoked one, with no token request of its own.
func TestReplacedTokenIsShared(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)
	g.revoke("tok1")

	_, err := api.ExchangeSDP("project", "device", "offer")
	require.NoError(t, err)
	require.Equal(t, "tok2", cachedToken(t))

	other := newTestAPI(t)
	require.Equal(t, "tok2", other.Token)
	require.Equal(t, int32(2), g.tokenRequests.Load())
}

// Cameras that hit a 401 at the same moment make one token request between them.
func TestConcurrent401sMakeOneTokenRequest(t *testing.T) {
	g := newFakeGoogle(t)
	const cameras = 4
	apis := make([]*API, cameras)
	for i := range apis {
		apis[i] = newTestAPI(t)
	}
	require.Equal(t, int32(1), g.tokenRequests.Load())
	g.revoke("tok1")

	// Hold every 401 until all cameras have sent their request, so all of
	// them hold the revoked token when they ask for a new one.
	var arrived sync.WaitGroup
	arrived.Add(cameras)
	g.mu.Lock()
	g.onSDM401 = func() {
		arrived.Done()
		arrived.Wait()
	}
	g.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, cameras)
	for i, api := range apis {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = api.ExchangeSDP("project", fmt.Sprintf("device%d", i), "offer")
		}()
	}
	wg.Wait()

	for i, api := range apis {
		require.NoError(t, errs[i])
		require.Equal(t, "tok2", api.Token)
	}
	require.Equal(t, int32(2), g.tokenRequests.Load())
}

// A camera whose token is older than the cache's (another camera already
// replaced it) takes the cached one instead of asking Google again.
func TestStale401AdoptsCachedToken(t *testing.T) {
	g := newFakeGoogle(t)
	first := newTestAPI(t)
	second := newTestAPI(t)
	g.revoke("tok1")

	_, err := first.ExchangeSDP("project", "device1", "offer")
	require.NoError(t, err)
	require.Equal(t, int32(2), g.tokenRequests.Load())

	_, err = second.ExchangeSDP("project", "device2", "offer")
	require.NoError(t, err)
	require.Equal(t, "tok2", second.Token)
	require.Equal(t, int32(2), g.tokenRequests.Load())
}

// A 409 or 429 is not a token problem: back off and retry with the same
// token, and make no token request.
func TestBusyMakesNoTokenRequest(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			g := newFakeGoogle(t)
			api := newTestAPI(t)
			g.setStatus(status, status)

			answer, err := api.ExchangeSDP("project", "device", "offer")
			require.NoError(t, err)
			require.Equal(t, "answer", answer)
			require.Equal(t, int32(1), g.tokenRequests.Load())
			require.Equal(t, []string{"tok1", "tok1", "tok1"}, g.sdmBearers())
		})
	}
}

// The normal path uses the cached token: no extra token request.
func TestCachedPathMakesNoTokenRequest(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)

	_, err := api.ExchangeSDP("project", "device", "offer")
	require.NoError(t, err)
	require.NoError(t, api.ExtendStream())
	_ = newTestAPI(t)
	require.Equal(t, int32(1), g.tokenRequests.Load())
	require.Equal(t, []string{"tok1", "tok1"}, g.sdmBearers())
}

// Other failures than 401 make no token request either.
func TestOtherErrorsMakeNoTokenRequest(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)
	g.setStatus(http.StatusBadRequest)

	_, err := api.ExchangeSDP("project", "device", "offer")
	require.ErrorContains(t, err, "400")
	require.Equal(t, int32(1), g.tokenRequests.Load())
}

// A 401 on the token that was just fetched fails rather than loops.
func TestSecond401Fails(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)
	g.revoke("tok1")
	g.revoke("tok2")

	_, err := api.ExchangeSDP("project", "device", "offer")
	require.ErrorContains(t, err, "401")
	require.Equal(t, []string{"tok1", "tok2"}, g.sdmBearers())
}

// The extend loop's ExtendStream gets the same 401 handling, so a revoked
// token does not stop extension and stall the stream.
func TestExtendStream401GetsNewToken(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)
	_, err := api.ExchangeSDP("project", "device", "offer")
	require.NoError(t, err)
	g.revoke("tok1")

	require.NoError(t, api.ExtendStream())
	require.Equal(t, "tok2", api.Token)
	require.Equal(t, "tok2", cachedToken(t))
	require.Equal(t, int32(2), g.tokenRequests.Load())
}

// If the token request after a 401 fails, the rejected token must not stay in
// the cache: the next dial asks Google again instead of starting with it.
func TestFailedReplaceDropsRejectedToken(t *testing.T) {
	g := newFakeGoogle(t)
	api := newTestAPI(t)
	g.revoke("tok1")
	g.tokenFail.Store(true)

	_, err := api.ExchangeSDP("project", "device", "offer")
	require.ErrorContains(t, err, "500")

	g.tokenFail.Store(false)
	other := newTestAPI(t)
	require.Equal(t, "tok3", other.Token)
}

// A cached token about to expire is not handed out.
func TestNearlyExpiredTokenIsNotCached(t *testing.T) {
	g := newFakeGoogle(t)
	g.expiresIn.Store(20)
	require.Equal(t, "tok1", newTestAPI(t).Token)
	g.expiresIn.Store(0)
	require.Equal(t, "tok2", newTestAPI(t).Token)
}

func TestEmptyAccessTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	old := tokenURL
	tokenURL = srv.URL
	t.Cleanup(func() { tokenURL = old })
	cacheMu.Lock()
	cache = map[string]*API{}
	cacheMu.Unlock()

	_, err := NewAPI("client", "secret", "refresh")
	require.Error(t, err)
}
