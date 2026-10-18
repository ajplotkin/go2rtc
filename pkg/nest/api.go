package nest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type API struct {
	Token     string
	ExpiresAt time.Time

	StreamProjectID string
	StreamDeviceID  string
	StreamExpiresAt time.Time

	// WebRTC
	StreamSessionID string

	// RTSP
	StreamToken          string
	StreamExtensionToken string

	key string // credentials cache key, used to refresh the OAuth token

	extendTimer *time.Timer
	extendStop  chan struct{}
	extendDone  chan struct{}
}

type Auth struct {
	AccessToken string
}

type DeviceInfo struct {
	Name      string
	DeviceID  string
	Protocols []string
}

var cache = map[string]*API{}
var cacheMu sync.Mutex

// Endpoints, variables so tests can point them at a local server.
var (
	tokenURL = "https://www.googleapis.com/oauth2/v4/token"
	sdmURL   = "https://smartdevicemanagement.googleapis.com/v1/enterprises/"
)

// Backoff before retrying GenerateWebRtcStream after a 409 or 429; doubles each retry.
var busyRetryDelay = time.Second * 30

// A cached token this close to its expiry is not handed out: it might expire
// before the caller's request reaches Google. The extend loop refreshes within the same margin.
const tokenExpiryMargin = 30 * time.Second

func NewAPI(clientID, clientSecret, refreshToken string) (*API, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	key := clientID + ":" + clientSecret + ":" + refreshToken

	// The cache only stores the OAuth token. Each caller gets its own API
	// instance, because the Stream* fields hold per-stream session state -
	// multiple cameras sharing one instance overwrite each other's session
	// and only the last one gets extended.
	if api := cache[key]; api != nil && time.Now().Add(tokenExpiryMargin).Before(api.ExpiresAt) {
		return &API{Token: api.Token, ExpiresAt: api.ExpiresAt, key: key}, nil
	}

	api, err := fetchToken(clientID, clientSecret, refreshToken)
	if err != nil {
		return nil, err
	}

	cache[key] = api

	return &API{Token: api.Token, ExpiresAt: api.ExpiresAt, key: key}, nil
}

// fetchToken requests a new access token from Google. The caller holds cacheMu.
func fetchToken(clientID, clientSecret, refreshToken string) (*API, error) {
	now := time.Now()

	data := url.Values{
		"grant_type":    []string{"refresh_token"},
		"client_id":     []string{clientID},
		"client_secret": []string{clientSecret},
		"refresh_token": []string{refreshToken},
	}

	client := &http.Client{Timeout: time.Second * 10}
	res, err := client.PostForm(tokenURL, data)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		AccessToken string        `json:"access_token"`
		ExpiresIn   time.Duration `json:"expires_in"`
		Scope       string        `json:"scope"`
		TokenType   string        `json:"token_type"`
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return nil, err
	}

	if resv.AccessToken == "" {
		return nil, errors.New("nest: no access token in response")
	}

	return &API{
		Token:     resv.AccessToken,
		ExpiresAt: now.Add(resv.ExpiresIn * time.Second),
	}, nil
}

// do sends a request to the SDM API. A 401 means Google no longer accepts the
// token, even if its clock expiry is still ahead (Google can revoke a token
// early), so get another one and send the request once more.
func (a *API) do(method, uri string, body []byte) (*http.Response, error) {
	client := &http.Client{Timeout: time.Second * 10}

	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, uri, reader)
		if err != nil {
			return nil, err
		}

		token := a.Token
		req.Header.Set("Authorization", "Bearer "+token)

		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		if res.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return res, nil
		}
		// Read to the end before closing, so the connection goes back to the pool
		// before the token request needs one. Go before 1.27 drops a connection
		// whose body is closed unread.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()

		if err = a.replaceToken(token); err != nil {
			return nil, err
		}
	}
}

