package webrtc

import (
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Nest retransmissions are commonly delayed by tens to hundreds of
// milliseconds. Multi-camera diagnostics observed p99 around 277ms, so wait
// up to 300ms for a missing sequence number. The wait applies only while a
// sequence gap is open and adds no latency to normal in-order traffic.
//
// Bound pending packets as well as time. Real traces have shown retransmitted
// packets arriving more than 50 sequence numbers behind the newest packet, so
// 128 leaves margin while keeping the queue strictly bounded.
const (
	nestRTPReorderMaxWait    = 300 * time.Millisecond
	nestRTPReorderMaxPending = 128
)

type nestRTPReorderResult struct {
	packets    []*rtp.Packet
	gapExpired bool
	missing    uint16
}

type nestRTPReorder struct {
	started bool
	next    uint16

	pending  map[uint16]*rtp.Packet
	gapSince time.Time

	// A sustained run of packets that all appear behind the playout head can
	// indicate that the sender reset its RTP sequence number while keeping the
	// same SSRC. A single late packet must not disturb a healthy stream.
	behindHead int
}

// push accepts RTP packets in arrival order and returns packets that are safe
// to emit in sequence order.
//
// A sequence gap starts a bounded wait. If the missing packet arrives within
// the window, buffered packets are released in order. If the window expires,
// the missing sequence numbers are abandoned and any later retransmission of
// them is dropped as late.
//
// Sequence comparisons deliberately use signed 16-bit differences so RTP
// wraparound (65535 -> 0) is handled naturally.
func (r *nestRTPReorder) push(packet *rtp.Packet, now time.Time) nestRTPReorderResult {
	var result nestRTPReorderResult

	if !r.started {
		r.started = true
		r.next = packet.SequenceNumber
		r.pending = make(map[uint16]*rtp.Packet)
	}

	// Expire an existing gap BEFORE considering the newly arrived packet.
	// Therefore a retransmission arriving after the 300ms deadline cannot
	// retroactively heal a gap whose wait window has already elapsed.
	if !r.gapSince.IsZero() && now.Sub(r.gapSince) >= nestRTPReorderMaxWait {
		packets, missing := r.expireGap(now)
		result.packets = append(result.packets, packets...)
		if missing != 0 {
			result.gapExpired = true
			result.missing += missing
		}
	}

	diff := int16(packet.SequenceNumber - r.next)

	// Behind the playout head: normally an ordinary duplicate or a
	// retransmission that arrived after its reorder window expired.
	//
	// Signed 16-bit RTP sequence comparisons are intentionally ambiguous for
	// jumps of half the sequence space or more. If a sustained run of packets
	// all appears behind the head, assume the sender reset its sequence number
	// while retaining the same SSRC and re-anchor on the current packet.
	if diff < 0 {
		r.behindHead++
		if r.behindHead < nestRTPReorderMaxPending {
			return result
		}

		r.next = packet.SequenceNumber
		r.pending = make(map[uint16]*rtp.Packet)
		r.gapSince = time.Time{}
		r.behindHead = 0

		r.pending[packet.SequenceNumber] = packet
		result.packets = append(result.packets, r.drain(now)...)
		return result
	}

	// Any packet at or ahead of the current head breaks a behind-head streak.
	r.behindHead = 0

	// A packet already waiting in the queue is a duplicate retransmission.
	if _, ok := r.pending[packet.SequenceNumber]; ok {
		return result
	}

	// Payload contents intentionally do not matter here. In particular,
	// zero-payload padding packets still occupy RTP sequence numbers and must
	// fill their slot even though the caller won't forward their payload.
	r.pending[packet.SequenceNumber] = packet
	result.packets = append(result.packets, r.drain(now)...)

	// If the time window hasn't fired but the queue reaches its hard bound,
	// abandon the current gap rather than allowing unbounded growth.
	if len(r.pending) >= nestRTPReorderMaxPending {
		packets, missing := r.expireGap(now)
		result.packets = append(result.packets, packets...)
		if missing != 0 {
			result.gapExpired = true
			result.missing += missing
		}
	}

	return result
}

type nestRTPWriteResult struct {
	packets           []*rtp.Packet
	waitPartitionHead bool
	recovered         bool
	recoveredSeq      uint16
}

// nestRTPPacketsToWrite applies the post-reorder H264 recovery gate.
//
// Once a reorder gap has been abandoned, packets are suppressed until a
// partition head arrives so an orphan FU-A/FU-B tail is never forwarded.
// Padding occupies sequence-number space but is never written downstream.
func nestRTPPacketsToWrite(
	result nestRTPReorderResult,
	waitPartitionHead bool,
	partitionHead *codecs.H264Packet,
) nestRTPWriteResult {
	write := nestRTPWriteResult{
		waitPartitionHead: waitPartitionHead || result.gapExpired,
	}

	for _, packet := range result.packets {
		if len(packet.Payload) == 0 {
			continue
		}

		if write.waitPartitionHead {
			if !partitionHead.IsPartitionHead(packet.Payload) {
				continue
			}

			write.waitPartitionHead = false
			write.recovered = true
			write.recoveredSeq = packet.SequenceNumber
		}

		write.packets = append(write.packets, packet)
	}

	return write
}

// drain emits every contiguous packet starting at the current playout head.
// If draining one repaired gap exposes another, the new gap receives its own
// full wait window.
func (r *nestRTPReorder) drain(now time.Time) []*rtp.Packet {
	startNext := r.next
	var packets []*rtp.Packet

	for {
		packet, ok := r.pending[r.next]
		if !ok {
			break
		}

		delete(r.pending, r.next)
		packets = append(packets, packet)
		r.next++
	}

	switch {
	case len(r.pending) == 0:
		r.gapSince = time.Time{}

	case r.next != startNext:
		// We advanced through one repaired gap and stopped at a new one.
		r.gapSince = now

	case r.gapSince.IsZero():
		// First packet observed beyond a newly opened gap.
		r.gapSince = now
	}

	return packets
}

// expireGap abandons the missing sequence numbers up to the nearest buffered
// packet ahead of the current playout head, then drains from that point.
func (r *nestRTPReorder) expireGap(now time.Time) ([]*rtp.Packet, uint16) {
	if len(r.pending) == 0 {
		r.gapSince = time.Time{}
		return nil, 0
	}

	var (
		bestSeq      uint16
		bestDistance int
		found        bool
	)

	for seq := range r.pending {
		diff := int16(seq - r.next)
		if diff <= 0 {
			continue
		}

		distance := int(diff)
		if !found || distance < bestDistance {
			bestSeq = seq
			bestDistance = distance
			found = true
		}
	}

	if !found {
		return nil, 0
	}

	missing := uint16(bestDistance)

	r.next = bestSeq
	r.gapSince = time.Time{}

	return r.drain(now), missing
}
