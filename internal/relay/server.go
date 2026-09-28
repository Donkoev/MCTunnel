// Package relay is the VPS side of MCTunnel. It authenticates hosts, gives every tunnel an
// external port, and splices each player connection to a data connection opened by the host.
package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"mctunnel/server/internal/protocol"
	"mctunnel/server/internal/proxy"
	"mctunnel/server/internal/ratelimit"
)

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("relay: server closed")

// User is a host allowed to open tunnels.
type User struct {
	Name   string
	Secret []byte
	Port   int // fixed external port; 0 = any free port
	// Disabled users cannot log in, but keep their fixed or home port (HomePorts): switching a
	// user off does not move any other user's address.
	Disabled bool
}

// Options configure a Server. Zero values get defaults.
type Options struct {
	PublicBind        string // IP for the players' external ports; "" = all interfaces
	PortMin, PortMax  int
	MaxHosts          int
	MaxPlayersPerHost int
	// Concurrent players of one tunnel from one source address (IPv6: one /64), so a single
	// client cannot fill the tunnel; default max(4, MaxPlayersPerHost/4).
	MaxPlayersPerIP   int
	MaxTunnelsPerUser int

	HandshakeTimeout time.Duration
	OpenTimeout      time.Duration
	IdleTimeout      time.Duration // no frame from the host for this long = dead tunnel
	WriteTimeout     time.Duration
	// How long a player connection may keep one direction open without moving data after the
	// other has ended (see proxy.HalfCloseGrace).
	HalfCloseGrace time.Duration

	MaxHandshakes      int
	MaxHandshakesPerIP int
	AuthMaxFailures    int
	AuthWindow         time.Duration
	AuthBan            time.Duration

	Logger *slog.Logger
}

// Protocol timing: hosts ping every 10 s, so 30 s of silence means the tunnel is dead.
const (
	DefaultIdleTimeout  = 30 * time.Second
	DefaultWriteTimeout = 10 * time.Second
)

// preambleTimeout bounds the wait for the 6-byte preamble over TCP and TLS (at most
// HandshakeTimeout). Hosts send it right after connecting, so a longer silence only holds a
// handshake slot. Over KCP the clock starts when the OPEN datagram arrives, while hosts send
// the preamble only after the OPEN is acknowledged, and a lost copy is resent only after the
// retransmission timeout has grown (about 3 RTT, then 1.5 times that): kcpPreambleTimeout leaves
// room for that on a slow link. Every listener has its own handshake budget, so the longer wait
// cannot starve TCP and TLS.
const (
	preambleTimeout    = 3 * time.Second
	kcpPreambleTimeout = 6 * time.Second
)

func (o Options) withDefaults() Options {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	if o.PortMin <= 0 || o.PortMax <= 0 {
		o.PortMin, o.PortMax = 25600, 25699
	}
	def(&o.MaxHosts, 16)
	def(&o.MaxPlayersPerHost, 32)
	def(&o.MaxPlayersPerIP, max(4, o.MaxPlayersPerHost/4))
	def(&o.MaxTunnelsPerUser, 1)
	defDur(&o.HandshakeTimeout, 10*time.Second)
	defDur(&o.OpenTimeout, 10*time.Second)
	defDur(&o.IdleTimeout, DefaultIdleTimeout)
	defDur(&o.WriteTimeout, DefaultWriteTimeout)
	defDur(&o.HalfCloseGrace, proxy.HalfCloseGrace)
	def(&o.MaxHandshakes, 256)
	def(&o.MaxHandshakesPerIP, 32)
	def(&o.AuthMaxFailures, 5)
	defDur(&o.AuthWindow, 10*time.Minute)
	defDur(&o.AuthBan, 15*time.Minute)
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o
}

