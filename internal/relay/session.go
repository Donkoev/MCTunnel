package relay

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"mctunnel/server/internal/protocol"
	"mctunnel/server/internal/proxy"
	"mctunnel/server/internal/ratelimit"
	"mctunnel/server/internal/transport"
)

// initialReadSize bounds the player's first read (Handshake + Login Start fit easily).
const initialReadSize = 4096

// maxFirstBytesWait caps the wait for a player's first bytes (normally OpenTimeout). A vanilla
// client sends its Handshake right after connecting, so a longer wait only lets idle sockets
// hold player slots.
const maxFirstBytesWait = 10 * time.Second

// playerUserTimeout is TCP_USER_TIMEOUT for player sockets (Linux): a player whose data stays
// unacknowledged this long, e.g. one that stopped reading and advertises a zero window, is
// dropped instead of stalling its data connection forever.
const playerUserTimeout = 60 * time.Second

// session is one authenticated tunnel: a control connection plus the listener on its external port.
type session struct {
	srv  *Server
	id   uint64
	user *User
	conn net.Conn
	br   *bufio.Reader
	ln   *net.TCPListener
	port int
	log  *slog.Logger

	writeMu   sync.Mutex // serializes frames on the control connection
	closeOnce sync.Once

	mu      sync.Mutex
	closed  bool
	players int                   // pending + connected
	perAddr map[netip.Prefix]int  // players by source (ratelimit.Key)
	active  map[net.Conn]struct{} // both ends of every spliced player connection

	rejectLog logThrottle[string] // refused players, by reason
}

// pendingConn is a player waiting for the host to answer OPEN.
type pendingConn struct {
	sess    *session
	player  net.Conn
	initial []byte // what the player sent before OPEN (its Handshake)
	nonce   protocol.Nonce
	timer   *time.Timer
	release func() // frees the player slot; idempotent
}

func newSession(srv *Server, id uint64, u *User, c net.Conn, br *bufio.Reader, ln *net.TCPListener, port int, log *slog.Logger) *session {
	return &session{
		srv: srv, id: id, user: u, conn: c, br: br, ln: ln, port: port,
		log:     log.With("port", port),
		perAddr: make(map[netip.Prefix]int),
		active:  make(map[net.Conn]struct{}),
	}
}

// run serves the tunnel until the control connection ends.
func (sess *session) run() {
	sess.srv.wg.Add(1)
	go sess.acceptPlayers()
	sess.readLoop()
}

func (sess *session) readLoop() {
	for {
		_ = sess.conn.SetReadDeadline(time.Now().Add(sess.srv.opts.IdleTimeout))
		msg, err := protocol.ReadMessage(sess.br)
		if err != nil {
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout():
				sess.log.Warn("host stopped answering (keepalive timeout)")
				sess.close(protocol.CodeTimeout)
			case errors.Is(err, protocol.ErrProtocol):
				sess.log.Warn("protocol violation by host", "err", err)
				sess.close(protocol.CodeProtocolError)
			default:
				sess.close(protocol.CodeOK) // host went away
			}
			return
		}
		switch m := msg.(type) {
		case *protocol.Ping:
			if err := sess.send(&protocol.Pong{Payload: m.Payload}); err != nil {
				sess.close(protocol.CodeOK)
				return
			}
		case *protocol.Pong:
			// The relay does not ping; tolerate unsolicited pongs.
		case *protocol.Close:
			sess.log.Info("host closed the tunnel")
			sess.close(protocol.CodeOK)
			return
		default:
			sess.log.Warn("unexpected frame from host", "type", msg.Type())
			sess.close(protocol.CodeProtocolError)
			return
		}
	}
}

// send writes one control frame.
func (sess *session) send(m protocol.Message) error {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	_ = sess.conn.SetWriteDeadline(time.Now().Add(sess.srv.opts.WriteTimeout))
	return protocol.WriteMessage(sess.conn, m)
}

// close ends the tunnel: frees the port, drops pending players and disconnects everyone.
// If code is not CodeOK, it is sent to the host first (best effort).
func (sess *session) close(code protocol.Code) {
	sess.closeOnce.Do(func() {
		s := sess.srv
		if code != protocol.CodeOK && sess.writeMu.TryLock() {
			_ = sess.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = protocol.WriteMessage(sess.conn, &protocol.Close{Code: code})
			sess.writeMu.Unlock()
			// FIN now, full close a bit later: closing with unread pings in the buffer would send
			// a RST that can wipe the CLOSE frame before the host reads the reason.
			if cw, ok := sess.conn.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
				_ = sess.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				time.AfterFunc(2*time.Second, func() { sess.conn.Close() })
			} else {
				sess.conn.Close()
			}
		} else {
			sess.conn.Close()
		}
		sess.ln.Close()

		s.mu.Lock()
		s.detachLocked(sess)
		var pending []*pendingConn
		for id, p := range s.pending {
			if p.sess == sess {
				delete(s.pending, id)
				p.timer.Stop()
				pending = append(pending, p)
			}
		}
		s.mu.Unlock()
		for _, p := range pending {
			p.player.Close()
			p.release()
		}

		sess.mu.Lock()
		sess.closed = true
		active := sess.active
		sess.active = nil
		sess.mu.Unlock()
		for c := range active {
			c.Close()
		}
		sess.log.Info("tunnel closed", "reason", code.String())
	})
}

