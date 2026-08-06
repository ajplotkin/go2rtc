package webrtc

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	zlog "github.com/rs/zerolog/log"
)

// nestConnSeq gives each Nest producer OnTrack a small unique id so the stall-watchdog logs can be
// grouped per camera-session — Nest assigns SSRC 7777 to every camera, so SSRC can't distinguish them.
var nestConnSeq atomic.Int64

type Conn struct {
	core.Connection
	core.Listener

	Mode core.Mode `json:"mode"`

	pc *webrtc.PeerConnection

	offer  string
	closed core.Waiter
}

func NewConn(pc *webrtc.PeerConnection) *Conn {
	c := &Conn{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "webrtc",
			Transport:  pc,
		},
		pc: pc,
	}

	// Patched (Nest): set true once the Nest video track delivers its first RTP packet.
	// Shared between OnTrack (setter) and OnConnectionStateChange (the connect watchdog reader)
	// via closure capture. The per-track stall watchdog inside OnTrack only arms after the
	// first packet; this covers the "connected but no video RTP ever" zombie, which OnTrack
	// would never see.
	var nestVideoSeen atomic.Bool

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		// last candidate will be empty
		if candidate != nil {
			c.Fire(candidate)
		}
	})

	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		c.Fire(channel)
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state != webrtc.ICEConnectionStateChecking {
			return
		}
		pc.SCTP().Transport().ICETransport().OnSelectedCandidatePairChange(
			func(pair *webrtc.ICECandidatePair) {
				// fix situation when candidate pair changes multiple times
				if i := strings.IndexByte(c.Protocol, '+'); i > 0 {
					c.Protocol = c.Protocol[:i]
				}
				c.Protocol += "+" + pair.Remote.Protocol.String()
				c.RemoteAddr = fmt.Sprintf(
					"%s:%d %s", sanitizeIP6(pair.Remote.Address), pair.Remote.Port, pair.Remote.Typ,
				)
				if pair.Remote.RelatedAddress != "" {
					c.RemoteAddr += fmt.Sprintf(
						" %s:%d", sanitizeIP6(pair.Remote.RelatedAddress), pair.Remote.RelatedPort,
					)
				}
			},
		)
	})

	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		media, codec := c.getMediaCodec(remote)
		if media == nil {
			return
		}

		// Adopt the transmitted codec into a receiver a consumer is already on, so the
		// GetTrack below (Conn.GetTrack, pkg/webrtc/producer.go:8 -- it shadows
		// core.Connection.GetTrack) finds it by pointer identity instead of minting a second
		// receiver.
		//
		// getMediaCodec resolves `codec` from the payload type actually being transmitted, but
		// media.Codecs is narrowed to that entry only BELOW. A consumer that wired up earlier
		// matched the FIRST codec in the answer, which for Nest need not be the transmitted
		// one -- so OnTrack ends up feeding a receiver with no consumers while the consumer
		// sits on one that never receives. Observed live: receiver A childs=true bytes=0
		// profile=<nil> beside receiver B childs=false bytes=3546841 profile=Main. Audio is
		// unaffected (single Opus payload type), which is why it is so quiet: ffmpeg keeps
		// reading audio so its socket timeout never fires, and frag_keyframe cannot cut a
		// fragment without video keyframes.
		//
		// Deliberately not the reverse (moving senders onto the new receiver): that receiver is
		// absent from Producer.receivers, so stopProducers would see no attached senders and
		// stop a producer that still has live consumers.
		//
		// Gated to Nest to bound the blast radius here; the pointer-vs-value mismatch is
		// generic to any producer whose answer keeps several codecs on one media.
		if c.FormatName == "nest/webrtc" {
			for _, recv := range c.Receivers {
				if recv.Media == media && recv.Codec != codec && recv.Codec.Match(codec) {
					recv.Codec = codec
					break
				}
			}
		}

		track, err := c.GetTrack(media, codec)
		if err != nil {
			return
		}

		switch c.Mode {
		case core.ModePassiveProducer, core.ModeActiveProducer:
			// replace the theoretical list of codecs with the actual list of codecs
			if len(media.Codecs) > 1 {
				media.Codecs = []*core.Codec{codec}
			}
		}

		// Nest drought-recovery state, shared by the PLI ticker and the stall watchdog below.
		// A Nest camera stops sending keyframes (sometimes all RTP) for ~12-24s while it processes
		// its own event clip after motion; established consumers then can't decode. The camera
		// ignores in-band keyframe requests (FIR) during this window, so recovery is a close/re-dial
		// (a REPLACEMENT session, not an addition), which opens with a fresh IDR.
		//
		// isNestVideoTrack = "a Nest video track arrived" (codec-irrelevant); it only gates
		// nestVideoSeen for the connect watchdog. isNestVideo additionally requires H264 and gates the
		// drought logic (read-loop keyframe/timestamp updates + the stall watchdog), because
		// isRTPKeyframe parses H264 NAL types and would misparse a non-H264 track. Keeping the two
		// separate avoids the connect watchdog killing a healthy non-H264 stream every 30s.
		isNestVideoTrack := c.FormatName == "nest/webrtc" && remote.Kind() == webrtc.RTPCodecTypeVideo
		isNestVideo := isNestVideoTrack && codec.Name == core.CodecH264
		if isNestVideoTrack {
			nestVideoSeen.Store(true) // disarms the connect watchdog in OnConnectionStateChange
		}
		var lastVideoNS, lastIDRNS atomic.Int64

		// Patched: also request periodic keyframes for the Nest source (ModeActiveProducer,
		// FormatName "nest/webrtc"). Upstream only does this for PassiveProducer (WHIP/browser
		// push). Gating on FormatName (not the mode) avoids forcing 2s IDRs on other
		// ActiveProducer WebRTC sources (ring/tuya battery cams etc.) where it'd be harmful.
		// Keeps keyframes ~2s fresh so RTSP consumers (Homebridge live view) start fast.
		if (c.Mode == core.ModePassiveProducer || c.FormatName == "nest/webrtc") && remote.Kind() == webrtc.RTPCodecTypeVideo {
			go func() {
				ssrc := uint32(remote.SSRC())
				t := time.NewTicker(time.Second * 2)
				defer t.Stop()
				for range t.C {
					// Plain PLI. FIR escalation was removed: production traces (2026-07-22 17:12 UTC
					// drought) show Nest IGNORES FIR during its post-motion upload window — idr_age
					// climbed past 11s while FIR was sent 5 times. The only thing that ends the
					// drought is a fresh session (the re-dial in the watchdog below), so there is no
					// point spending RTCP on an in-band keyframe request the camera won't honor.
					pkts := []rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}
					if err := pc.WriteRTCP(pkts); err != nil {
						return
					}
				}
			}()
		}

		// Patched: capture SPS/PPS from the Nest H264 stream and append sprop-parameter-sets to
		// the codec FmtpLine, so RTSP consumers (Homebridge live view via ffmpeg) learn video
		// dimensions from the DESCRIBE SDP and start fast instead of waiting for an in-band
		// keyframe + probe (~3.7s -> near-instant). Google's WebRTC SDP has profile-level-id but
		// no sprop; SPS/PPS arrive in-band (usually bundled in a STAP-A). Upstream does the
		// equivalent for H265 (pkg/dvrip). Gated on FormatName like the PLI patch above.
		//
		// Note (accepted race): FmtpLine is appended from this receive goroutine without a
		// lock. In practice SPS/PPS are captured within the first few frames (the 2s PLI above
		// forces an early keyframe), before any RTSP consumer's DESCRIBE clones the codec, so
		// the write happens-before the read. A -race build could flag a consumer connecting
		// during the sub-2s warm-up window; upstream's H265 append in pkg/dvrip is unguarded
		// the same way. Codec.Match ignores FmtpLine, so this can't affect media matching.
		// Left lock-free to mirror upstream; documented rather than hidden.
		captureSprop := c.FormatName == "nest/webrtc" && codec.Name == core.CodecH264 &&
			!strings.Contains(codec.FmtpLine, "sprop-parameter-sets=")
		var spropSPS, spropPPS []byte

		// Patched: stall watchdog for the Nest video track. An expired/quiet Nest WebRTC session
		// stops delivering media WITHOUT erroring the connection, so producer.go never reconnects
		// and the stream becomes a zombie (warm but frozen). Two triggers, both -> close, which makes
		// producer.go re-dial a fresh session (a REPLACEMENT: the old pc is closed here first):
		//   - no real video RTP at all for 8s (full stall), or
		//   - real video flows but no keyframe for 4s (the post-motion upload drought).
		// A fresh session is the ONLY thing that ends a drought: Nest withholds video ~12-24s after a
		// motion event and IGNORES FIR (verified 2026-07-22 17:12 UTC: idr_age climbed past 11s while
		// FIR was sent 5x), but a re-dialed session opens with an IDR and recovers in ~2-4s. The
		// keyframe trigger is 4s (down from 8s) so that fresh IDR reaches an in-progress HKSV recording
		// well under the Apple hub's ~16-23s record deadline. Healthy video keeps a keyframe every ~2s
		// (2s PLI), so idr_age >4s means a real drought, not jitter. connID distinguishes cameras in
		// the logs — Nest assigns SSRC 7777 to every camera, so SSRC can't tell them apart.
		if isNestVideo {
			now := time.Now().UnixNano()
			lastVideoNS.Store(now)
			lastIDRNS.Store(now)
			connID := nestConnSeq.Add(1)
			zlog.Debug().Int64("conn", connID).Msg("nest: stall watchdog armed")
			stallDone := make(chan struct{})
			defer close(stallDone)
			go func() {
				t := time.NewTicker(time.Second)
				defer t.Stop()
				for {
					select {
					case <-t.C:
						videoAge := time.Since(time.Unix(0, lastVideoNS.Load()))
						idrAge := time.Since(time.Unix(0, lastIDRNS.Load()))
						if videoAge > 3*time.Second || idrAge > 3*time.Second {
							zlog.Debug().Int64("conn", connID).Dur("video_age", videoAge).Dur("idr_age", idrAge).Msg("nest: no recent video/keyframe")
						}
						// Permanently dry session: video still flowing but no keyframe for 60s. Gated on
						// videoAge because lastIDRNS <= lastVideoNS always (a keyframe IS a video
						// packet), so idrAge >= videoAge; without the videoAge guard this fast trigger
						// would shadow the full-stall branch below and turn a mere 5s network blip into
						// a teardown. "Video flowing, no keyframe" is the signature we actually want.
						if videoAge <= 8*time.Second && idrAge > 60*time.Second {
							zlog.Warn().Int64("conn", connID).Dur("video_age", videoAge).Dur("idr_age", idrAge).Msg("nest: closing stalled stream (no keyframe)")
							_ = c.Close()
							return
						}
						// Full RTP stall: no real video at all for 8s (dead session / sustained
						// outage). More tolerant than the keyframe path — this is a network event,
						// not a Nest drought, and the re-dial can't help until connectivity returns.
						if videoAge > 8*time.Second {
							zlog.Warn().Int64("conn", connID).Dur("video_age", videoAge).Dur("idr_age", idrAge).Msg("nest: closing stalled stream (no video RTP)")
							_ = c.Close()
							return
						}
					case <-stallDone:
						return
					}
				}
			}()
		}

		for {
			b := make([]byte, ReceiveMTU)
			n, _, err := remote.Read(b)
			if err != nil {
				return
			}

			c.Recv += n

			packet := &rtp.Packet{}
			if err := packet.Unmarshal(b[:n]); err != nil {
				return
			}

			if len(packet.Payload) == 0 {
				// WebRTC padding / bandwidth-probe packet: not real media. Must NOT refresh
				// lastVideoNS, or a camera that keeps probing through a drought would keep the
				// "no video RTP" watchdog from ever firing.
				continue
			}

			if isNestVideo {
				lastVideoNS.Store(time.Now().UnixNano())
				if isRTPKeyframe(packet.Payload) {
					lastIDRNS.Store(time.Now().UnixNano())
				}
			}

			if captureSprop {
				save := func(nal []byte) {
					if len(nal) == 0 {
						return
					}
					switch nal[0] & 0x1F {
					case h264.NALUTypeSPS:
						spropSPS = append([]byte(nil), nal...)
					case h264.NALUTypePPS:
						spropPPS = append([]byte(nil), nal...)
					}
				}
				if pl := packet.Payload; pl[0]&0x1F == 24 { // STAP-A: bundled NALs
					for bb := pl[1:]; len(bb) >= 2; {
						sz := int(binary.BigEndian.Uint16(bb))
						bb = bb[2:]
						if sz < 1 || sz > len(bb) {
							break
						}
						save(bb[:sz])
						bb = bb[sz:]
					}
				} else {
					save(pl)
				}
				if spropSPS != nil && spropPPS != nil {
					if codec.FmtpLine != "" {
						codec.FmtpLine += ";"
					}
					codec.FmtpLine += "sprop-parameter-sets=" +
						base64.StdEncoding.EncodeToString(spropSPS) + "," +
						base64.StdEncoding.EncodeToString(spropPPS)
					captureSprop = false
				}
			}

			track.WriteRTP(packet)
		}
	})

	// OK connection:
	// 15:01:46 ICE connection state changed: checking
	// 15:01:46 peer connection state changed: connected
	// 15:01:54 peer connection state changed: disconnected
	// 15:02:20 peer connection state changed: failed
	//
	// Fail connection:
	// 14:53:08 ICE connection state changed: checking
	// 14:53:39 peer connection state changed: failed
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		c.Fire(state)

		switch state {
		case webrtc.PeerConnectionStateConnected:
			for _, sender := range c.Senders {
				sender.Start()
			}
			// Patched (Nest): connect watchdog for the "connected but no video RTP ever" zombie.
			// OnTrack (and its per-track stall watchdog) only fire once media arrives; a session
			// that reaches connected but never delivers a video packet would otherwise sit warm
			// forever with no reconnect. If no Nest video track has been seen 30s after connect,
			// close so producer.go reconnects. Gated on FormatName so only Nest is affected.
			if c.FormatName == "nest/webrtc" {
				go func() {
					time.Sleep(30 * time.Second)
					if !nestVideoSeen.Load() {
						_ = c.Close()
					}
				}()
			}
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// disconnect event comes earlier, than failed
			// but it comes only for success connections
			_ = c.Close()
		}
	})

	return c
}

