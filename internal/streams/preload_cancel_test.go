package streams

import "testing"

// COVERAGE, STATED HONESTLY.
//
// Only one behaviour of the preload cancel mechanism is pinned by a test here: the
// close-once ownership rule. That test is mutation-verified — making cancelRetryLocked
// stop deleting the entry makes it panic on a double close.
//
// The other two properties of the mechanism — that a retry cancelled MID-DIAL does not
// register (the resurrection guard at preload.go, `preloadRetries[name] == stop`), and
// that retryPreload deregisters itself on exit — are verified by review, NOT by test.
//
// This is deliberate, and worth writing down because the first two attempts got it wrong:
//   - Attempt 1 re-implemented the guard inline in the test and asserted on its own copy.
//     Reverting the real guard changed nothing; it passed either way.
//   - Attempt 2 called retryPreload() for real, but with no stream registered the function
//     returns at `Get(name) == nil` before it ever reaches the dial or the guard. Reverting
//     the guard, and even deleting the whole select-on-stop, still passed.
//
// Reaching that code needs a Stream whose AddConsumer succeeds on demand, so cancellation
// can be timed into the window between the dial and the registration. That is a real test
// worth writing; it is not written yet. Until it is, do not claim those paths are covered.
func TestCancelRetryTwiceDoesNotDoubleClose(t *testing.T) {
	preloadsMu.Lock()
	defer preloadsMu.Unlock()

	preloadRetries = map[string]chan struct{}{}
	preloadRetries["cam"] = make(chan struct{})

	// The canceller closes AND removes; a second cancel must find nothing. If it did not
	// remove, this would close an already-closed channel and panic.
	if first := cancelRetryLocked("cam"); !first {
		t.Fatal("first cancel should report that it cancelled a pending retry")
	}
	if second := cancelRetryLocked("cam"); second {
		t.Fatal("second cancel should report nothing to cancel")
	}
}
