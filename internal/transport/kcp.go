package transport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"mctunnel/server/internal/kcp"
	"mctunnel/server/internal/ratelimit"
)

func init() { register(kcpTransport{}) }

// kcpTransport carries host connections over KCP on UDP, for lossy links (Wi-Fi, mobile
// networks): a lost packet is sent again after tens of milliseconds instead of TCP's hundreds,
// so the game stutters less. It costs somewhat more bandwidth. docs/PROTOCOL.md describes it.
//
// Every host connection (control or data) is its own KCP conversation, from its own UDP socket
// on the host. KCP carries messages whose first byte says what they are: OPEN (the dialer's
// first message; the listener creates the conversation on it), DATA, FIN (half-close). A 5-byte
// datagram conv|0xFF resets a conversation the other side does not know (any more).
type kcpTransport struct{}

func (kcpTransport) Name() string { return "kcp" }

const (
	kcpMTU         = 1200 // fits paths with VPN/PPPoE overhead (QUIC uses the same floor)
	kcpWindow      = 256  // segments of up to 1176 B: at most ~300 KB in flight (delay control keeps it lower)
	kcpInterval    = 10   // ms between timer runs
	kcpResend      = 2    // fast resend after 2 duplicate ACKs
	kcpMaxChunk    = 32 << 10
	kcpMaxBuffered = 256 << 10 // received but unread; beyond that data stays in KCP's window
	kcpOpenTimeout = 10 * time.Second
	kcpMinAttempt  = 2 * time.Second  // the least one relay address gets when others remain (as net.Dialer)
	kcpIdle        = 30 * time.Second // nothing received this long: the peer is gone
	kcpKeepalive   = 10 * time.Second // no data sent this long: an empty DATA message keeps the peer (and NAT) from idling out
	kcpLinger      = 5 * time.Second  // Close waits this long for our last data to be acknowledged
	kcpRecentTTL   = time.Minute      // a finished conversation's late datagrams are not a new one
	kcpMaxPerIP    = 256              // per address, IPv6 per /64 (ratelimit.Key)
	kcpMaxTotal    = 4096
	kcpRSTInterval = time.Second // at most one RST per address this often
	kcpMaxRSTTrack = 16384       // addresses remembered for that; when full, no RST (a flood of new sources)

	kcpMsgData = 0x00
	kcpMsgOpen = 0x01
	kcpMsgFin  = 0x02
	kcpRST     = 0xFF
)

var (
	errKCPReset    = errors.New("kcp: connection reset by peer")
	errKCPDead     = errors.New("kcp: peer stopped acknowledging")
	errKCPIdle     = errors.New("kcp: nothing received for too long")
	errKCPNoReply  = errors.New("kcp: no answer (is the relay's UDP port open?)")
	errKCPWriteFin = errors.New("kcp: write after CloseWrite")
)

func kcpReset(conv uint32) []byte {
	b := make([]byte, 5)
	binary.LittleEndian.PutUint32(b, conv)
	b[4] = kcpRST
	return b
}

// kcpConn is one conversation, used as a net.Conn with half-close (CloseWrite).
type kcpConn struct {
	mu           sync.Mutex
	k            *kcp.KCP
	conv         uint32
	epoch        time.Time
	laddr, raddr net.Addr
	send         func([]byte)
	onRelease    func()

	rbuf               []byte
	rfin, wfin, closed bool
	failure            error
	released           bool
	lastRecv, closedAt time.Time
	lastSend           time.Time // the last DATA message (or keepalive) queued
	rdl, wdl           time.Time

	readable, writable, wake chan struct{}
	done                     chan struct{}
}

func newKCPConn(conv uint32, laddr, raddr net.Addr, send func([]byte), onRelease func()) *kcpConn {
	c := &kcpConn{
		conv: conv, epoch: time.Now(), laddr: laddr, raddr: raddr, send: send, onRelease: onRelease,
		lastRecv: time.Now(), lastSend: time.Now(),
		readable: make(chan struct{}, 1), writable: make(chan struct{}, 1), wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	c.k = kcp.New(conv, send)
	c.k.NoDelay(1, kcpInterval, kcpResend, true)
	c.k.WndSize(kcpWindow, kcpWindow)
	c.k.SetMtu(kcpMTU)
	c.k.DelayControl(true)
	c.k.Update(c.now())
	go c.run()
	return c
}

func (c *kcpConn) now() uint32 { return uint32(time.Since(c.epoch) / time.Millisecond) }

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// input takes one datagram of this conversation.
func (c *kcpConn) input(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return
	}
	c.lastRecv = time.Now()
	if c.failure != nil || c.k.Input(p, c.now()) < 0 {
		return
	}
	c.pull()
	c.k.Flush(c.now()) // acknowledge at once: the peer's RTT (and its resend timer) stays tight
	poke(c.readable)
	poke(c.writable)
}

