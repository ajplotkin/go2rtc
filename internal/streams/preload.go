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
		go retryPreload(name, rawQuery, query)
		return err
	}

	preloads[name] = &Preload{stream: stream, Cons: cons, Query: rawQuery}
	return nil
}

func retryPreload(name, rawQuery string, query url.Values) {
	for {
		// Gentler than 1 min: each attempt is one GenerateWebRtcStream (SDM executeCommand,
		// counts against the 100 QPH/camera and shared 10 QPM/project quotas). 2 min keeps a
		// recovered camera warming reasonably fast without hammering quota while it stays off.
		time.Sleep(2 * time.Minute)

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

		// Dial succeeded — register under lock, but a concurrent AddPreload/retry may have won
		// while we were dialing; if so, drop our extra consumer instead of leaking it.
		preloadsMu.Lock()
		if preloads[name] == nil {
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

	if p := preloads[name]; p != nil {
		p.stream.RemoveConsumer(p.Cons)
		delete(preloads, name)
		return nil
	}

	return fmt.Errorf("streams: preload not found: %s", name)
}

func GetPreloads() map[string]*Preload {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()
	return maps.Clone(preloads)
}
