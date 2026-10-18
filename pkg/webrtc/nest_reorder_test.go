package webrtc

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
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

	// 101 arrives after the 300ms gap deadline. The gap must be expired
	// first, releasing 102; 101 is then behind the playout head and dropped.
	res := r.push(nestTestPacket(101), now.Add(311*time.Millisecond))

	require.True(t, res.gapExpired)
	require.Equal(t, uint16(1), res.missing)
	require.Equal(t, []uint16{102}, nestPacketSeqs(res.packets))
	require.Empty(t, r.pending)
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

	// Repair it within its own 300ms window.
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

func TestNestRTPReorderResyncAfterLargeForwardJump(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{1000},
		nestPacketSeqs(r.push(nestTestPacket(1000), now).packets),
	)

	// A +40000 jump is interpreted as behind by a signed 16-bit comparison.
	// Do not re-anchor on a single packet, but recover after a sustained run.
	start := 1001 + 40000
	var res nestRTPReorderResult

	for i := 0; i < nestRTPReorderMaxPending-1; i++ {
		res = r.push(
			nestTestPacket(uint16(start+i)),
			now.Add(time.Duration(i+1)*time.Millisecond),
		)
		require.Empty(t, res.packets)
	}

	anchor := uint16(start + nestRTPReorderMaxPending - 1)
	res = r.push(
		nestTestPacket(anchor),
		now.Add(nestRTPReorderMaxPending*time.Millisecond),
	)
	require.Equal(t, []uint16{anchor}, nestPacketSeqs(res.packets))
	require.Empty(t, r.pending)

	res = r.push(
		nestTestPacket(anchor+1),
		now.Add((nestRTPReorderMaxPending+1)*time.Millisecond),
	)
	require.Equal(t, []uint16{anchor + 1}, nestPacketSeqs(res.packets))
}

func TestNestRTPReorderResyncAfterBackwardJump(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(1, 0)

	require.Equal(t,
		[]uint16{1000},
		nestPacketSeqs(r.push(nestTestPacket(1000), now).packets),
	)

	// The sender resets from expected sequence 1001 back to 465. A short run
	// could still be delayed traffic, so only re-anchor after the threshold.
	start := 465
	var res nestRTPReorderResult

	for i := 0; i < nestRTPReorderMaxPending-1; i++ {
		res = r.push(
			nestTestPacket(uint16(start+i)),
			now.Add(time.Duration(i+1)*time.Millisecond),
		)
		require.Empty(t, res.packets)
	}

	anchor := uint16(start + nestRTPReorderMaxPending - 1)
	res = r.push(
		nestTestPacket(anchor),
		now.Add(nestRTPReorderMaxPending*time.Millisecond),
	)
	require.Equal(t, []uint16{anchor}, nestPacketSeqs(res.packets))
	require.Empty(t, r.pending)

	res = r.push(
		nestTestPacket(anchor+1),
		now.Add((nestRTPReorderMaxPending+1)*time.Millisecond),
	)
	require.Equal(t, []uint16{anchor + 1}, nestPacketSeqs(res.packets))
}

