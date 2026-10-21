package webrtc

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// These tests are meant to be run with -race. Each one drives a code path that
// had an unsynchronized access, so without the fix the race detector fails it.

// fakeCamera is a pion peer on loopback that answers like a camera: it sends
// H264 video and, optionally, Opus audio.
type fakeCamera struct {
	pc    *webrtc.PeerConnection
	video *webrtc.TrackLocalStaticRTP
	audio *webrtc.TrackLocalStaticRTP

	camOpts
	stop chan struct{}
	done sync.WaitGroup
}

// camOpts times a fakeCamera's streams, counted from the moment it connects.
type camOpts struct {
	audio      bool
	spropAfter time.Duration // send SPS/PPS only this long after connecting (-1: never)
	audioAfter time.Duration // start audio only this long after connecting
	videoAfter time.Duration // start video only this long after connecting
	answerFmtp string        // appended to every H264 fmtp line of the camera's answer
}

// dialFakeCamera connects a go2rtc Conn (as an active producer, the way
// pkg/nest does) to a fakeCamera, and starts the camera sending.
func dialFakeCamera(t *testing.T, formatName string, opts camOpts) (*Conn, *fakeCamera) {
	t.Helper()

	api, err := NewAPI()
	require.NoError(t, err)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)

	conn := NewConn(pc)
	conn.FormatName = formatName
	conn.Mode = core.ModeActiveProducer

	offer, err := conn.CreateCompleteOffer([]*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		{Kind: core.KindVideo, Direction: core.DirectionRecvonly},
		{Kind: "app"},
	})
	require.NoError(t, err)

	cam := &fakeCamera{camOpts: opts, stop: make(chan struct{})}
	cam.pc, err = webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)

	cam.video, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "video", "cam")
	require.NoError(t, err)
	_, err = cam.pc.AddTrack(cam.video)
	require.NoError(t, err)

	if opts.audio {
		cam.audio, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		}, "audio", "cam")
		require.NoError(t, err)
		_, err = cam.pc.AddTrack(cam.audio)
		require.NoError(t, err)
	}

	require.NoError(t, cam.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}))
	answer, err := cam.pc.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(cam.pc)
	require.NoError(t, cam.pc.SetLocalDescription(answer))
	<-gathered

	// Start sending BEFORE SetAnswer. A goroutine started after it would give the
	// race detector a happens-before edge from everything SetAnswer wrote to the
	// camera's sends, and from there (through pion's shared locks) to our OnTrack.
	// A real camera has no such edge, and the edge hid the SetAnswer race.
	cam.done.Add(1)
	go cam.send()

	answerSDP := cam.pc.LocalDescription().SDP
	if opts.answerFmtp != "" {
		// pion's answer echoes the offer's fmtp, so a camera that puts more in its
		// answer, as a camera could, is simulated by editing the SDP.
		var h264 []string
		for _, line := range strings.Split(answerSDP, "\r\n") {
			var pt int
			if _, err := fmt.Sscanf(line, "a=rtpmap:%d H264/", &pt); err == nil {
				h264 = append(h264, fmt.Sprintf("a=fmtp:%d ", pt))
			}
		}
		lines := strings.Split(answerSDP, "\r\n")
		for i, line := range lines {
			for _, prefix := range h264 {
				if strings.HasPrefix(line, prefix) {
					lines[i] = line + ";" + opts.answerFmtp
				}
			}
		}
		answerSDP = strings.Join(lines, "\r\n")
		require.Contains(t, answerSDP, opts.answerFmtp)
	}
	require.NoError(t, conn.SetAnswer(answerSDP))

	t.Cleanup(func() {
		close(cam.stop)
		cam.done.Wait()
		_ = cam.pc.Close()
		_ = conn.Close()
	})
	return conn, cam
}

var (
	testSPS = []byte{0x67, 0x42, 0xe0, 0x1f, 0x8c, 0x8d, 0x40, 0x50, 0x1e, 0xd0}
	testPPS = []byte{0x68, 0xce, 0x3c, 0x80}
)

