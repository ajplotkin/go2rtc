package webrtc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func withNestTimeouts(t *testing.T, video, sprop time.Duration) {
	oldVideo, oldSprop := NestVideoTimeout, NestSpropTimeout
	NestVideoTimeout, NestSpropTimeout = video, sprop
	t.Cleanup(func() { NestVideoTimeout, NestSpropTimeout = oldVideo, oldSprop })
}

// SPS/PPS that arrive after WaitNestVideo gave up on them must not be written:
// the Conn is published by then and consumers are reading the codec.
func TestNestGate_LateSpropIsNotWritten(t *testing.T) {
	withNestTimeouts(t, 5*time.Second, 200*time.Millisecond)
	conn, _ := dialFakeCamera(t, "nest/webrtc", camOpts{spropAfter: time.Second})
	require.NoError(t, conn.WaitNestVideo())

	// read straight through the moment the camera starts sending SPS/PPS
	fmtp := readVideoFmtp(conn, 2*time.Second)
	require.NotContains(t, fmtp, "sprop-parameter-sets=")
}

// A camera that never sends SPS/PPS still gets its Conn, after the sprop window.
func TestNestGate_NoSpropStillPublishes(t *testing.T) {
	withNestTimeouts(t, 5*time.Second, 300*time.Millisecond)
	conn, _ := dialFakeCamera(t, "nest/webrtc", camOpts{spropAfter: -1})

	start := time.Now()
	require.NoError(t, conn.WaitNestVideo())
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
	require.False(t, strings.Contains(readVideoFmtp(conn, 100*time.Millisecond), "sprop-parameter-sets="))
}

// A video codec whose answer already carries sprop-parameter-sets has nothing to
// capture, so the gate must not sit out NestSpropTimeout waiting for SPS/PPS.
func TestNestGate_SpropInAnswerDoesNotWait(t *testing.T) {
	withNestTimeouts(t, 5*time.Second, 10*time.Second)
	conn, _ := dialFakeCamera(t, "nest/webrtc", camOpts{spropAfter: -1,
		answerFmtp: "sprop-parameter-sets=Z0LgH4yNQFAe0A==,aM48gA=="})

	start := time.Now()
	require.NoError(t, conn.WaitNestVideo())
	require.Less(t, time.Since(start), 5*time.Second, "waited for SPS/PPS the answer already had")
	require.Equal(t, 1, strings.Count(videoMedia(t, conn).Codecs[0].FmtpLine, "sprop-parameter-sets="))
}

// No video at all is not a dial failure: the Conn is returned once NestVideoTimeout
// passes, and the connect watchdog and reconnect loop deal with it as they always did.
func TestNestGate_NoVideoStillPublishes(t *testing.T) {
	withNestTimeouts(t, 300*time.Millisecond, time.Second)
	conn := newNestConn(t)

	start := time.Now()
	require.NoError(t, conn.WaitNestVideo())
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
}

// A video track that arrives after NestVideoTimeout must write nothing shared, and
// must still reach a consumer that attached in the meantime. The answer lists H264
// at PT 96 first, while the camera sends PT 97, so this is the #2389 mismatch.
func TestNestGate_LateVideoAfterTimeout(t *testing.T) {
	withNestTimeouts(t, 200*time.Millisecond, 200*time.Millisecond)
	conn, _ := dialFakeCamera(t, "nest/webrtc", camOpts{spropAfter: 300 * time.Millisecond, videoAfter: time.Second})
	require.NoError(t, conn.WaitNestVideo())

	video := attachConsumer(t, conn, core.KindVideo, core.CodecH264)

	fmtp := readVideoFmtp(conn, 2*time.Second) // across the late track's arrival
	require.NotContains(t, fmtp, "sprop-parameter-sets=")
	require.Eventually(t, func() bool { return video.Load() > 0 }, 3*time.Second, 20*time.Millisecond,
		"late video was not delivered to the consumer")
	require.Greater(t, len(videoMedia(t, conn).Codecs), 1, "late track narrowed a published media")
}

// The point of the gate for #2389: once WaitNestVideo returns, the video media lists
// only the codec actually sent, so a consumer's MatchMedia/GetTrack lands on the
// receiver OnTrack feeds, even though the answer listed another H264 first.
func TestNestGate_VideoNarrowedBeforePublish(t *testing.T) {
	withNestTimeouts(t, 5*time.Second, 200*time.Millisecond)
	conn, cam := dialFakeCamera(t, "nest/webrtc", camOpts{spropAfter: -1})
	require.NoError(t, conn.WaitNestVideo())

	media := videoMedia(t, conn)
	require.Len(t, media.Codecs, 1)
	sent := media.Codecs[0]
	require.Contains(t, sent.FmtpLine, "profile-level-id=42e01f") // the camera's track
	require.NotEqual(t, firstH264PT(t, cam.pc.LocalDescription().SDP), sent.PayloadType,
		"answer must list another H264 first, or this doesn't test #2389")

	conn.tracksMu.Lock()
	fed := append([]*core.Receiver(nil), conn.Receivers...)
	conn.tracksMu.Unlock()
	require.Len(t, fed, 1) // OnTrack's

	want := &core.Media{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecH264}}}
	prodCodec, _ := media.MatchMedia(want)
	track, err := conn.GetTrack(media, prodCodec)
	require.NoError(t, err)
	require.Same(t, fed[0], track)
}

func videoMedia(t *testing.T, conn *Conn) *core.Media {
	for _, media := range conn.GetMedias() {
		if media.Kind == core.KindVideo {
			return media
		}
	}
	t.Fatal("no video media")
	return nil
}

func firstH264PT(t *testing.T, sdp string) uint8 {
	for _, line := range strings.Split(sdp, "\n") {
		var pt uint8
		if _, err := fmt.Sscanf(line, "a=rtpmap:%d H264/", &pt); err == nil {
			return pt
		}
	}
	t.Fatal("no H264 in the answer")
	return 0
}

// A connection that closes during the wait fails the dial at once.
func TestNestGate_CloseFailsAtOnce(t *testing.T) {
	withNestTimeouts(t, 10*time.Second, time.Second)
	conn := newNestConn(t)

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close()
	}()

	start := time.Now()
	require.ErrorContains(t, conn.WaitNestVideo(), "closed")
	require.Less(t, time.Since(start), 5*time.Second)
}

func newNestConn(t *testing.T) *Conn {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	conn := NewConn(pc)
	conn.FormatName = "nest/webrtc"
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