// pull moves complete messages out of KCP while there is room; the rest waits in KCP's receive
// window, which then closes and makes the sender wait (backpressure).
func (c *kcpConn) pull() {
	for len(c.rbuf) < kcpMaxBuffered {
		n := c.k.PeekSize()
		if n < 0 {
			return
		}
		msg := make([]byte, n)
		c.k.Recv(msg)
		if n == 0 {
			continue
		}
		switch msg[0] {
		case kcpMsgData:
			if !c.closed {
				c.rbuf = append(c.rbuf, msg[1:]...)
			}
		case kcpMsgFin:
			c.rfin = true
		}
	}
}

// reset is the peer's RST: the conversation is gone on its side.
func (c *kcpConn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure == nil {
		c.failLocked(errKCPReset)
	}
	poke(c.wake)
}

func (c *kcpConn) failLocked(err error) {
	c.failure = err
	poke(c.readable)
	poke(c.writable)
}

// fail is for the owner of the socket (a read error).
func (c *kcpConn) fail(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failLocked(err)
	}
	c.mu.Unlock()
	poke(c.wake)
}

// run is the conversation's timer: resends, window probes, keepalives, dead-peer and linger
// checks.
func (c *kcpConn) run() {
	timer := time.NewTimer(kcpInterval * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-c.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-c.done:
			return
		}
		c.mu.Lock()
		now := c.now()
		c.k.Update(now)
		if c.failure == nil {
			if c.k.Dead() {
				c.failLocked(errKCPDead)
			} else if time.Since(c.lastRecv) > kcpIdle {
				c.failLocked(errKCPIdle)
			} else if !c.closed && !c.wfin && c.k.WaitSnd() == 0 && time.Since(c.lastSend) > kcpKeepalive {
				// KCP sends nothing on an idle conversation. An empty DATA message (receivers
				// skip it) is acknowledged: that refreshes lastRecv here, the message refreshes
				// the peer's, so an idle stream and its NAT mapping stay up.
				c.k.Send([]byte{kcpMsgData})
				c.k.Flush(now)
				c.lastSend = time.Now()
			}
		}
		release := c.closed && (c.failure != nil || c.k.WaitSnd() == 0)
		if c.closed && !release && time.Since(c.closedAt) > kcpLinger {
			c.send(kcpReset(c.conv)) // our last data could not be delivered: tell the peer
			release = true
		}
		next := c.k.Check(now) - now
		c.mu.Unlock()
		if release {
			c.release()
			return
		}
		timer.Reset(time.Duration(max(next, 1)) * time.Millisecond)
	}
}

func (c *kcpConn) release() {
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return
	}
	c.released = true
	close(c.done)
	c.mu.Unlock()
	if c.onRelease != nil {
		c.onRelease()
	}
}

// abort drops the conversation at once (listener shut down, backlog full, dial failed).
func (c *kcpConn) abort(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failLocked(err)
	}
	c.closed = true
	c.mu.Unlock()
	c.release()
}

// wait blocks until ch is poked, the deadline passes or the conversation is released.
func (c *kcpConn) wait(ch chan struct{}, deadline time.Time) error {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ch:
	case <-c.done:
	case <-timeout:
		return os.ErrDeadlineExceeded
	}
	return nil
}