func (cam *fakeCamera) send() {
	defer cam.done.Done()

	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()

	// spropAfter and audioAfter count from the connection, not from the first
	// write: packets written before it are dropped.
	var start time.Time
	var seq uint16
	var ts uint32
	for {
		select {
		case <-cam.stop:
			return
		case <-t.C:
		}

		if start.IsZero() {
			if cam.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				continue
			}
			start = time.Now()
		}

		seq++
		ts += 900

		var payload []byte
		if cam.spropAfter >= 0 && time.Since(start) >= cam.spropAfter && seq%20 == 0 {
			// STAP-A carrying SPS + PPS, as libwebrtc bundles them ahead of an IDR
			payload = []byte{24}
			for _, nal := range [][]byte{testSPS, testPPS} {
				payload = binary.BigEndian.AppendUint16(payload, uint16(len(nal)))
				payload = append(payload, nal...)
			}
		} else {
			payload = []byte{0x41, 0x9a, 0x00, 0x01} // non-IDR slice
		}

		if time.Since(start) >= cam.videoAfter {
			_ = cam.video.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: seq, Timestamp: ts},
				Payload: payload,
			})
		}
		if cam.audio != nil && time.Since(start) >= cam.audioAfter {
			_ = cam.audio.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: seq, Timestamp: uint32(seq) * 480},
				Payload: []byte{0xfc, 0xff, 0xfe},
			})
		}
	}
}

// waitPublishable calls the Nest dial gate when the Conn has one. It is
// looked up through an interface so this file compiles against code without
// the gate, where the test then shows the race the gate removes.
func waitPublishable(t *testing.T, conn *Conn) {
	t.Helper()
	if g, ok := any(conn).(interface{ WaitNestVideo() error }); ok {
		require.NoError(t, g.WaitNestVideo())
	}
}

// Race 1: OnTrack appended sprop-parameter-sets to the published codec's
// FmtpLine, and narrowed media.Codecs, while consumers read both (MatchMedia,
// rtsp/mp4 Clone, h264.RTPDepay, Codec.MarshalJSON on /api/streams). The reads
// below are what a consumer does once Dial has returned the Conn to the streams
// layer. Audio starts late, so its track arrives after the Conn is published.
func TestRace_NestSpropFmtpLine(t *testing.T) {
	conn, _ := dialFakeCamera(t, "nest/webrtc", camOpts{audio: true, spropAfter: 300 * time.Millisecond, audioAfter: 1200 * time.Millisecond})
	waitPublishable(t, conn)
	audio := attachConsumer(t, conn, core.KindAudio, core.CodecOpus)

	// keep reading until well after the audio track starts (1.2s after connecting)
	fmtp := readVideoFmtp(conn, 2*time.Second)
	require.Contains(t, fmtp, "sprop-parameter-sets=Z0LgH4yNQFAe0A==,aM48gA==")
	require.Equal(t, 1, strings.Count(fmtp, "sprop-parameter-sets="))

	// the consumer attached before the audio track existed must still get its packets
	require.Eventually(t, func() bool { return audio.Load() > 0 }, 3*time.Second, 20*time.Millisecond,
		"late audio track was not delivered to the consumer")
}

// attachConsumer wires a consumer that accepts codecName to conn the way
// streams.AddConsumer does (MatchMedia, GetTrack, a Sender on the receiver), and
// returns its count of received payload bytes. Call it before that track starts.
func attachConsumer(t *testing.T, conn *Conn, kind, codecName string) *atomic.Int64 {
	t.Helper()
	want := &core.Media{Kind: kind, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: codecName}}}
	for _, media := range conn.GetMedias() {
		prodCodec, _ := media.MatchMedia(want)
		if prodCodec == nil {
			continue
		}
		track, err := conn.GetTrack(media, prodCodec)
		require.NoError(t, err)

		var n atomic.Int64
		sender := core.NewSender(want, prodCodec)
		sender.Handler = func(packet *rtp.Packet) { n.Add(int64(len(packet.Payload))) }
		sender.HandleRTP(track)

		// core.Receiver.Input ranges over its children without the lock that
		// AppendChild and RemoveChild take, an upstream race outside these tests. So
		// attach only before the track starts, and never detach: the sender's goroutine
		// just idles until the test binary exits. GetTransceivers takes pion's pc.mu as
		// a write lock, and the late OnTrack's getMediaCodec takes it again, which gives
		// the race detector its edge from this attach to the first packet. The edge
		// holds only if this runs before that OnTrack, which the callers' camOpts
		// delays (audioAfter, videoAfter) arrange in wall-clock time.
		_ = conn.pc.GetTransceivers()
		return &n
	}
	t.Fatalf("no %s media offers %s", kind, codecName)
	return nil
}