// isRTPKeyframe reports whether an RTP H264 payload begins or contains an IDR NAL.
// Handles single NAL (type 5), STAP-A (24, bundled), and FU-A (28) start fragments.
// h264.IsKeyframe expects AVCC (length-prefixed), not RTP payloads, so detect here.
func isRTPKeyframe(pl []byte) bool {
	if len(pl) == 0 {
		return false
	}
	switch pl[0] & 0x1F {
	case h264.NALUTypeIFrame: // 5: single-NAL IDR
		return true
	case 24: // STAP-A: walk bundled NALs
		for b := pl[1:]; len(b) >= 2; {
			sz := int(binary.BigEndian.Uint16(b))
			b = b[2:]
			if sz < 1 || sz > len(b) {
				break
			}
			if b[0]&0x1F == h264.NALUTypeIFrame {
				return true
			}
			b = b[sz:]
		}
	case 28: // FU-A: start fragment of an IDR
		return len(pl) >= 2 && pl[1]&0x80 != 0 && pl[1]&0x1F == h264.NALUTypeIFrame
	}
	return false
}

func (c *Conn) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Connection)
}

func (c *Conn) Close() error {
	c.closed.Done(nil)
	return c.pc.Close()
}

func (c *Conn) AddCandidate(candidate string) error {
	// pion uses only candidate value from json/object candidate struct
	return c.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate})
}

