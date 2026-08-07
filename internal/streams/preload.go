package streams

import (
	"fmt"
	"maps"
	"net/url"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/probe"
)

type Preload struct {
	stream *Stream      // Don't include the stream in JSON to avoid leaking secrets.
	Cons   *probe.Probe `json:"consumer"`
	Query  string       `json:"query"`
}

var preloads = map[string]*Preload{}
var preloadsMu sync.Mutex

// In-flight retry goroutines, name -> cancel channel. Guarded by preloadsMu.
//
// Without this the retry loop was fire-and-forget: it exited only once a preload
// existed for the name or the stream vanished from config. Two consequences, both
// observed as possible by inspection rather than in production here, where preloads
// come only from boot config:
//   - DelPreload on a preload that had never successfully attached returned "not
//     found" and left the goroutine running, so when the camera came back it
//     re-registered the preload the operator had just deleted.
//   - Every failing AddPreload spawned another goroutine, each dialing SDM on its own
//     2-minute cadence against the same quota, until one happened to succeed.
var preloadRetries = map[string]chan struct{}{}

// Retry cadence. A variable rather than a constant purely so tests can drive
// retryPreload() itself instead of re-implementing its logic -- a test that copies the
// decision it means to check passes no matter what the real code does.
var preloadRetryInterval = 2 * time.Minute

// cancelRetryLocked stops any in-flight retry for name. preloadsMu must be held.
// The canceller closes the channel and removes the entry; the goroutine only ever
// reads from it, so there is exactly one closer and no double-close.
func cancelRetryLocked(name string) bool {
	if stop, ok := preloadRetries[name]; ok {
		close(stop)
		delete(preloadRetries, name)
		return true
	}
	return false
}

func AddPreload(name, rawQuery string) error {
	if rawQuery == "" {
		rawQuery = "video&audio"
	}

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}

	preloadsMu.Lock()
	defer preloadsMu.Unlock()

	if p := preloads[name]; p != nil {
		p.stream.RemoveConsumer(p.Cons)
		// Delete the entry too, not just its consumer. If the fresh dial below fails we
		// return early, and leaving the old entry behind means: the API reports a healthy
		// preload whose consumer is gone, AND the retry goroutine we just spawned exits on
		// its first wake because `preloads[name] != nil` -- so the background reattach this
		// whole mechanism exists for never happens. Reachable via PUT /api/preload on an
		// existing name while the camera is off.
		delete(preloads, name)
	}

	stream := Get(name)
	if stream == nil {
		return fmt.Errorf("streams: stream not found: %s", name)
	}
	cons := probe.Create("preload", query)

	if err = stream.AddConsumer(cons); err != nil {
		// The source may be temporarily unavailable (e.g. a Nest camera powered off at
		// boot returns 400). Retry in the background so the preload attaches when the
		// source comes back, instead of staying cold until a restart.
		//
		// Single-flight: only one retry goroutine per name, or repeated AddPreload calls
		// each add another dialer against the same SDM quota.
		if _, busy := preloadRetries[name]; !busy {
			stop := make(chan struct{})
			preloadRetries[name] = stop
			go retryPreload(name, rawQuery, query, stop)
		}
		return err
	}

	// Attached directly, so any retry for this name is now redundant. It would exit on
	// its own next wake, but cancelling frees it (and its timer) immediately.
	cancelRetryLocked(name)

	preloads[name] = &Preload{stream: stream, Cons: cons, Query: rawQuery}
	return nil
}

func retryPreload(name, rawQuery string, query url.Values, stop chan struct{}) {
	// Deregister on every exit path so a later AddPreload can start a fresh retry --
	// but only if the map still points at OUR channel, since a canceller may already
	// have removed us and a newer goroutine taken the slot.
	defer func() {
		preloadsMu.Lock()
		if preloadRetries[name] == stop {
			delete(preloadRetries, name)
		}
		preloadsMu.Unlock()
	}()

	for {
		// Gentler than 1 min: each attempt is one GenerateWebRtcStream (SDM executeCommand,
		// counts against the 100 QPH/camera and shared 10 QPM/project quotas). 2 min keeps a
		// recovered camera warming reasonably fast without hammering quota while it stays off.
		//
		// Waited on alongside stop so DelPreload takes effect immediately rather than up to
		// two minutes later, and so a cancelled retry cannot dial once more on its way out.
		select {
		case <-stop:
			return
		case <-time.After(preloadRetryInterval):
		}

		// Decide whether we still need to retry, then release the lock BEFORE the network dial
		// so a 429 backoff inside AddConsumer can't hold preloadsMu (blocking config reload /
		// consumer attach for this stream for the whole backoff).
		preloadsMu.Lock()
		if preloads[name] != nil { // a successful AddPreload (or newer retry) already ran
			preloadsMu.Unlock()
			return
		}
		stream := Get(name)
		preloadsMu.Unlock()
		if stream == nil { // stream removed from config
			return
		}

		cons := probe.Create("preload", query)
		if err := stream.AddConsumer(cons); err != nil {
			continue // still unavailable; try again next cycle
		}

		// Dial succeeded. Two reasons not to register, both checked under the lock:
		//
		//  1. We were CANCELLED while dialing. The select above only covers cancellation
		//     that arrives while waiting; a dial takes seconds (much longer behind a 429
		//     backoff), and DelPreload during that window deletes preloads[name] — so the
		//     `preloads[name] == nil` test alone reads as "free to register" and puts back
		//     exactly the preload the operator just deleted. That resurrection is the bug
		//     this whole cancel mechanism exists to prevent, so testing only the nil was
		//     not enough.
		//  2. A concurrent AddPreload or a newer retry won while we were dialing.
		//
		// preloadRetries[name] == stop is the single test for "still ours, still wanted":
		// a canceller deletes the entry, and a replacement overwrites it, so anything other
		// than our own channel means do not register.
		preloadsMu.Lock()
		if preloadRetries[name] == stop && preloads[name] == nil {
			preloads[name] = &Preload{stream: stream, Cons: cons, Query: rawQuery}
			preloadsMu.Unlock()
		} else {
			preloadsMu.Unlock()
			stream.RemoveConsumer(cons)
		}
		return
	}
}

func DelPreload(name string) error {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()

	// Cancel an in-flight retry as well as removing any attached preload. A preload that
	// has never successfully attached exists ONLY as a retry goroutine, so without this
	// DelPreload reported "not found" and left it running -- and it would re-register the
	// preload the operator had just deleted as soon as the source came back.
	cancelled := cancelRetryLocked(name)

	if p := preloads[name]; p != nil {
		p.stream.RemoveConsumer(p.Cons)
		delete(preloads, name)
		return nil
	}

	// Cancelling a pending retry is a real deletion, not a miss.
	if cancelled {
		return nil
	}

	return fmt.Errorf("streams: preload not found: %s", name)
}

func GetPreloads() map[string]*Preload {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()
	return maps.Clone(preloads)
}
