package webrtc

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func nestTestPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: seq,
		},
		Payload: []byte{0x01},
	}
}

func nestTestPadding(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: seq,
		},
	}
}

func nestPacketSeqs(packets []*rtp.Packet) []uint16 {
	seqs := make([]uint16, len(packets))
	for i, packet := range packets {
		seqs[i] = packet.SequenceNumber
	}
	return seqs
}

func TestNestRTPReorderInOrder(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	res := r.push(nestTestPacket(100), now)
	require.Equal(t, []uint16{100}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)

	res = r.push(nestTestPacket(101), now.Add(time.Millisecond))
	require.Equal(t, []uint16{101}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)
}

func TestNestRTPReorderDelayedBurst(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{100},
		nestPacketSeqs(r.push(nestTestPacket(100), now).packets),
	)

	require.Empty(t,
		r.push(nestTestPacket(103), now.Add(10*time.Millisecond)).packets,
	)
	require.Empty(t,
		r.push(nestTestPacket(104), now.Add(20*time.Millisecond)).packets,
	)

	res := r.push(nestTestPacket(101), now.Add(80*time.Millisecond))
	require.Equal(t, []uint16{101}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)

	res = r.push(nestTestPacket(102), now.Add(100*time.Millisecond))
	require.Equal(t, []uint16{102, 103, 104}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)
}

func TestNestRTPReorderDuplicate(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{100},
		nestPacketSeqs(r.push(nestTestPacket(100), now).packets),
	)

	require.Empty(t,
		r.push(nestTestPacket(102), now.Add(10*time.Millisecond)).packets,
	)

	// Duplicate while the packet is still buffered.
	require.Empty(t,
		r.push(nestTestPacket(102), now.Add(20*time.Millisecond)).packets,
	)

	res := r.push(nestTestPacket(101), now.Add(30*time.Millisecond))
	require.Equal(t, []uint16{101, 102}, nestPacketSeqs(res.packets))

	// Duplicate after the packet was already emitted.
	require.Empty(t,
		r.push(nestTestPacket(101), now.Add(40*time.Millisecond)).packets,
	)
}

func TestNestRTPReorderWraparound(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	res := r.push(nestTestPacket(65534), now)
	require.Equal(t, []uint16{65534}, nestPacketSeqs(res.packets))

	require.Empty(t,
		r.push(nestTestPacket(0), now.Add(10*time.Millisecond)).packets,
	)

	res = r.push(nestTestPacket(65535), now.Add(20*time.Millisecond))
	require.Equal(t, []uint16{65535, 0}, nestPacketSeqs(res.packets))

	res = r.push(nestTestPacket(1), now.Add(30*time.Millisecond))
	require.Equal(t, []uint16{1}, nestPacketSeqs(res.packets))
}

func TestNestRTPReorderPaddingFillsGap(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{100},
		nestPacketSeqs(r.push(nestTestPacket(100), now).packets),
	)

	require.Empty(t,
		r.push(nestTestPacket(102), now.Add(10*time.Millisecond)).packets,
	)

	// Padding has no media payload but still consumes sequence 101.
	res := r.push(nestTestPadding(101), now.Add(20*time.Millisecond))
	require.Equal(t, []uint16{101, 102}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)
}

func TestNestRTPReorderLatePacketAfterTimeout(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{100},
		nestPacketSeqs(r.push(nestTestPacket(100), now).packets),
	)

	require.Empty(t,
		r.push(nestTestPacket(102), now.Add(10*time.Millisecond)).packets,
	)

	// 101 arrives after the 200ms gap deadline. The gap must be expired
	// first, releasing 102; 101 is then behind the playout head and dropped.
	res := r.push(nestTestPacket(101), now.Add(211*time.Millisecond))

	require.True(t, res.gapExpired)
	require.Equal(t, uint16(1), res.missing)
	require.Equal(t, []uint16{102}, nestPacketSeqs(res.packets))
}

func TestNestRTPReorderNewGapGetsFullWindow(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{100},
		nestPacketSeqs(r.push(nestTestPacket(100), now).packets),
	)

	// First gap: missing 101.
	require.Empty(t,
		r.push(nestTestPacket(102), now.Add(10*time.Millisecond)).packets,
	)

	// A second gap already exists behind 104: missing 103.
	require.Empty(t,
		r.push(nestTestPacket(104), now.Add(100*time.Millisecond)).packets,
	)

	// Repair the first gap at 150ms. This emits 101,102 and starts a fresh
	// timer for the newly exposed gap at 103.
	res := r.push(nestTestPacket(101), now.Add(150*time.Millisecond))
	require.Equal(t, []uint16{101, 102}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)

	// Only 100ms has elapsed for the second gap, so it must NOT expire.
	res = r.push(nestTestPacket(105), now.Add(250*time.Millisecond))
	require.Empty(t, res.packets)
	require.False(t, res.gapExpired)

	// Repair it within its own 200ms window.
	res = r.push(nestTestPacket(103), now.Add(300*time.Millisecond))
	require.Equal(t, []uint16{103, 104, 105}, nestPacketSeqs(res.packets))
	require.False(t, res.gapExpired)
}

func TestNestRTPReorderPendingBound(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{100},
		nestPacketSeqs(r.push(nestTestPacket(100), now).packets),
	)

	// Leave sequence 101 missing and fill the queue with 128 packets.
	var res nestRTPReorderResult
	for i := 0; i < nestRTPReorderMaxPending; i++ {
		res = r.push(
			nestTestPacket(uint16(102+i)),
			now.Add(time.Duration(i+1)*time.Millisecond),
		)
	}

	require.True(t, res.gapExpired)
	require.Equal(t, uint16(1), res.missing)
	require.Len(t, res.packets, nestRTPReorderMaxPending)
	require.Equal(t, uint16(102), res.packets[0].SequenceNumber)
	require.Equal(t, uint16(229), res.packets[len(res.packets)-1].SequenceNumber)
}