func (c *kcpConn) Read(b []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.rbuf) > 0 && !c.closed {
			// As on a net.TCPConn, a read deadline in the past fails the Read even with data
			// waiting, and takes none of it. proxy.Pipe relies on that to regain control of a
			// copy whose source always has data buffered (a player downloading more slowly than
			// the host sends).
			if !c.rdl.IsZero() && !time.Now().Before(c.rdl) {
				c.mu.Unlock()
				return 0, os.ErrDeadlineExceeded
			}
			n := copy(b, c.rbuf)
			c.rbuf = c.rbuf[n:]
			if len(c.rbuf) == 0 {
				c.rbuf = nil
			}
			c.pull()
			c.mu.Unlock()
			poke(c.wake) // the receive window may have reopened: let the timer tell the peer
			return n, nil
		}
		var err error
		switch {
		case c.closed:
			err = net.ErrClosed
		case c.rfin:
			err = io.EOF
		case c.failure != nil:
			err = c.failure
		}
		deadline := c.rdl
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if err := c.wait(c.readable, deadline); err != nil {
			return 0, err
		}
	}
}

func (c *kcpConn) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		c.mu.Lock()
		var err error
		switch {
		case c.failure != nil:
			err = c.failure
		case c.closed:
			err = net.ErrClosed
		case c.wfin:
			err = errKCPWriteFin
		}
		if err != nil {
			c.mu.Unlock()
			return written, err
		}
		if c.k.WaitSnd() >= 2*kcpWindow { // the peer is behind: wait for acknowledgements
			deadline := c.wdl
			c.mu.Unlock()
			if err := c.wait(c.writable, deadline); err != nil {
				return written, err
			}
			continue
		}
		n := min(len(b), kcpMaxChunk)
		msg := make([]byte, 1+n)
		msg[0] = kcpMsgData
		copy(msg[1:], b[:n])
		c.k.Send(msg)
		c.k.Flush(c.now()) // out now, not at the next timer tick
		c.lastSend = time.Now()
		c.mu.Unlock()
		b = b[n:]
		written += n
	}
	return written, nil
}

// CloseWrite is the half-close: the peer reads EOF after our data and can still send.
func (c *kcpConn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return c.failure
	}
	if c.closed {
		return net.ErrClosed
	}
	c.finLocked()
	return nil
}

func (c *kcpConn) finLocked() {
	if !c.wfin {
		c.wfin = true
		c.k.Send([]byte{kcpMsgFin})
		c.k.Flush(c.now())
	}
}

// Close sends FIN (if not sent yet) and lets the timer release the conversation once our data
// is acknowledged, or after kcpLinger with a RST.
func (c *kcpConn) Close() error {
	c.mu.Lock()
	if c.closed || c.released {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.closedAt = time.Now()
	c.rbuf = nil
	if c.failure == nil {
		c.finLocked()
	}
	c.mu.Unlock()
	poke(c.readable)
	poke(c.writable)
	poke(c.wake)
	return nil
}

func (c *kcpConn) LocalAddr() net.Addr  { return c.laddr }
func (c *kcpConn) RemoteAddr() net.Addr { return c.raddr }

func (c *kcpConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *kcpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl = t
	c.mu.Unlock()
	poke(c.readable) // a waiting Read re-checks with the new deadline
	return nil
}

func (c *kcpConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	poke(c.writable)
	return nil
}

// ---- listener (relay) ----

type kcpKey struct {
	addr netip.AddrPort
	conv uint32
}

type kcpListener struct {
	conn      *net.UDPConn
	mu        sync.Mutex
	sessions  map[kcpKey]*kcpConn
	perIP     map[netip.Prefix]int // conversations per ratelimit.Key
	recent    map[kcpKey]time.Time // finished conversations, until their late datagrams are gone
	lastRST   map[netip.AddrPort]time.Time
	accept    chan *kcpConn
	done      chan struct{}
	closeOnce sync.Once
}

func (kcpTransport) Listen(cfg Config) (net.Listener, error) {
	addr, err := net.ResolveUDPAddr("udp", cfg.Address)
	if err != nil {
		return nil, err
	}
	uc, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	_ = uc.SetReadBuffer(4 << 20)
	_ = uc.SetWriteBuffer(4 << 20)
	l := &kcpListener{
		conn: uc, sessions: map[kcpKey]*kcpConn{}, perIP: map[netip.Prefix]int{},
		recent: map[kcpKey]time.Time{}, lastRST: map[netip.AddrPort]time.Time{},
		accept: make(chan *kcpConn, 128), done: make(chan struct{}),
	}
	go l.readLoop()
	go l.janitor()
	return l, nil
}

func (l *kcpListener) readLoop() {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := l.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			time.Sleep(time.Millisecond) // e.g. Windows reports an earlier ICMP error here: keep serving
			continue
		}
		if n < 5 || n > kcpMTU { // the peer never sends more than kcpMTU in one datagram
			continue
		}
		pkt := buf[:n] // input copies what it keeps
		key := kcpKey{netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), binary.LittleEndian.Uint32(pkt)}
		l.mu.Lock()
		c := l.sessions[key]
		l.mu.Unlock()
		switch {
		case n == 5 && pkt[4] == kcpRST:
			if c != nil {
				c.reset()
			}
		case c != nil:
			c.input(pkt)
		case isKCPOpen(pkt):
			l.open(key, pkt)
		default:
			l.maybeReset(key)
		}
	}
}

