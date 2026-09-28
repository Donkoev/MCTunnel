package transport

import (
	"context"
	"net"
)

func init() { register(tcpTransport{}) }

// tcpTransport is plain TCP. Accepted and dialed connections are the raw *net.TCPConn, so
// io.Copy between two of them uses splice(2) on Linux (no copying through user space).
type tcpTransport struct{}

func (tcpTransport) Name() string { return "tcp" }

func (tcpTransport) Listen(cfg Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return nil, err
	}
	return tcpListener{ln.(*net.TCPListener)}, nil
}

func (tcpTransport) DialContext(ctx context.Context, address string, local net.IP) (net.Conn, error) {
	var d net.Dialer
	if local != nil {
		d.LocalAddr = &net.TCPAddr{IP: local}
	}
	c, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	TuneTCP(c)
	return c, nil
}

type tcpListener struct{ *net.TCPListener }

func (l tcpListener) Accept() (net.Conn, error) {
	c, err := l.AcceptTCP()
	if err != nil {
		return nil, err
	}
	TuneTCP(c)
	return c, nil
}