func (a *API) GetDevices(projectID string) ([]DeviceInfo, error) {
	res, err := a.do("GET", sdmURL+projectID+"/devices", nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		Devices []Device
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return nil, err
	}

	devices := make([]DeviceInfo, 0, len(resv.Devices))

	for _, device := range resv.Devices {
		// only RTSP and WEB_RTC available (both supported)
		if len(device.Traits.SdmDevicesTraitsCameraLiveStream.SupportedProtocols) == 0 {
			continue
		}

		i := strings.LastIndexByte(device.Name, '/')
		if i <= 0 {
			continue
		}

		name := device.Traits.SdmDevicesTraitsInfo.CustomName
		// Devices configured through the Nest app use the container/room name as opposed to the customName trait
		if name == "" && len(device.ParentRelations) > 0 {
			name = device.ParentRelations[0].DisplayName
		}

		devices = append(devices, DeviceInfo{
			Name:      name,
			DeviceID:  device.Name[i+1:],
			Protocols: device.Traits.SdmDevicesTraitsCameraLiveStream.SupportedProtocols,
		})
	}

	return devices, nil
}

func (a *API) ExchangeSDP(projectID, deviceID, offer string) (string, error) {
	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			Offer string `json:"offerSdp"`
		} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream"
	reqv.Params.Offer = offer

	b, err := json.Marshal(reqv)
	if err != nil {
		return "", err
	}

	uri := sdmURL + projectID + "/devices/" + deviceID + ":executeCommand"

	maxRetries := 3
	retryDelay := busyRetryDelay

	for attempt := 0; attempt < maxRetries; attempt++ {
		// a.do already replaced the token and retried on a 401
		res, err := a.do("POST", uri, b)
		if err != nil {
			return "", err
		}

		// Handle 409 (Conflict) and 429 (Too Many Requests). A new token
		// would not help with either, so only back off.
		if res.StatusCode == 409 || res.StatusCode == 429 {
			res.Body.Close()
			if attempt < maxRetries-1 {
				time.Sleep(retryDelay)
				retryDelay *= 2 // exponential backoff
				continue
			}
		}

		defer res.Body.Close()

		if res.StatusCode != 200 {
			return "", errors.New("nest: wrong status: " + res.Status)
		}

		var resv struct {
			Results struct {
				Answer         string    `json:"answerSdp"`
				ExpiresAt      time.Time `json:"expiresAt"`
				MediaSessionID string    `json:"mediaSessionId"`
			} `json:"results"`
		}

		if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
			return "", err
		}

		a.StreamProjectID = projectID
		a.StreamDeviceID = deviceID
		a.StreamSessionID = resv.Results.MediaSessionID
		a.StreamExpiresAt = resv.Results.ExpiresAt

		return resv.Results.Answer, nil
	}

	return "", errors.New("nest: max retries exceeded")
}

// credentials parses the OAuth credentials from the cache key.
func (a *API) credentials() (clientID, clientSecret, refreshToken string, err error) {
	if a.key == "" {
		return "", "", "", errors.New("nest: unable to find cached credentials")
	}

	parts := strings.SplitN(a.key, ":", 3)
	if len(parts) != 3 {
		return "", "", "", errors.New("nest: invalid cache key format")
	}
	return parts[0], parts[1], parts[2], nil
}

// refreshToken renews a token that is about to expire. It goes through the
// cache, so it is only for the clock-based refresh: after a 401 the cached
// token can be the rejected one, see replaceToken.
func (a *API) refreshToken() error {
	clientID, clientSecret, refreshToken, err := a.credentials()
	if err != nil {
		return err
	}

	// Get new API instance which will refresh the token
	newAPI, err := NewAPI(clientID, clientSecret, refreshToken)
	if err != nil {
		return err
	}

	// Update current API with new token
	a.Token = newAPI.Token
	a.ExpiresAt = newAPI.ExpiresAt
	return nil
}

