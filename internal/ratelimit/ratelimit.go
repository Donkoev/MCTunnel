// Package ratelimit protects the relay's authentication from brute force and handshake floods.
package ratelimit

import (
	"net/netip"
	"sync"
	"time"
)

// Key groups addresses the way they are handed out: IPv4 per address, IPv6 per /64 (one
// subscriber normally owns a whole /64, so banning single IPv6 addresses would be pointless).
func Key(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	bits := 32
	if a.Is6() {
		bits = 64
	}
	p, err := a.Prefix(bits)
	if err != nil { // invalid address: everything unknown shares one bucket
		return netip.Prefix{}
	}
	return p
}

// maxTracked bounds memory when many different addresses fail authentication.
const maxTracked = 65536

// AuthLimiter bans an address after too many failed authentications within a window.
type AuthLimiter struct {
	maxFailures int
	window      time.Duration
	ban         time.Duration
	now         func() time.Time

	mu      sync.Mutex
	entries map[netip.Prefix]*authEntry
}

type authEntry struct {
	failures    int
	windowStart time.Time
	bannedUntil time.Time
}

// NewAuthLimiter bans for ban after maxFailures failures within window.
func NewAuthLimiter(maxFailures int, window, ban time.Duration) *AuthLimiter {
	return &AuthLimiter{
		maxFailures: maxFailures,
		window:      window,
		ban:         ban,
		now:         time.Now,
		entries:     make(map[netip.Prefix]*authEntry),
	}
}

// Banned reports whether the address is banned and for how much longer.
func (l *AuthLimiter) Banned(a netip.Addr) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[Key(a)]
	if e == nil {
		return false, 0
	}
	if left := e.bannedUntil.Sub(l.now()); left > 0 {
		return true, left
	}
	return false, 0
}

// Failure records a failed authentication and reports whether the address is now banned.
func (l *AuthLimiter) Failure(a netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	k := Key(a)
	e := l.entries[k]
	if e == nil {
		if len(l.entries) >= maxTracked {
			l.sweepLocked(now)
			if len(l.entries) >= maxTracked {
				return false // table full of live entries: fail open rather than grow without bound
			}
		}
		e = &authEntry{}
		l.entries[k] = e
	}
	if now.Sub(e.windowStart) > l.window {
		e.failures = 0
		e.windowStart = now
	}
	e.failures++
	if e.failures >= l.maxFailures {
		e.bannedUntil = now.Add(l.ban)
		e.failures = 0
		e.windowStart = now
		return true
	}
	return false
}

// Success forgets past failures of the address (an active ban is kept).
func (l *AuthLimiter) Success(a netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := Key(a)
	if e := l.entries[k]; e != nil && !e.bannedUntil.After(l.now()) {
		delete(l.entries, k)
	}
}

func (l *AuthLimiter) sweepLocked(now time.Time) {
	for k, e := range l.entries {
		if !e.bannedUntil.After(now) && now.Sub(e.windowStart) > l.window {
			delete(l.entries, k)
		}
	}
}

// Handshakes caps connections that have not authenticated yet, in total and per address, so a
// flood of slow or idle connections cannot exhaust the relay.
type Handshakes struct {
	maxTotal, maxPerKey int

	mu     sync.Mutex
	total  int
	perKey map[netip.Prefix]int
}

func NewHandshakes(maxTotal, maxPerKey int) *Handshakes {
	return &Handshakes{maxTotal: maxTotal, maxPerKey: maxPerKey, perKey: make(map[netip.Prefix]int)}
}

// Acquire takes a handshake slot; false means the connection must be dropped.
func (h *Handshakes) Acquire(a netip.Addr) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := Key(a)
	if h.total >= h.maxTotal || h.perKey[k] >= h.maxPerKey {
		return false
	}
	h.total++
	h.perKey[k]++
	return true
}

// Release returns a slot taken by Acquire.
func (h *Handshakes) Release(a netip.Addr) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := Key(a)
	h.total--
	h.perKey[k]--
	if h.perKey[k] <= 0 {
		delete(h.perKey, k)
	}
}