// Server is the relay. Create it with New, feed it listeners with Serve.
type Server struct {
	opts Options
	log  *slog.Logger
	auth *ratelimit.AuthLimiter
	// bannedLog throttles the lines about an address that is already banned ("rejected banned
	// address", failed data connections): a banned client may reconnect in a loop.
	bannedLog logThrottle[netip.Prefix]
	// udpAuthLog throttles authentication failures over UDP (KCP), which never lead to a ban.
	udpAuthLog logThrottle[netip.Prefix]
	// dummyKey authenticates unknown user names, so they take as long to reject as wrong secrets.
	dummyKey []byte

	users atomic.Pointer[map[string]*User] // the users that may log in (not disabled)

	mu            sync.Mutex
	closed        bool
	listeners     map[net.Listener]struct{}
	handshaking   map[net.Conn]struct{}
	sessions      map[*session]struct{}
	byUser        map[string][]*session // oldest first
	pending       map[uint64]*pendingConn
	lastPort      map[string]int   // sticky ports: a returning user gets its previous port if free
	homePort      map[string]int   // users without a fixed port → their home port (HomePorts)
	configured    map[string]*User // every user of the list, disabled ones too (for the ports)
	ports         portAllocator
	nextSessionID uint64
	nextConnID    uint64

	wg sync.WaitGroup
}

// New creates a relay for the given users.
func New(opts Options, users []User) *Server {
	opts = opts.withDefaults()
	s := &Server{
		opts:        opts,
		log:         opts.Logger,
		auth:        ratelimit.NewAuthLimiter(opts.AuthMaxFailures, opts.AuthWindow, opts.AuthBan),
		udpAuthLog:  logThrottle[netip.Prefix]{limit: maxUDPAuthLogSources},
		dummyKey:    make([]byte, 32),
		listeners:   make(map[net.Listener]struct{}),
		handshaking: make(map[net.Conn]struct{}),
		sessions:    make(map[*session]struct{}),
		byUser:      make(map[string][]*session),
		pending:     make(map[uint64]*pendingConn),
		lastPort:    make(map[string]int),
		ports: portAllocator{
			bind: opts.PublicBind, min: opts.PortMin, max: opts.PortMax,
			used: make(map[int]bool), reserved: make(map[int]string),
		},
	}
	_, _ = rand.Read(s.dummyKey)
	s.SetUsers(users)
	return s
}