// replaceToken gets a new token after Google rejected the token `failed` with a
// 401. The cache cannot be trusted here: it may still hold `failed` within its
// clock expiry, and every camera on the account shares that entry. So unless
// another camera has already replaced it, request a new token and store it for
// all of them. cacheMu is held across the request, so cameras that get a 401
// at the same moment make one token request between them.
func (a *API) replaceToken(failed string) error {
	clientID, clientSecret, refreshToken, err := a.credentials()
	if err != nil {
		return err
	}

	cacheMu.Lock()
	defer cacheMu.Unlock()

	if api := cache[a.key]; api != nil && api.Token != failed &&
		time.Now().Add(tokenExpiryMargin).Before(api.ExpiresAt) {
		a.Token = api.Token
		a.ExpiresAt = api.ExpiresAt
		return nil
	}

	// Drop the rejected token first, so that if the request fails the next
	// NewAPI asks Google again instead of handing it out.
	if api := cache[a.key]; api != nil && api.Token == failed {
		delete(cache, a.key)
	}

	api, err := fetchToken(clientID, clientSecret, refreshToken)
	if err != nil {
		return err
	}

	cache[a.key] = api

	a.Token = api.Token
	a.ExpiresAt = api.ExpiresAt
	return nil
}

func (a *API) ExtendStream() error {
	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			MediaSessionID       string `json:"mediaSessionId,omitempty"`
			StreamExtensionToken string `json:"streamExtensionToken,omitempty"`
		} `json:"params"`
	}

	if a.StreamToken != "" {
		// RTSP
		reqv.Command = "sdm.devices.commands.CameraLiveStream.ExtendRtspStream"
		reqv.Params.StreamExtensionToken = a.StreamExtensionToken
	} else {
		// WebRTC
		reqv.Command = "sdm.devices.commands.CameraLiveStream.ExtendWebRtcStream"
		reqv.Params.MediaSessionID = a.StreamSessionID
	}

	b, err := json.Marshal(reqv)
	if err != nil {
		return err
	}

	uri := sdmURL + a.StreamProjectID + "/devices/" + a.StreamDeviceID + ":executeCommand"
	res, err := a.do("POST", uri, b)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		Results struct {
			ExpiresAt            time.Time `json:"expiresAt"`
			MediaSessionID       string    `json:"mediaSessionId"`
			StreamExtensionToken string    `json:"streamExtensionToken"`
			StreamToken          string    `json:"streamToken"`
		} `json:"results"`
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return err
	}

	a.StreamSessionID = resv.Results.MediaSessionID
	a.StreamExpiresAt = resv.Results.ExpiresAt
	a.StreamExtensionToken = resv.Results.StreamExtensionToken
	a.StreamToken = resv.Results.StreamToken

	return nil
}

func (a *API) GenerateRtspStream(projectID, deviceID string) (string, error) {
	var reqv struct {
		Command string   `json:"command"`
		Params  struct{} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.GenerateRtspStream"

	b, err := json.Marshal(reqv)
	if err != nil {
		return "", err
	}

	uri := sdmURL + projectID + "/devices/" + deviceID + ":executeCommand"
	res, err := a.do("POST", uri, b)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return "", errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		Results struct {
			StreamURLs           map[string]string `json:"streamUrls"`
			StreamExtensionToken string            `json:"streamExtensionToken"`
			StreamToken          string            `json:"streamToken"`
			ExpiresAt            time.Time         `json:"expiresAt"`
		} `json:"results"`
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return "", err
	}

	if _, ok := resv.Results.StreamURLs["rtspUrl"]; !ok {
		return "", errors.New("nest: failed to generate rtsp url")
	}

	a.StreamProjectID = projectID
	a.StreamDeviceID = deviceID
	a.StreamToken = resv.Results.StreamToken
	a.StreamExtensionToken = resv.Results.StreamExtensionToken
	a.StreamExpiresAt = resv.Results.ExpiresAt

	return resv.Results.StreamURLs["rtspUrl"], nil
}

func (a *API) StopRTSPStream() error {
	if a.StreamProjectID == "" || a.StreamDeviceID == "" {
		return errors.New("nest: tried to stop rtsp stream without a project or device ID")
	}

	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			StreamExtensionToken string `json:"streamExtensionToken"`
		} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.StopRtspStream"
	reqv.Params.StreamExtensionToken = a.StreamExtensionToken

	b, err := json.Marshal(reqv)
	if err != nil {
		return err
	}

	uri := sdmURL + a.StreamProjectID + "/devices/" + a.StreamDeviceID + ":executeCommand"
	res, err := a.do("POST", uri, b)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return errors.New("nest: wrong status: " + res.Status)
	}

	a.StreamProjectID = ""
	a.StreamDeviceID = ""
	a.StreamExtensionToken = ""
	a.StreamToken = ""

	return nil
}