// isKCPOpen: the datagram starts with the first segment (sn 0) of a conversation, an OPEN.
func isKCPOpen(p []byte) bool {
	return len(p) > kcp.Overhead && p[4] == 81 && binary.LittleEndian.Uint32(p[12:]) == 0 &&
		binary.LittleEndian.Uint32(p[20:]) >= 1 && p[kcp.Overhead] == kcpMsgOpen
}

func (l *kcpListener) open(key kcpKey, pkt []byte) {
	ip := ratelimit.Key(key.addr.Addr())
	l.mu.Lock()
	_, finished := l.recent[key]
	if finished || len(l.sessions) >= kcpMaxTotal || l.perIP[ip] >= kcpMaxPerIP {
		l.mu.Unlock()
		return // a late copy of a finished conversation's OPEN, or too many: drop
	}
	c := newKCPConn(key.conv, l.conn.LocalAddr(), net.UDPAddrFromAddrPort(key.addr),
		func(p []byte) { _, _ = l.conn.WriteToUDPAddrPort(p, key.addr) },
		func() { l.forget(key) })
	l.sessions[key] = c
	l.perIP[ip]++
	l.mu.Unlock()
	c.input(pkt)
	select {
	case l.accept <- c:
	default:
		c.abort(errors.New("kcp: accept backlog full"))
	}
}

func (l *kcpListener) forget(key kcpKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.sessions[key]; !ok {
		return
	}
	delete(l.sessions, key)
	ip := ratelimit.Key(key.addr.Addr())
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
	l.recent[key] = time.Now().Add(kcpRecentTTL)
}

// maybeReset answers a datagram of an unknown conversation with RST, at most once a second per
// address: the host learns at once that the relay restarted or dropped the conversation. The
// table behind that limit is bounded: under a flood of new source addresses it fills up and
// they get no RST (a host whose conversation is gone then notices by its idle timeout).
func (l *kcpListener) maybeReset(key kcpKey) {
	l.mu.Lock()
	last, seen := l.lastRST[key.addr]
	if seen && time.Since(last) < kcpRSTInterval || !seen && len(l.lastRST) >= kcpMaxRSTTrack {
		l.mu.Unlock()
		return
	}
	l.lastRST[key.addr] = time.Now()
	l.mu.Unlock()
	_, _ = l.conn.WriteToUDPAddrPort(kcpReset(key.conv), key.addr)
}

func (l *kcpListener) janitor() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			l.sweep(now)
		}
	}
}

// sweep forgets finished conversations whose late datagrams are gone and RST times that no
// longer limit anything.
func (l *kcpListener) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, until := range l.recent {
		if now.After(until) {
			delete(l.recent, k)
		}
	}
	for a, at := range l.lastRST {
		if now.Sub(at) >= kcpRSTInterval {
			delete(l.lastRST, a)
		}
	}
}

func (l *kcpListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *kcpListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.mu.Lock()
		all := make([]*kcpConn, 0, len(l.sessions))
		for _, c := range l.sessions {
			all = append(all, c)
		}
		l.mu.Unlock()
		for _, c := range all {
			c.abort(net.ErrClosed)
		}
		l.conn.Close()
	})
	return nil
}

func (l *kcpListener) Addr() net.Addr { return l.conn.LocalAddr() }

// ---- dialer (host) ----