// SetUsers replaces the user list (config reload). Tunnels of users that were removed, disabled
// or got a new secret are closed with CodeRevoked; all other tunnels are untouched. The home
// ports (HomePorts) are worked out again for the new list; running tunnels keep their ports.
// Disabled users keep their fixed and home ports, so switching one off or on again moves no
// other user's address.
func (s *Server) SetUsers(users []User) {
	all := make(map[string]*User, len(users))
	for _, u := range users {
		u := u
		u.Secret = bytes.Clone(u.Secret)
		all[u.Name] = &u
	}
	m := make(map[string]*User, len(all))
	reserved := make(map[int]string)
	for name, u := range all {
		if !u.Disabled {
			m[name] = u
		}
		if u.Port != 0 {
			reserved[u.Port] = name
		}
	}
	home := homePorts(all, s.opts.PortMin, s.opts.PortMax)
	s.users.Store(&m)

	s.mu.Lock()
	s.ports.reserved = reserved
	s.homePort = home
	s.configured = all
	var revoked []*session
	for sess := range s.sessions {
		if nu := m[sess.user.Name]; nu == nil || !bytes.Equal(nu.Secret, sess.user.Secret) {
			revoked = append(revoked, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range revoked {
		sess.close(protocol.CodeRevoked)
	}
}

// Serve accepts host connections from ln until Shutdown. Call it once per listener/transport.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return ErrServerClosed
	}
	s.listeners[ln] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	// Every listener has its own handshake budget: a flood on one transport (a spoofed KCP OPEN
	// costs a single datagram) cannot starve the others.
	hs := ratelimit.NewHandshakes(s.opts.MaxHandshakes, s.opts.MaxHandshakesPerIP)
	// A UDP (KCP) source address is not verified before Accept and can be forged.
	unverified := ln.Addr().Network() == "udp"

	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return ErrServerClosed
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			delay = nextBackoff(delay)
			s.log.Warn("accept failed", "err", err, "retry_in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		s.wg.Add(1)
		go s.handleConn(c, hs, unverified)
	}
}

// Shutdown stops accepting connections, closes every tunnel with CodeShutdown and waits for all
// goroutines to finish (or for ctx to expire).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	var closers []io.Closer
	for ln := range s.listeners {
		closers = append(closers, ln)
	}
	for c := range s.handshaking {
		closers = append(closers, c)
	}
	sessions := make([]*session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

	for _, c := range closers {
		c.Close()
	}
	for _, sess := range sessions {
		sess.close(protocol.CodeShutdown)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats is a snapshot of the relay's load.
type Stats struct {
	Tunnels int
	Pending int // players waiting for the host's data connection
	Players int // pending + connected players over all tunnels
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	st := Stats{Tunnels: len(s.sessions), Pending: len(s.pending)}
	sessions := make([]*session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		st.Players += sess.playerCount()
	}
	return st
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// handleConn runs the handshake of a new host connection and dispatches on its kind. hs is the
// handshake budget of the listener c came from; unverified means its source address may be forged.
func (s *Server) handleConn(c net.Conn, hs *ratelimit.Handshakes, unverified bool) {
	defer s.wg.Done()
	start := time.Now()
	ip := remoteIP(c)
	if !hs.Acquire(ip) {
		s.log.Debug("too many pending handshakes, dropping connection", "remote", c.RemoteAddr().String())
		c.Close()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		hs.Release(ip)
		c.Close()
		return
	}
	s.handshaking[c] = struct{}{}
	s.mu.Unlock()

	// release ends the handshake phase: the slot is freed and the connection is no longer ours to
	// close on shutdown (a session or a pipe owns it from here on).
	var once sync.Once
	release := func() {
		once.Do(func() {
			hs.Release(ip)
			s.mu.Lock()
			delete(s.handshaking, c)
			s.mu.Unlock()
		})
	}
	defer release()

	wait := preambleTimeout
	if unverified { // the UDP (KCP) listener
		wait = kcpPreambleTimeout
	}
	_ = c.SetDeadline(start.Add(min(wait, s.opts.HandshakeTimeout)))
	kind, err := protocol.ReadPreamble(c)
	if err != nil {
		s.log.Debug("rejected connection", "remote", c.RemoteAddr().String(), "err", err)
		if errors.Is(err, protocol.ErrUnsupportedVersion) {
			reject(c, protocol.CodeUnsupportedVersion)
		} else {
			c.Close()
		}
		return
	}
	_ = c.SetDeadline(start.Add(s.opts.HandshakeTimeout))
	switch kind {
	case protocol.KindControl:
		s.handleControl(c, ip, release, unverified)
	case protocol.KindData:
		s.handleData(c, ip, release, unverified)
	}
}

// handleControl authenticates a host and runs its tunnel until it ends.
func (s *Server) handleControl(c net.Conn, ip netip.Addr, release func(), unverified bool) {
	log := s.log.With("remote", c.RemoteAddr().String())
	if banned, left := s.auth.Banned(ip); banned {
		if ok, suppressed := s.bannedLog.allow(ratelimit.Key(ip)); ok {
			log.Warn("rejected banned address", "ban_left", left.Round(time.Second).String(), "suppressed", suppressed)
		}
		reject(c, protocol.CodeBanned)
		return
	}

	br := bufio.NewReaderSize(c, 512)
	hello, err := expect[*protocol.Hello](br)
	if err != nil {
		s.handshakeFailed(c, log, err)
		return
	}
	serverNonce := protocol.NewNonce()
	if err := protocol.WriteMessage(c, &protocol.Challenge{ServerNonce: serverNonce}); err != nil {
		c.Close()
		return
	}
	auth, err := expect[*protocol.Auth](br)
	if err != nil {
		s.handshakeFailed(c, log, err)
		return
	}

	user := (*s.users.Load())[hello.User]
	key := s.dummyKey
	if user != nil {
		key = user.Secret
	}
	want := protocol.ControlMAC(key, serverNonce, hello.ClientNonce, hello.User)
	if ok := protocol.EqualMAC(want, auth.MAC); !ok || user == nil {
		s.authFailure(log, ip, unverified, "authentication failed", "user", hello.User)
		reject(c, protocol.CodeAuthFailed)
		return
	}
	s.auth.Success(ip)
	log = log.With("user", user.Name)

	sess, code := s.openSession(user, c, br, log)
	if code != protocol.CodeOK {
		log.Warn("tunnel refused", "reason", code.String())
		reject(c, code)
		return
	}
	_ = c.SetDeadline(time.Time{})
	release()

	proof := protocol.ServerProof(user.Secret, serverNonce, hello.ClientNonce, hello.User)
	if err := sess.send(&protocol.Welcome{ExternalPort: uint16(sess.port), ServerProof: proof}); err != nil {
		sess.close(protocol.CodeOK)
		return
	}
	log.Info("tunnel opened", "port", sess.port)
	sess.run()
}

// maxUDPAuthLogSources bounds the addresses udpAuthLog tracks, and so its lines per logInterval:
// a spoofer can send every attempt from a new address.
const maxUDPAuthLogSources = 64

// authFailure logs msg with args for a failed authentication from ip and counts it towards a ban
// of ip. Failures from an unverified (UDP) source are not counted: anyone can forge that address,
// so counting them would let a spoofer lock any host out (brute force gains nothing, since
// secrets have at least 32 random characters). No ban ends those attempts, so their lines are
// throttled per address instead.
//
// Over TCP and TLS every failure is logged until the address is banned, AuthMaxFailures per
// AuthWindow at most. A ban refuses control connections before they authenticate, but not
// data connections (that would also cut off the players of a legitimate host behind the same
// address), so a banned address can go on failing there: those lines go through bannedLog.
func (s *Server) authFailure(log *slog.Logger, ip netip.Addr, unverified bool, msg string, args ...any) {
	if unverified {
		if ok, suppressed := s.udpAuthLog.allow(ratelimit.Key(ip)); ok {
			log.Warn(msg, append(args, "suppressed", suppressed)...)
		}
		return
	}
	if banned, _ := s.auth.Banned(ip); banned {
		s.auth.Failure(ip) // still counted: the ban is renewed while they go on
		if ok, suppressed := s.bannedLog.allow(ratelimit.Key(ip)); ok {
			log.Warn(msg, append(args, "now_banned", true, "suppressed", suppressed)...)
		}
		return
	}
	log.Warn(msg, append(args, "now_banned", s.auth.Failure(ip))...)
}

func (s *Server) handshakeFailed(c net.Conn, log *slog.Logger, err error) {
	log.Debug("handshake failed", "err", err)
	if errors.Is(err, protocol.ErrProtocol) {
		reject(c, protocol.CodeProtocolError)
		return
	}
	c.Close()
}

// openSession registers an authenticated tunnel and binds its external port.
func (s *Server) openSession(u *User, c net.Conn, br *bufio.Reader, log *slog.Logger) (*session, protocol.Code) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, protocol.CodeShutdown
	}
	// u was looked up before s.mu was taken, so a reload may have revoked it since. SetUsers
	// stores the new map before it takes s.mu: either this sees the new map, or SetUsers sees
	// the new session and closes it.
	cur := (*s.users.Load())[u.Name]
	if cur == nil || !bytes.Equal(cur.Secret, u.Secret) {
		s.mu.Unlock()
		return nil, protocol.CodeRevoked
	}
	u = cur // also picks up a fixed port changed by that reload
	// Over the per-user limit, the user's oldest tunnels are replaced. Usually that is a stale
	// tunnel left behind by a network drop: the reconnecting host gets its port back at once
	// instead of waiting for the keepalive timeout.
	list := s.byUser[u.Name]
	replace := len(list) - s.opts.MaxTunnelsPerUser + 1
	if replace < 0 {
		replace = 0
	}
	if len(s.sessions)-replace >= s.opts.MaxHosts {
		s.mu.Unlock()
		return nil, protocol.CodeServerFull
	}
	replaced := append([]*session(nil), list[:replace]...)
	for _, old := range replaced {
		s.detachLocked(old)
		old.ln.Close() // release the port right now so the new tunnel can take it over
	}

	preferred, avoid := s.portChoiceLocked(u)
	ln, port, err := s.ports.listen(u.Name, preferred, avoid)
	var sess *session
	if err == nil {
		s.nextSessionID++
		sess = newSession(s, s.nextSessionID, u, c, br, ln, port, log)
		s.sessions[sess] = struct{}{}
		s.byUser[u.Name] = append(s.byUser[u.Name], sess)
	}
	s.mu.Unlock()

	for _, old := range replaced {
		old.log.Info("tunnel replaced by a new login of the same user")
		old.close(protocol.CodeReplaced)
	}
	if err != nil {
		log.Error("cannot bind an external port", "err", err)
		return nil, protocol.CodeNoFreePort
	}
	return sess, protocol.CodeOK
}

// portChoiceLocked returns the ports to try first for u and the ports a random pick should
// avoid. A user with a fixed port wants that one. Any other user wants its home port
// (HomePorts): it follows from the user list alone, so the address survives a relay restart
// and does not depend on which host connects first. The port of the user's previous tunnel
// comes first when it is not another user's home port: it differs from the home port only when
// that one was busy, and the friends last saw that address. Random picks avoid the home and
// previous ports of the other users, disabled ones included, so they do not take those users'
// addresses.
func (s *Server) portChoiceLocked(u *User) (preferred []int, avoid map[int]bool) {
	avoid = make(map[int]bool)
	othersHome := make(map[int]bool)
	for name, o := range s.configured {
		if name == u.Name || o.Port != 0 {
			continue // fixed ports are reserved by the allocator anyway
		}
		if p, ok := s.homePort[name]; ok {
			avoid[p] = true
			othersHome[p] = true
		}
		if p := s.lastPort[name]; p != 0 {
			avoid[p] = true
		}
	}
	if u.Port != 0 {
		return []int{u.Port}, avoid
	}
	home, hasHome := s.homePort[u.Name]
	if p := s.lastPort[u.Name]; p != 0 && p != home && !othersHome[p] {
		preferred = append(preferred, p)
	}
	if hasHome {
		preferred = append(preferred, home)
	}
	return preferred, avoid
}

// detachLocked removes a session from the registries and frees its port. It is idempotent.
func (s *Server) detachLocked(sess *session) {
	if _, ok := s.sessions[sess]; !ok {
		return
	}
	delete(s.sessions, sess)
	list := s.byUser[sess.user.Name]
	for i, x := range list {
		if x == sess {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.byUser, sess.user.Name)
	} else {
		s.byUser[sess.user.Name] = list
	}
	s.lastPort[sess.user.Name] = sess.port
	s.ports.release(sess.port)
}

// addPending registers a player waiting for its data connection and arms the open timeout.
func (s *Server) addPending(p *pendingConn) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, alive := s.sessions[p.sess]; !alive || s.closed {
		return 0, false
	}
	s.nextConnID++
	id := s.nextConnID
	s.pending[id] = p
	p.timer = time.AfterFunc(s.opts.OpenTimeout, func() { s.expirePending(id) })
	return id, true
}

func (s *Server) expirePending(id uint64) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	p.sess.log.Info("player dropped: the host did not open a data connection in time", "player", p.player.RemoteAddr().String())
	p.player.Close()
	p.release()
}

