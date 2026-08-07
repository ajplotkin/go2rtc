package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A WriteTo still blocked on a keyframe must unblock on Close, or the consumer leaks.
func TestWriteBufferCloseUnblocksWriteTo(t *testing.T) {
	wb := NewWriteBuffer(nil)

	returned := make(chan struct{})
	go func() {
		_, _ = wb.WriteTo(&OnceBuffer{}) // no keyframe is ever written
		close(returned)
	}()

	// WriteTo must still be blocked: no keyframe, no Close yet.
	select {
	case <-returned:
		t.Fatal("WriteTo returned before any keyframe or Close")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, wb.Close())

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("WriteTo did not unblock after Close - the consumer would leak")
	}
}

// countingWriter records whether it was ever written to. Deliberately NOT an
// io.Closer, so Close() takes the same path the HTTP consumer path takes.
type countingWriter struct{ writes int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return len(p), nil
}

// A Write arriving after Close must be rejected and must NOT reach the underlying
// writer. On the HTTP path that writer is net/http's bufio.Writer, which is recycled
// into the server pool once the handler returns — writing into it then corrupts an
// unrelated request's buffer, which is the panic behind upstream #338/#1220/#1261/#1757.
//
// Close previously set only `closed`, while Write gates solely on `err`, so this was
// reachable. Reverting the err-on-close lines in Close() makes this test fail; the two
// tests above pass either way, which is why this one exists.
func TestWriteBufferWriteAfterCloseDoesNotReachWriter(t *testing.T) {
	cw := &countingWriter{}
	wb := NewWriteBuffer(cw)

	require.NoError(t, wb.Close())

	_, err := wb.Write([]byte("late"))
	require.Error(t, err, "Write after Close must return an error")
	require.Zero(t, cw.writes, "Write after Close reached the underlying writer")
}

// Close before WriteTo registers its wait must not block WriteTo forever.
func TestWriteBufferCloseBeforeWriteTo(t *testing.T) {
	wb := NewWriteBuffer(nil)
	require.NoError(t, wb.Close())

	returned := make(chan struct{})
	go func() {
		_, _ = wb.WriteTo(&OnceBuffer{})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("WriteTo blocked after Close-before-WriteTo - the race window leaks the consumer")
	}
}
