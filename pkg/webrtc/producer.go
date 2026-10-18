package webrtc

import (
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/webrtc/v4"
)

func (c *Conn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	core.Assert(media.Direction == core.DirectionRecvonly)

	// OnTrack calls this from one goroutine per remote track, and the streams layer from
	// its own; unlocked, two calls could both miss below and one append would be lost.
	c.tracksMu.Lock()
	defer c.tracksMu.Unlock()

	for _, track := range c.Receivers {
		if track.Codec == codec {
			return track, nil
		}
	}

	switch c.Mode {
	case core.ModePassiveConsumer: // backchannel from browser
		// set codec for consumer recv track so remote peer should send media with this codec
		params := webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  MimeType(codec),
				ClockRate: codec.ClockRate,
				Channels:  uint16(codec.Channels),
			},
			PayloadType: 0, // don't know if this necessary
		}

		tr := c.getTranseiver(media.ID)

		_ = tr.SetCodecPreferences([]webrtc.RTPCodecParameters{params})

	case core.ModePassiveProducer, core.ModeActiveProducer:
		// Passive producers: OBS Studio via WHIP or Browser
		// Active producers: go2rtc as WebRTC client or WebTorrent

	default:
		panic(core.Caller())
	}

	track := core.NewReceiver(media, codec)
	c.Receivers = append(c.Receivers, track)
	return track, nil
}

func (c *Conn) Start() error {
	c.closed.Wait()
	return nil
}

// Stop shadows the promoted core.Connection.Stop, which the streams layer and
// nest.WebRTCClient.Stop call. That one ranges over Receivers while a late OnTrack may
// still be appending in GetTrack, so take a snapshot under the lock. The closes run
// outside it, so none of them can block while holding it.
func (c *Conn) Stop() error {
	c.tracksMu.Lock()
	receivers := append([]*core.Receiver(nil), c.Receivers...)
	c.tracksMu.Unlock()

	for _, receiver := range receivers {
		receiver.Close()
	}
	for _, sender := range c.Senders {
		sender.Close()
	}
	return c.pc.Close()
}
