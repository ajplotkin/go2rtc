package streams

import (
	"testing"
)

// DelPreload must cancel a pending retry, and a retry that was cancelled MID-DIAL must
// not register afterwards. The mid-dial case is the one that matters: the select in
// retryPreload only covers cancellation while waiting, so a naive `preloads[name] == nil`
// check at registration time reads as "free to register" and puts back the preload the
// operator just deleted.
//
// This exercises the decision directly rather than spinning a real 2-minute retry.
func TestPreloadRetryCancelledMidDialDoesNotRegister(t *testing.T) {
	preloadsMu.Lock()
	preloads = map[string]*Preload{}
	preloadRetries = map[string]chan struct{}{}
	stop := make(chan struct{})
	preloadRetries["cam"] = stop
	preloadsMu.Unlock()

	// Operator deletes the preload while our goroutine is mid-dial.
	preloadsMu.Lock()
	cancelled := cancelRetryLocked("cam")
	preloadsMu.Unlock()
	if !cancelled {
		t.Fatal("cancelRetryLocked should have reported cancelling a pending retry")
	}

	// The dial now succeeds. This is the exact registration guard from retryPreload.
	preloadsMu.Lock()
	mayRegister := preloadRetries["cam"] == stop && preloads["cam"] == nil
	preloadsMu.Unlock()

	if mayRegister {
		t.Fatal("a retry cancelled mid-dial would re-register the deleted preload")
	}

	// And with no cancellation, it must still be allowed to register.
	preloadsMu.Lock()
	stop2 := make(chan struct{})
	preloadRetries["cam2"] = stop2
	ok := preloadRetries["cam2"] == stop2 && preloads["cam2"] == nil
	preloadsMu.Unlock()
	if !ok {
		t.Fatal("an uncancelled retry must still be able to register")
	}
}

// Cancelling twice must not panic on a double close.
func TestCancelRetryTwiceDoesNotDoubleClose(t *testing.T) {
	preloadsMu.Lock()
	preloadRetries = map[string]chan struct{}{}
	preloadRetries["cam"] = make(chan struct{})
	first := cancelRetryLocked("cam")
	second := cancelRetryLocked("cam")
	preloadsMu.Unlock()
	if !first || second {
		t.Fatalf("expected first=true second=false, got first=%v second=%v", first, second)
	}
}