func (c *Conn) GetSenderTrack(mid string) *Track {
	if tr := c.getTranseiver(mid); tr != nil {
		if s := tr.Sender(); s != nil {
			if t := s.Track().(*Track); t != nil {
				return t
			}
		}
	}
	return nil
}

func (c *Conn) getTranseiver(mid string) *webrtc.RTPTransceiver {
	for _, tr := range c.pc.GetTransceivers() {
		if tr.Mid() == mid {
			return tr
		}
	}
	return nil
}

func (c *Conn) getMediaCodec(remote *webrtc.TrackRemote) (*core.Media, *core.Codec) {
	for _, tr := range c.pc.GetTransceivers() {
		// search Transeiver for this TrackRemote
		if tr.Receiver() == nil || tr.Receiver().Track() != remote {
			continue
		}

		// search Media for this MID
		for _, media := range c.Medias {
			if media.ID != tr.Mid() || media.Direction != core.DirectionRecvonly {
				continue
			}

			// search codec for this PayloadType
			for _, codec := range media.Codecs {
				if codec.PayloadType != uint8(remote.PayloadType()) {
					continue
				}
				return media, codec
			}
		}
	}

	// fix moment when core.ModePassiveProducer or core.ModeActiveProducer
	// sends new codec with new payload type to same media
	// check GetTrack
	panic(core.Caller())

	return nil, nil
}

func sanitizeIP6(host string) string {
	if strings.IndexByte(host, ':') > 0 {
		return "[" + host + "]"
	}
	return host
}