func TestNestRTPPacketsToWriteWaitsForPartitionHead(t *testing.T) {
	var partitionHead codecs.H264Packet

	// A real reorder loss is declared. Padding is ignored, and the FU-A tail
	// must not be forwarded because its partition head was lost.
	first := nestRTPPacketsToWrite(
		nestRTPReorderResult{
			gapExpired: true,
			packets: []*rtp.Packet{
				nestTestPadding(200),
				{
					Header:  rtp.Header{SequenceNumber: 201},
					Payload: []byte{0x7c, 0x05, 0xaa}, // FU-A middle/tail
				},
			},
		},
		false,
		&partitionHead,
	)

	require.Empty(t, first.packets)
	require.True(t, first.waitPartitionHead)
	require.False(t, first.recovered)

	// The wait state persists into the next push. Another orphan fragment is
	// discarded; forwarding resumes exactly at the next FU-A partition head.
	second := nestRTPPacketsToWrite(
		nestRTPReorderResult{
			packets: []*rtp.Packet{
				{
					Header:  rtp.Header{SequenceNumber: 202},
					Payload: []byte{0x7c, 0x05, 0xbb}, // still orphaned
				},
				{
					Header:  rtp.Header{SequenceNumber: 203},
					Payload: []byte{0x7c, 0x80 | 0x05, 0xcc}, // FU-A start
				},
				{
					Header:  rtp.Header{SequenceNumber: 204},
					Payload: []byte{0x7c, 0x40 | 0x05, 0xdd}, // FU-A end
				},
			},
		},
		first.waitPartitionHead,
		&partitionHead,
	)

	require.Equal(t, []uint16{203, 204}, nestPacketSeqs(second.packets))
	require.False(t, second.waitPartitionHead)
	require.True(t, second.recovered)
	require.Equal(t, uint16(203), second.recoveredSeq)
}

func TestNestRTPReorderReanchorWaitsForPartitionHead(t *testing.T) {
	var r nestRTPReorder
	var head codecs.H264Packet
	now := time.Unix(0, 0)
	wait := false
	var forwarded []*rtp.Packet

	feed := func(p *rtp.Packet) {
		w := nestRTPPacketsToWrite(r.push(p, now), wait, &head)
		wait = w.waitPartitionHead
		forwarded = append(forwarded, w.packets...)
	}

	for s := uint16(40000); s < 40010; s++ {
		feed(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: s},
			Payload: []byte{0x41, 0x01},
		})
	}

	feed(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 40010},
		Payload: []byte{0x7c, 0x80 | 0x05, 0xaa},
	})

	forwarded = nil

	for i := 0; i < nestRTPReorderMaxPending; i++ {
		now = now.Add(5 * time.Millisecond)
		feed(&rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(39000 + i),
			},
			Payload: []byte{0x7c, 0x05, 0xbb},
		})
	}

	require.Empty(t, forwarded)
}

func TestNestRTPReorderIsolatedLatePacketsNeverReanchor(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(0, 0)
	seq := uint16(1000)

	r.push(nestTestPacket(seq), now)

	for i := 0; i < 3*nestRTPReorderMaxPending; i++ {
		seq++
		now = now.Add(10 * time.Millisecond)

		res := r.push(nestTestPacket(seq), now)
		require.Equal(t, []uint16{seq}, nestPacketSeqs(res.packets))

		res = r.push(nestTestPacket(seq-50), now)
		require.Empty(t, res.packets)
	}
}

func TestNestRTPPacketsToWriteSkipsPaddingWhenHealthy(t *testing.T) {
	var head codecs.H264Packet

	res := nestRTPReorderResult{
		packets: []*rtp.Packet{
			nestTestPacket(1),
			nestTestPadding(2),
			nestTestPacket(3),
		},
	}

	w := nestRTPPacketsToWrite(res, false, &head)

	require.Equal(t, []uint16{1, 3}, nestPacketSeqs(w.packets))
}

func TestNestRTPReorderReanchorResetsBehindStreak(t *testing.T) {
	var r nestRTPReorder
	now := time.Unix(0, 0)

	r.push(nestTestPacket(40000), now)

	seq := uint16(39000)

	// Backward reset: behind the old head in signed 16-bit terms.
	var res nestRTPReorderResult

	for i := 0; i < nestRTPReorderMaxPending; i++ {
		res = r.push(nestTestPacket(seq), now)
		seq++
	}

	require.True(t, res.reanchored)

	// A stray packet behind the new head must not re-anchor again.
	res = r.push(nestTestPacket(38000), now)

	require.False(t, res.reanchored)
	require.Empty(t, res.packets)

	// In-order forwarding must continue normally.
	res = r.push(nestTestPacket(seq), now)

	require.Equal(t, []uint16{seq}, nestPacketSeqs(res.packets))
}