func (sess *session) acceptPlayers() {
	defer sess.srv.wg.Done()
	var delay time.Duration
	for {
		c, err := sess.ln.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = nextBackoff(delay) // e.g. out of file descriptors: back off, keep the tunnel
			sess.log.Warn("accept failed", "err", err, "retry_in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		transport.TuneTCP(c)
		proxy.SetUserTimeout(c, playerUserTimeout)
		key := ratelimit.Key(remoteIP(c))
		if ok, perAddr := sess.acquireSlot(key); !ok {
			sess.logRejected(c, perAddr)
			c.Close()
			continue
		}
		sess.srv.wg.Add(1)
		go sess.handlePlayer(c, key)
	}
}

// logRejected logs a refused player, at most once per logInterval for each reason: a client
// reconnecting in a loop would otherwise write a line per connection.
func (sess *session) logRejected(c net.Conn, perAddr bool) {
	msg, limit := "player rejected: tunnel is full", sess.srv.opts.MaxPlayersPerHost
	if perAddr {
		msg, limit = "player rejected: too many connections from one address", sess.srv.opts.MaxPlayersPerIP
	}
	if ok, suppressed := sess.rejectLog.allow(msg); ok {
		sess.log.Info(msg, "player", c.RemoteAddr().String(), "max", limit, "suppressed", suppressed)
	}
}

// handlePlayer waits for the player's first bytes, then asks the host for a data connection.
// key is the player's source (ratelimit.Key), whose slot is released when the player leaves.
func (sess *session) handlePlayer(c net.Conn, key netip.Prefix) {
	defer sess.srv.wg.Done()
	release := sync.OnceFunc(func() { sess.releaseSlot(key) })

	// A Minecraft client always speaks first (Handshake), so waiting for its first bytes costs
	// nothing, while port scanners that merely connect never make the host dial back.
	buf := make([]byte, initialReadSize)
	_ = c.SetReadDeadline(time.Now().Add(min(sess.srv.opts.OpenTimeout, maxFirstBytesWait)))
	n, err := c.Read(buf)
	if n == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			sess.log.Debug("player sent nothing", "player", c.RemoteAddr().String(), "err", err)
		}
		c.Close()
		release()
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	p := &pendingConn{sess: sess, player: c, initial: buf[:n], nonce: protocol.NewNonce(), release: release}
	id, ok := sess.srv.addPending(p)
	if !ok {
		c.Close()
		release()
		return
	}
	if err := sess.send(&protocol.Open{ConnID: id, Nonce: p.nonce}); err != nil {
		sess.close(protocol.CodeOK) // control connection is broken; close cleans up the pending player
	}
}

// acquireSlot takes a player slot for a player from key. perAddr reports a refusal because of
// the per-address limit rather than a full (or closing) tunnel.
func (sess *session) acquireSlot(key netip.Prefix) (ok, perAddr bool) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed || sess.players >= sess.srv.opts.MaxPlayersPerHost {
		return false, false
	}
	if sess.perAddr[key] >= sess.srv.opts.MaxPlayersPerIP {
		return false, true
	}
	sess.players++
	sess.perAddr[key]++
	return true, false
}

func (sess *session) releaseSlot(key netip.Prefix) {
	sess.mu.Lock()
	sess.players--
	if sess.perAddr[key]--; sess.perAddr[key] <= 0 {
		delete(sess.perAddr, key)
	}
	sess.mu.Unlock()
}

func (sess *session) playerCount() int {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.players
}

// track registers a spliced pair so that closing the tunnel disconnects it.
func (sess *session) track(a, b net.Conn) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return false
	}
	sess.active[a] = struct{}{}
	sess.active[b] = struct{}{}
	return true
}

func (sess *session) untrack(a, b net.Conn) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	delete(sess.active, a)
	delete(sess.active, b)
}

// pipeConns splices a player and its data connection; returns bytes player→host and host→player.
func pipeConns(player, data net.Conn, grace time.Duration) (up, down int64) {
	return proxy.PipeGrace(player, data, grace)
}
