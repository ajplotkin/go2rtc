package core

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// WriteBuffer by defaul Write(s) to bytes.Buffer.
// But after WriteTo to new io.Writer - calls Reset.
// Reset will flush current buffer data to new writer and starts to Write to new io.Writer
// WriteTo will be locked until Write fails or Close will be called.
type WriteBuffer struct {
	io.Writer
	err    error
	mu     sync.Mutex
	wg     sync.WaitGroup
	state  byte
	closed bool
}

func NewWriteBuffer(wr io.Writer) *WriteBuffer {
	if wr == nil {
		wr = bytes.NewBuffer(nil)
	}
	return &WriteBuffer{Writer: wr}
}

func (w *WriteBuffer) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	if w.err != nil {
		err = w.err
	} else if n, err = w.Writer.Write(p); err != nil {
		w.err = err
		w.done()
	} else if f, ok := w.Writer.(http.Flusher); ok {
		f.Flush()
	}
	w.mu.Unlock()
	return
}

func (w *WriteBuffer) WriteTo(wr io.Writer) (n int64, err error) {
	w.Reset(wr)
	w.wg.Wait()
	return 0, w.err // TODO: fix counter
}

func (w *WriteBuffer) Close() error {
	// snapshot Writer under the lock; WriteTo/Reset can swap it concurrently
	w.mu.Lock()
	writer := w.Writer
	w.mu.Unlock()

	if closer, ok := writer.(io.Closer); ok {
		// Mark closed on this path too. Returning early left err nil, so a later Write
		// had nothing to reject on -- same hazard as below.
		//
		// done() is REQUIRED here, not optional. Upstream leaves this path alone, and it
		// got away with it because a blocked WriteTo was freed indirectly: the next Write
		// hit the now-closed connection, failed, set err and called done(). Setting err
		// here removes that escape hatch, because Write() short-circuits on err BEFORE
		// touching the writer -- so without this the WriteTo goroutine would block
		// forever. Reachable for any consumer whose WriteTo target is an io.Closer, e.g.
		// internal/rtmp passing a net.Conn. Not a path this deployment uses, but adding
		// err without done() would be strictly worse than not touching this branch.
		w.mu.Lock()
		w.closed = true
		if w.err == nil {
			w.err = io.ErrClosedPipe
		}
		w.done()
		w.mu.Unlock()
		return closer.Close()
	}
	w.mu.Lock()
	w.closed = true
	// Set err, not merely closed. Write() gates on `w.err != nil` and nothing else, so
	// before this a write arriving after Close() went straight through to
	// w.Writer.Write() -- and on the HTTP path that Writer is net/http's bufio.Writer,
	// which is RECYCLED into the server's pool once the handler returns. Writing into a
	// pooled buffer now owned by an unrelated request is the cause behind the
	// long-running panic reports upstream (#338, #1220, #1261, #1757); this mirrors
	// upstream PR #2339 (Color-Kat).
	//
	// Worth stating plainly: a panic here takes down the go2rtc process, i.e. every
	// camera at once. On this deployment that is a whole-stack outage, not one dropped
	// consumer.
	if w.err == nil {
		w.err = io.ErrClosedPipe
	}
	w.done()
	w.mu.Unlock()
	return nil
}

func (w *WriteBuffer) Reset(wr io.Writer) {
	w.mu.Lock()
	w.add()
	if buf, ok := w.Writer.(*bytes.Buffer); ok && buf.Len() != 0 {
		if _, err := io.Copy(wr, buf); err != nil {
			w.err = err
			w.done()
		}
	}
	w.Writer = wr
	w.mu.Unlock()
}

const (
	none = iota
	start
	end
)

func (w *WriteBuffer) add() {
	// don't Add if Close already ran, or WriteTo would block on a Done that already fired
	if w.state == none && !w.closed {
		w.state = start
		w.wg.Add(1)
	}
}

func (w *WriteBuffer) done() {
	if w.state == start {
		w.state = end
		w.wg.Done()
	}
}

// OnceBuffer will catch only first message
type OnceBuffer struct {
	buf []byte
}

func (o *OnceBuffer) Write(p []byte) (n int, err error) {
	if o.buf == nil {
		o.buf = p
	}
	return 0, io.EOF
}

func (o *OnceBuffer) WriteTo(w io.Writer) (n int64, err error) {
	return io.Copy(w, bytes.NewReader(o.buf))
}

func (o *OnceBuffer) Buffer() []byte {
	return o.buf
}

func (o *OnceBuffer) Len() int {
	return len(o.buf)
}