func (kcpTransport) DialContext(ctx context.Context, address string, local net.IP) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	rport, err := net.LookupPort("udp", port)
	if err != nil {
		return nil, err
	}
	candidates, err := kcpCandidates(ips, local)
	if err != nil {
		return nil, fmt.Errorf("kcp: %s: %w", host, err)
	}
	addrs := make([]netip.AddrPort, len(candidates))
	for i, ip := range candidates {
		addrs[i] = netip.AddrPortFrom(ip, uint16(rport))
	}
	return dialKCP(ctx, addrs, local)
}

// kcpCandidates orders the relay's addresses for dialing, as net.Dialer does for TCP: with a
// local address only those of its family (a socket bound to IPv4 cannot reach an IPv6 address),
// otherwise IPv4 first, as the mod's Java resolver does.
func kcpCandidates(ips []netip.Addr, local net.IP) ([]netip.Addr, error) {
	var v4, v6 []netip.Addr
	for _, ip := range ips {
		if ip = ip.Unmap(); ip.Is4() {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	if local == nil || local.IsUnspecified() { // a wildcard matches both families, as in net.Dialer
		if len(v4)+len(v6) == 0 {
			return nil, errors.New("no address")
		}
		return append(v4, v6...), nil
	}
	out, family := v6, "IPv6"
	if local.To4() != nil {
		out, family = v4, "IPv4"
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s address to dial from the local address %s", family, local)
	}
	return out, nil
}

// dialKCP tries the addresses in turn. Each gets its share of ctx's deadline (net.Dialer's rule),
// so one that does not answer (UDP blocked over IPv6, say) leaves time for the next. It returns
// the first error.
func dialKCP(ctx context.Context, addrs []netip.AddrPort, local net.IP) (net.Conn, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, kcpOpenTimeout)
		defer cancel()
	}
	var firstErr error
	for i, raddr := range addrs {
		c, err := dialKCPAddr(ctx, raddr, local, len(addrs)-i)
		if err == nil {
			return c, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// dialKCPAddr opens a conversation to one address. With others left to try it gets an equal
// share of the time left, at least kcpMinAttempt.
func dialKCPAddr(ctx context.Context, raddr netip.AddrPort, local net.IP, remaining int) (*kcpConn, error) {
	if deadline, ok := ctx.Deadline(); ok && remaining > 1 {
		left := time.Until(deadline)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, max(left/time.Duration(remaining), min(kcpMinAttempt, left)))
		defer cancel()
	}
	var laddr *net.UDPAddr
	if local != nil {
		laddr = &net.UDPAddr{IP: local}
	}
	uc, err := net.DialUDP("udp", laddr, net.UDPAddrFromAddrPort(raddr))
	if err != nil {
		return nil, err
	}
	_ = uc.SetReadBuffer(4 << 20)
	_ = uc.SetWriteBuffer(4 << 20)
	var cb [4]byte
	_, _ = rand.Read(cb[:])
	conv := binary.LittleEndian.Uint32(cb[:])
	c := newKCPConn(conv, uc.LocalAddr(), uc.RemoteAddr(), func(p []byte) { _, _ = uc.Write(p) }, func() { uc.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := uc.Read(buf)
			if err != nil {
				c.fail(err) // after release: the socket was closed on purpose, fail is a no-op then
				return
			}
			switch {
			case n == 5 && buf[4] == kcpRST && binary.LittleEndian.Uint32(buf) == conv:
				c.reset()
			case n >= kcp.Overhead && n <= kcpMTU:
				c.input(buf[:n])
			}
		}
	}()
	if err := c.openAndWait(ctx); err != nil {
		c.abort(err)
		return nil, err
	}
	return c, nil
}

// openAndWait sends OPEN and waits for its acknowledgement: one round trip, like a TCP connect,
// and it tells whether anything answers on the relay's UDP port.
func (c *kcpConn) openAndWait(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, kcpOpenTimeout)
		defer cancel()
	}
	c.mu.Lock()
	c.k.Send([]byte{kcpMsgOpen})
	c.k.Flush(c.now())
	c.mu.Unlock()
	for {
		c.mu.Lock()
		acked, failure := c.k.WaitSnd() == 0, c.failure
		c.mu.Unlock()
		if acked {
			return nil
		}
		if failure != nil {
			return failure
		}
		select {
		case <-c.writable: // input pokes it on every datagram
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errKCPNoReply
			}
			return ctx.Err()
		}
	}
}
