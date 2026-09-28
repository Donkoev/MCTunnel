// Package transport abstracts how host connections (control and data) travel between the host
// and the relay: plain TCP, TLS (for port 443 in restrictive networks) and KCP over UDP (for
// lossy links). The relay and host logic only ever see net.Listener and net.Conn, so they do not
// change when a transport is added.
//
// Players never use this package: they are vanilla Minecraft clients and always speak TCP to
// the external ports.
package transport

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"
)

// Config selects and configures a transport endpoint.
type Config struct {
	Transport string `json:"transport"` // "tcp", "tls" or "kcp"
	Address   string `json:"address"`   // host:port
	// For "tls": PEM certificate and key files; empty = a self-signed certificate generated at
	// startup (clients do not verify it, see tls.go).
	Cert string `json:"cert,omitempty"`
	Key  string `json:"key,omitempty"`
}

// Transport is one way of carrying host connections.
type Transport interface {
	Name() string
	// Listen is used by the relay.
	Listen(cfg Config) (net.Listener, error)
	// DialContext is used by host clients. A non-nil local address binds the source (e.g. the
	// physical adapter, to bypass a VPN that captures the default route).
	DialContext(ctx context.Context, address string, local net.IP) (net.Conn, error)
}

var registry = map[string]Transport{}

func register(t Transport) { registry[t.Name()] = t }

// Get returns the transport with the given name; "" means "tcp".
func Get(name string) (Transport, error) {
	if name == "" {
		name = "tcp"
	}
	t, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown transport %q (supported: %v)", name, Names())
	}
	return t, nil
}

// Names lists the supported transports.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Network is the IP protocol a transport listens on: "udp" for KCP, "tcp" for the others (for
// firewall rules).
func Network(name string) string {
	if name == "kcp" {
		return "udp"
	}
	return "tcp"
}

// Listen opens a listener for the configured transport.
func Listen(cfg Config) (net.Listener, error) {
	t, err := Get(cfg.Transport)
	if err != nil {
		return nil, err
	}
	return t.Listen(cfg)
}

// KeepAlivePeriod is the TCP keepalive interval for every tunnel socket.
const KeepAlivePeriod = 15 * time.Second

// TuneTCP applies the options every tunnel socket gets: TCP_NODELAY (game packets are small and
// latency-sensitive, Nagle would hold them back) and TCP keepalive (detects dead peers on idle
// connections). Go enables TCP_NODELAY by default; it is set explicitly so the intent is visible.
func TuneTCP(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetNoDelay(true)
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(KeepAlivePeriod)
}
