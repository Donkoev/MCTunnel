// Package proxy splices two connections together.
package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// HalfCloseGrace is how long Pipe lets the other direction go without moving data once one
// direction has finished. Minecraft never keeps a half-open stream, so this only bounds peers
// that go silent after a half-close or stop reading: without it they would hold both
// connections (and a player slot) forever. A direction that keeps moving data runs on, as in
// the mod's DataBridge.
const HalfCloseGrace = 30 * time.Second

// Pipe copies a→b and b→a until both directions are finished, then closes both connections.
// It returns the number of bytes copied in each direction.
//
// A clean EOF in one direction is forwarded as a half-close (FIN) when the destination supports
// it, and the other direction keeps flowing until it ends too or has moved no data for
// HalfCloseGrace; an error in either direction tears down both. With two *net.TCPConn, io.Copy
// uses splice(2) on Linux.
func Pipe(a, b net.Conn) (aToB, bToA int64) {
	return PipeGrace(a, b, HalfCloseGrace)
}

// PipeGrace is Pipe with a custom grace period for the direction left running.
func PipeGrace(a, b net.Conn, grace time.Duration) (aToB, bToA int64) {
	// As soon as one direction ends, cleanly or not, the grace period starts for the other.
	h := &halfClose{a: a, b: b, grace: grace}
	done := make(chan int64, 1)
	go func() { // b → a
		n := h.copy(a, b)
		h.start()
		done <- n
	}()
	aToB = h.copy(b, a)
	h.start()
	bToA = <-done
	a.Close()
	b.Close()
	return aToB, bToA
}

// halfClose bounds the direction left running once the other one has finished, with deadlines
// instead of wrappers around the connections, so that io.Copy keeps using splice(2).
//
// The remaining copy gets a read deadline grace ahead. When it passes and the copy has moved
// data since the deadline was set, the copy simply goes on with a new one: a read that times
// out has taken nothing from its connection. The pipe ends when a whole deadline period went
// by without any data (so after one to two grace periods of silence), or when a write has been
// stuck for another grace period after the read deadline (a peer that stopped reading): a
// write that times out may have dropped data it had already read, so the copy cannot go on
// after one.
//
// This relies on the net.Conn rule that a Read fails once its deadline has passed, even with
// data buffered (the KCP transport follows it too). Otherwise a source that always has data
// waiting, while the destination is the slower side, would never hand control back, and the
// copy would run into the write deadline in the middle of a transfer.
type halfClose struct {
	a, b  net.Conn
	grace time.Duration

	mu      sync.Mutex
	started bool      // one direction has finished
	armed   bool      // the remaining copy has set its own deadlines
	writeBy time.Time // write deadline of the remaining copy
}

// start begins the grace period (the first call only). The read deadlines in the past stop the
// remaining copy at its next read, so that it counts its progress from its own deadline on;
// a write already underway gets one grace period to finish.
func (h *halfClose) start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return
	}
	h.started = true
	now := time.Now()
	h.writeBy = now.Add(h.grace)
	for _, c := range []net.Conn{h.a, h.b} {
		_ = c.SetReadDeadline(now)
		_ = c.SetWriteDeadline(h.writeBy)
	}
}

// resume reports whether a copy that stopped with err after moving n bytes may go on, and then
// sets its next deadlines. Only a read deadline of the grace period qualifies: the copy has
// moved data since the deadline was set, or it has just been stopped by start.
func (h *halfClose) resume(dst, src net.Conn, n int64, err error) bool {
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	switch {
	case !h.started: // a deadline the caller set
		return false
	case !now.Before(h.writeBy): // a write may have timed out
		return false
	case h.armed && n == 0: // a whole grace period without data
		return false
	}
	h.armed = true
	readBy := now.Add(h.grace)
	h.writeBy = readBy.Add(h.grace)
	_ = src.SetReadDeadline(readBy)
	_ = dst.SetWriteDeadline(h.writeBy)
	return true
}

// copy copies src to dst. At EOF it half-closes dst; after an error, or when dst cannot
// half-close, it closes both connections.
func (h *halfClose) copy(dst, src net.Conn) int64 {
	var total int64
	for {
		n, err := io.Copy(dst, src)
		total += n
		if err != nil && h.resume(dst, src, n, err) {
			continue
		}
		if err != nil {
			dst.Close()
			src.Close()
			return total
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
			return total
		}
		dst.Close()
		src.Close()
		return total
	}
}
