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

// closerWriter is an io.Writer that is ALSO an io.Closer, which sends Close() down the
// early-return branch — the path internal/rtmp uses by handing WriteTo a net.Conn.
type closerWriter struct{ closed bool }

func (c *closerWriter) Write(p []byte) (int, error) { return len(p), nil }
func (c *closerWriter) Close() error                { c.closed = true; return nil }

// A WriteTo blocked on an io.Closer target must still unblock on Close.
//
// This branch previously returned before calling done(). Upstream gets away with that
// because the next Write hits the dead connection, fails, sets err and calls done() --
// but once Close sets err itself, Write short-circuits before touching the writer and
// that escape hatch disappears, stranding the WriteTo goroutine forever. Removing the
// done() from the Closer branch makes this test hang and fail.
func TestWriteBufferCloseUnblocksWriteToOnCloserTarget(t *testing.T) {
	wb := NewWriteBuffer(nil)
	target := &closerWriter{} // WriteTo swaps this in as w.Writer; Close() then sees a Closer

	returned := make(chan struct{})
	go func() {
		_, _ = wb.WriteTo(target) // no keyframe is ever written
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("WriteTo returned before any keyframe or Close")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, wb.Close())

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("WriteTo did not unblock after Close on an io.Closer target - goroutine leaked")
	}
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