// handleData authenticates a data connection and splices it to its waiting player.
func (s *Server) handleData(c net.Conn, ip netip.Addr, release func(), unverified bool) {
	hdr, err := protocol.ReadDataHeader(c)
	if err != nil {
		c.Close()
		return
	}
	s.mu.Lock()
	p := s.pending[hdr.ConnID]
	s.mu.Unlock()
	if p == nil {
		s.log.Debug("data connection for an unknown or expired conn id", "remote", c.RemoteAddr().String(), "conn", hdr.ConnID)
		rejectData(c, protocol.CodeUnknownConn)
		return
	}
	// Verify before claiming: a forged header must not be able to cancel someone's pending player.
	want := protocol.DataMAC(p.sess.user.Secret, hdr.ConnID, p.nonce)
	if !protocol.EqualMAC(want, hdr.MAC) {
		s.authFailure(s.log, ip, unverified, "data connection with a bad MAC", "remote", c.RemoteAddr().String())
		rejectData(c, protocol.CodeAuthFailed)
		return
	}
	s.mu.Lock()
	if s.pending[hdr.ConnID] != p { // expired or claimed in the meantime
		s.mu.Unlock()
		rejectData(c, protocol.CodeUnknownConn)
		return
	}
	delete(s.pending, hdr.ConnID)
	p.timer.Stop()
	s.mu.Unlock()
	release()

	// Status and the player's first bytes go out in one write: accepting costs no extra round trip.
	out := make([]byte, 0, 1+len(p.initial))
	out = append(out, byte(protocol.CodeOK))
	out = append(out, p.initial...)
	_ = c.SetDeadline(time.Now().Add(s.opts.WriteTimeout))
	if _, err := c.Write(out); err != nil {
		c.Close()
		p.player.Close()
		p.release()
		return
	}
	_ = c.SetDeadline(time.Time{})

	sess := p.sess
	if !sess.track(p.player, c) {
		c.Close()
		p.player.Close()
		p.release()
		return
	}
	player := p.player.RemoteAddr().String()
	sess.log.Info("player connected", "player", player, "conn", hdr.ConnID)
	start := time.Now()
	up, down := pipeConns(p.player, c, s.opts.HalfCloseGrace)
	sess.untrack(p.player, c)
	p.release()
	sess.log.Info("player disconnected", "player", player, "conn", hdr.ConnID,
		"duration", time.Since(start).Round(time.Second).String(), "bytes_up", up, "bytes_down", down)
}