// readVideoFmtp reads every media's codecs as a consumer would, repeatedly for
// d, and returns the last video FmtpLine it saw.
func readVideoFmtp(conn *Conn, d time.Duration) (fmtp string) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, media := range conn.GetMedias() {
			for _, codec := range media.Codecs {
				clone := codec.Clone() // as pkg/rtsp/consumer.go
				if media.Kind == core.KindVideo {
					fmtp = clone.FmtpLine
				}
			}
		}
	}
	return
}

// Race 2: GetTrack's lookup-or-append on Receivers ran unlocked, while OnTrack
// calls it from one goroutine per remote track (and the streams layer from its own).
func TestRace_GetTrackConcurrentAppend(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	conn := NewConn(pc)
	conn.Mode = core.ModeActiveProducer
	t.Cleanup(func() { _ = conn.Close() })

	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}

	const perWorker = 50
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, _ = conn.GetTrack(media, &core.Codec{Name: core.CodecH264})
			}
		}()
	}
	wg.Wait()

	require.Len(t, conn.Receivers, 2*perWorker) // none lost to a concurrent append
}

// Race 3: the promoted core.Connection.Stop iterated Receivers while a late
// OnTrack could still be appending in GetTrack.
func TestRace_StopWhileGetTrack(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	conn := NewConn(pc)
	conn.Mode = core.ModeActiveProducer

	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	_, _ = conn.GetTrack(media, &core.Codec{Name: core.CodecH264})

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.GetTrack(media, &core.Codec{Name: core.CodecH264})
		close(started)
		for i := 0; i < 2000; i++ {
			_, _ = conn.GetTrack(media, &core.Codec{Name: core.CodecH264})
		}
	}()

	<-started // every append after this one is concurrent with Stop
	require.NoError(t, conn.Stop())
	<-done
}

// Race 4: SetAnswer assigned c.Medias after SetRemoteDescription, which starts
// the transports whose OnTrack reads c.Medias in getMediaCodec. Video only, and
// not Nest, so the read loop has no other shared write to trip over.
func TestRace_SetAnswerMediasBeforeOnTrack(t *testing.T) {
	conn, _ := dialFakeCamera(t, "webrtc", camOpts{})
	requireMediaThenClose(t, conn)
}

// Race 5: `c.Recv += n` ran in every track's read goroutine, so audio and video
// raced each other. MarshalJSON must still report the total.
func TestRace_RecvFromTwoTracks(t *testing.T) {
	conn, _ := dialFakeCamera(t, "webrtc", camOpts{audio: true})
	requireMediaThenClose(t, conn)
}

// requireMediaThenClose lets media flow, closes the Conn and checks that bytes
// were counted. It reads the counter, not MarshalJSON, because MarshalJSON also
// reads Receiver.Bytes, which core.Receiver writes per packet without a lock.
func requireMediaThenClose(t *testing.T, conn *Conn) {
	t.Helper()
	time.Sleep(time.Second)
	_ = conn.Close()
	require.Greater(t, bytesRecv(conn), 0, "no media arrived")
}

// bytesRecv reads the fixed Conn's atomic counter. Through an interface so this
// file compiles against the code before the fix, which only had the racy Recv.
func bytesRecv(conn *Conn) int {
	if r, ok := any(conn).(interface{ bytesRecv() int }); ok {
		return r.bytesRecv()
	}
	return conn.Recv
}
