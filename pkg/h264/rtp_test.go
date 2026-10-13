package h264

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func testDepay() (core.HandlerFunc, *[][]byte) {
	got := &[][]byte{}

	handler := RTPDepay(
		&core.Codec{Name: core.CodecH264},
		func(packet *rtp.Packet) {
			payload := append([]byte(nil), packet.Payload...)
			*got = append(*got, payload)
		},
	)

	return handler, got
}

func TestRTPDepayRestartsOnNewFUAStart(t *testing.T) {
	handler, got := testDepay()

	// Start a fragmented IDR, but never finish it.
	handler(&rtp.Packet{
		Header: rtp.Header{SequenceNumber: 100},
		Payload: []byte{
			0x7c,        // FU-A, NRI=3
			0x80 | 0x05, // start, IDR
			0xaa,
		},
	})

	// A new FU-A start means the previous fragmented NAL was abandoned.
	handler(&rtp.Packet{
		Header: rtp.Header{SequenceNumber: 101},
		Payload: []byte{
			0x7c,
			0x80 | 0x05,
			0xbb,
		},
	})

	// Finish the second IDR.
	handler(&rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 102,
			Marker:         true,
		},
		Payload: []byte{
			0x7c,
			0x40 | 0x05, // end, IDR
			0xcc,
		},
	})

	require.Len(t, *got, 1)

	// AVCC: length=3, NAL header=0x65, then only the SECOND FU payload.
	require.Equal(t,
		[]byte{0x00, 0x00, 0x00, 0x03, 0x65, 0xbb, 0xcc},
		(*got)[0],
	)
}

func TestRTPDepayDropsOrphanFUAFragmentsAfterSync(t *testing.T) {
	handler, got := testDepay()

	// Establish normal synchronized operation with a complete P-frame.
	handler(&rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 100,
			Marker:         true,
		},
		Payload: []byte{0x41, 0x11, 0x22},
	})

	require.Len(t, *got, 1)

	// Middle fragment with no FU-A start.
	handler(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 101},
		Payload: []byte{0x7c, 0x05, 0xaa},
	})

	// End fragment still has no valid start.
	handler(&rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 102,
			Marker:         true,
		},
		Payload: []byte{0x7c, 0x40 | 0x05, 0xbb},
	})

	// No corrupt synthetic IDR should have been emitted.
	require.Len(t, *got, 1)
}

func TestRTPDepayAbandonsFragmentOnStandaloneNAL(t *testing.T) {
	handler, got := testDepay()

	// Begin a fragmented IDR.
	handler(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 100},
		Payload: []byte{0x7c, 0x80 | 0x05, 0xaa},
	})

	// Instead of an FU-A continuation, a standalone P-frame arrives.
	// The stale fragmented state must be discarded.
	handler(&rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 101,
			Marker:         true,
		},
		Payload: []byte{0x41, 0x33, 0x44},
	})

	require.Len(t, *got, 1)
	require.Equal(t,
		[]byte{0x00, 0x00, 0x00, 0x03, 0x41, 0x33, 0x44},
		(*got)[0],
	)
}
