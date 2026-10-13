package webrtc

import (
	"time"

	"github.com/pion/rtp"
)

// Nest retransmissions have been observed arriving roughly 60-140ms after
// newer H264 RTP packets. Wait up to 200ms for a missing sequence number:
// enough for pion's 100ms NACK cycle plus network round-trip, without adding
// latency to normal in-order traffic.
//
// Bound pending packets as well as time. Real traces have shown retransmitted
// packets arriving more than 50 sequence numbers behind the newest packet, so
// 128 leaves margin while keeping the queue strictly bounded.
const (
	nestRTPReorderMaxWait    = 200 * time.Millisecond
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
	// Therefore a retransmission arriving after the 200ms deadline cannot
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

	// Behind the playout head: either an ordinary duplicate or a
	// retransmission that arrived after its reorder window expired.
	if diff < 0 {
		return result
	}

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
