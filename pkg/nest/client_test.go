package nest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// The dial gate only works if rtcConn waits on it before returning the Conn: OnTrack
// narrows the video media.Codecs and writes sprop-parameter-sets, and the streams layer
// starts reading both as soon as Dial returns. So, as a consumer would, look at the
// Conn the moment rtcConn returns: both writes must already be done.
func TestRtcConnReturnsOnlyAfterTheGate(t *testing.T) {
	cam := &loopbackCamera{t: t}
	old := http.DefaultTransport
	http.DefaultTransport = cam // ExchangeSDP's client uses it
	t.Cleanup(func() { http.DefaultTransport = old })
	oldConf := rtcConfig
	rtcConfig = pion.Configuration{} // no STUN: host candidates only
	t.Cleanup(func() { rtcConfig = oldConf })

	client, err := rtcConn(&API{Token: "test"}, "nest:test", "project", "device")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Stop() })

	var video *core.Media
	for _, media := range client.GetMedias() {
		if media.Kind == core.KindVideo {
			video = media
		}
	}
	require.NotNil(t, video)
	require.Len(t, video.Codecs, 1, "rtcConn returned before OnTrack narrowed the video codecs")
	require.Contains(t, video.Codecs[0].FmtpLine, "sprop-parameter-sets=",
		"rtcConn returned before OnTrack wrote sprop-parameter-sets")
}

// loopbackCamera answers GenerateWebRtcStream like Google, with a local pion peer that
// sends H264: non-IDR slices, then SPS/PPS from 300ms after it connects.
type loopbackCamera struct {
	t  *testing.T
	mu sync.Mutex
}

func (c *loopbackCamera) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	require.True(t, strings.HasSuffix(req.URL.Path, ":executeCommand"), req.URL.String())

	var reqv struct {
		Params struct {
			Offer string `json:"offerSdp"`
		} `json:"params"`
	}
	require.NoError(t, json.NewDecoder(req.Body).Decode(&reqv))

	pc, err := pion.NewPeerConnection(pion.Configuration{})
	require.NoError(t, err)
	track, err := pion.NewTrackLocalStaticRTP(pion.RTPCodecCapability{
		MimeType: pion.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "video", "cam")
	require.NoError(t, err)
	_, err = pc.AddTrack(track)
	require.NoError(t, err)

	require.NoError(t, pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: reqv.Params.Offer}))
	answer, err := pc.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := pion.GatheringCompletePromise(pc)
	require.NoError(t, pc.SetLocalDescription(answer))
	<-gathered

	stop := make(chan struct{})
	done := make(chan struct{})
	go sendH264(pc, track, stop, done)
	t.Cleanup(func() {
		close(stop)
		<-done
		_ = pc.Close()
	})

	var resv struct {
		Results struct {
			Answer         string    `json:"answerSdp"`
			ExpiresAt      time.Time `json:"expiresAt"`
			MediaSessionID string    `json:"mediaSessionId"`
		} `json:"results"`
	}
	resv.Results.Answer = pc.LocalDescription().SDP
	resv.Results.ExpiresAt = time.Now().Add(5 * time.Minute)
	resv.Results.MediaSessionID = "test"
	b, err := json.Marshal(resv)
	require.NoError(t, err)

	return &http.Response{
		StatusCode: 200, Status: "200 OK", Header: http.Header{},
		Body: io.NopCloser(bytes.NewReader(b)), Request: req,
	}, nil
}

func sendH264(pc *pion.PeerConnection, track *pion.TrackLocalStaticRTP, stop, done chan struct{}) {
	defer close(done)
	sps := []byte{0x67, 0x42, 0xe0, 0x1f, 0x8c, 0x8d, 0x40, 0x50, 0x1e, 0xd0}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}

	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	var start time.Time
	var seq uint16
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		if start.IsZero() {
			if pc.ConnectionState() != pion.PeerConnectionStateConnected {
				continue
			}
			start = time.Now()
		}

		seq++
		payload := []byte{0x41, 0x9a, 0x00, 0x01} // non-IDR slice
		if time.Since(start) >= 300*time.Millisecond && seq%20 == 0 {
			payload = []byte{24} // STAP-A: SPS + PPS
			for _, nal := range [][]byte{sps, pps} {
				payload = binary.BigEndian.AppendUint16(payload, uint16(len(nal)))
				payload = append(payload, nal...)
			}
		}
		_ = track.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: seq, Timestamp: uint32(seq) * 900},
			Payload: payload,
		})
	}
}