func expect[T protocol.Message](r io.Reader) (T, error) {
	var zero T
	m, err := protocol.ReadMessage(r)
	if err != nil {
		return zero, err
	}
	t, ok := m.(T)
	if !ok {
		return zero, fmt.Errorf("%w: unexpected frame 0x%02x", protocol.ErrProtocol, m.Type())
	}
	return t, nil
}

// reject sends a CLOSE frame and closes the connection gracefully.
func reject(c net.Conn, code protocol.Code) {
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_ = protocol.WriteMessage(c, &protocol.Close{Code: code})
	lingerClose(c)
}

// rejectData answers a data connection with an error status and closes it gracefully.
func rejectData(c net.Conn, code protocol.Code) {
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte{byte(code)})
	lingerClose(c)
}

// lingerClose sends FIN after our last bytes and drains what the peer already sent before
// closing. Closing a socket with unread input makes the kernel answer with a RST, and a RST can
// destroy our last frame before the peer reads it: the host would see "connection reset"
// instead of the reason. Bounded to 1 s and 64 KiB.
func lingerClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = io.Copy(io.Discard, io.LimitReader(c, 64<<10))
	c.Close()
}

func remoteIP(c net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// logInterval is the minimum time between two log lines about the same kind of rejected
// connection, so a connection flood cannot flood the log.
const logInterval = 10 * time.Second

// maxThrottled bounds the number of keys a logThrottle remembers.
const maxThrottled = 4096

// logThrottle lets through at most one log line per key every logInterval and counts the lines
// it held back in between. The zero value is ready to use.
type logThrottle[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*throttled
	limit   int // keys remembered at most; 0 = maxThrottled
}

type throttled struct {
	next       time.Time // no line for this key before then
	suppressed int
}

// allow reports whether a line for key may be written now and, if so, how many lines for key
// were suppressed since the previous one.
func (t *logThrottle[K]) allow(key K) (ok bool, suppressed int) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.entries[key]; e != nil {
		if now.Before(e.next) {
			e.suppressed++
			return false, 0
		}
		suppressed = e.suppressed
		e.next, e.suppressed = now.Add(logInterval), 0
		return true, suppressed
	}
	if t.entries == nil {
		t.entries = make(map[K]*throttled)
	}
	limit := t.limit
	if limit <= 0 {
		limit = maxThrottled
	}
	if len(t.entries) >= limit {
		for k, e := range t.entries {
			if !now.Before(e.next) {
				delete(t.entries, k)
			}
		}
		if len(t.entries) >= limit {
			return false, 0 // too many sources at once: stay quiet rather than grow
		}
	}
	t.entries[key] = &throttled{next: now.Add(logInterval)}
	return true, 0
}

func nextBackoff(d time.Duration) time.Duration {
	if d == 0 {
		return 5 * time.Millisecond
	}
	if d *= 2; d > time.Second {
		d = time.Second
	}
	return d
}
