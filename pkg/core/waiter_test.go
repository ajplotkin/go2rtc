package core

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Done must publish its error before the WaitGroup.Done that releases Wait, which
// reads w.err without the lock. In the old order (release, then store) a waiter could
// return the previous value, nil, for a connection that ended with an error.
//
// Run it with -race. The race report is what fails the old order, every time; the
// assertion only occasionally sees nil, with or without -race, because the window is
// a few instructions wide. Only an iteration in which Wait is already blocked when
// Done runs exercises the bug: if Done comes first, the mutex orders the write before
// the read and there is nothing to detect. The sleep makes the blocked case the usual
// one; it can't make the fixed code fail, since both orders return errEnded there.
func TestWaiterDoneErrorReachesWaiter(t *testing.T) {
	errEnded := errors.New("ended")

	for i := 0; i < 100; i++ {
		var w Waiter
		w.Add(1) // as a running task would, so Wait below blocks instead of auto-starting

		got := make(chan error, 1)
		go func() { got <- w.Wait() }()
		time.Sleep(5 * time.Millisecond) // let Wait block, so Done is what releases it

		w.Done(errEnded)

		select {
		case err := <-got:
			require.ErrorIs(t, err, errEnded, "iteration %d: Wait returned before Done's error was stored", i)
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after the last Done")
		}
	}
}

// A Done before anyone waits must still hand its error to a later Wait. This path
// (the else branch in Done) was not changed by the fix; the test pins it so a future
// reordering can't break it.
func TestWaiterDoneBeforeWait(t *testing.T) {
	errEnded := errors.New("ended")
	var w Waiter
	w.Done(errEnded)

	got := make(chan error, 1)
	go func() { got <- w.Wait() }()
	select {
	case err := <-got:
		require.ErrorIs(t, err, errEnded)
	case <-time.After(time.Second):
		t.Fatal("Wait blocked after a Done that came first")
	}
}