type Device struct {
	Name string `json:"name"`
	Type string `json:"type"`
	//Assignee string `json:"assignee"`
	Traits struct {
		SdmDevicesTraitsInfo struct {
			CustomName string `json:"customName"`
		} `json:"sdm.devices.traits.Info"`
		SdmDevicesTraitsCameraLiveStream struct {
			VideoCodecs        []string `json:"videoCodecs"`
			AudioCodecs        []string `json:"audioCodecs"`
			SupportedProtocols []string `json:"supportedProtocols"`
		} `json:"sdm.devices.traits.CameraLiveStream"`
		//SdmDevicesTraitsCameraImage struct {
		//	MaxImageResolution struct {
		//		Width  int `json:"width"`
		//		Height int `json:"height"`
		//	} `json:"maxImageResolution"`
		//} `json:"sdm.devices.traits.CameraImage"`
		//SdmDevicesTraitsCameraPerson struct {
		//} `json:"sdm.devices.traits.CameraPerson"`
		//SdmDevicesTraitsCameraMotion struct {
		//} `json:"sdm.devices.traits.CameraMotion"`
		//SdmDevicesTraitsDoorbellChime struct {
		//} `json:"sdm.devices.traits.DoorbellChime"`
		//SdmDevicesTraitsCameraClipPreview struct {
		//} `json:"sdm.devices.traits.CameraClipPreview"`
	} `json:"traits"`
	ParentRelations []struct {
		Parent      string `json:"parent"`
		DisplayName string `json:"displayName"`
	} `json:"parentRelations"`
}

func (a *API) StartExtendStreamTimer() {
	if a.extendTimer != nil {
		return
	}

	// Google expires sessions after ~5 minutes; each successful extension
	// returns a new expiresAt, so keep extending until the stream stops.
	timer := time.NewTimer(time.Until(a.StreamExpiresAt) - time.Minute)
	stop := make(chan struct{})
	done := make(chan struct{})
	a.extendTimer = timer
	a.extendStop = stop
	a.extendDone = done

	go func() {
		defer close(done)
		// Retry transient extend/refresh failures a few times before giving up, so a single
		// blip (one 10s timeout, a 429/401) does not permanently stop extension and leave the
		// session to expire (which stalls the stream). If it truly can't extend, the loop exits
		// and the stall watchdog in pkg/webrtc reconnects.
		fails := 0
		for {
			select {
			case <-timer.C:
				// The OAuth token lives ~1 hour, sessions can live longer
				if time.Now().After(a.ExpiresAt.Add(-tokenExpiryMargin)) {
					if err := a.refreshToken(); err != nil {
						if fails++; fails <= 3 {
							timer.Reset(15 * time.Second)
							continue
						}
						return
					}
				}
				if err := a.ExtendStream(); err != nil {
					if fails++; fails <= 3 {
						timer.Reset(15 * time.Second)
						continue
					}
					return
				}
				fails = 0
				// Clamp so a zero/past/short expiresAt from Google can't hot-loop ExtendStream
				// at HTTP-round-trip rate (which would 429 and kill the goroutine).
				d := time.Until(a.StreamExpiresAt) - time.Minute
				if d < 30*time.Second {
					d = 30 * time.Second
				}
				timer.Reset(d)
			case <-stop:
				return
			}
		}
	}()
}

func (a *API) StopExtendStreamTimer() {
	if a.extendStop != nil {
		close(a.extendStop)
		a.extendStop = nil
	}
	if a.extendTimer != nil {
		a.extendTimer.Stop()
		a.extendTimer = nil
	}
	// Wait for the goroutine to actually exit before returning, so a caller (RTSPClient.Stop)
	// can't race an in-flight ExtendStream writing StreamToken/StreamExtensionToken against
	// StopRTSPStream clearing them. Bounded by a few 10s HTTP timeouts: the extend
	// request, a token request before it and, after a 401, a token request and the
	// retry, each token request possibly queued behind other cameras' on cacheMu.
	if a.extendDone != nil {
		<-a.extendDone
		a.extendDone = nil
	}
}
