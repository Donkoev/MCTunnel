package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"time"
)

func init() { register(tlsTransport{}) }

// tlsTransport is TCP wrapped in TLS, meant for port 443: it passes networks that only let
// HTTPS through and hides the tunnel from passive observers.
//
// Certificates are not verified by clients. Both sides already authenticate each other with the
// tunnel protocol's HMAC handshake (docs/PROTOCOL.md); TLS adds reachability and confidentiality,
// not identity. Without "cert"/"key" the relay generates a self-signed certificate at startup.
type tlsTransport struct{}

func (tlsTransport) Name() string { return "tls" }

func (tlsTransport) Listen(cfg Config) (net.Listener, error) {
	cert, err := serverCertificate(cfg.Cert, cfg.Key)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return nil, err
	}
	return &tlsListener{Listener: ln, config: &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}}, nil
}

type tlsListener struct {
	net.Listener
	config *tls.Config
}

// Accept returns the TLS connection without handshaking: the handshake runs on the first read,
// under the relay's handshake deadline, so a slow client cannot block the accept loop.
func (l *tlsListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	TuneTCP(c)
	return tls.Server(c, l.config), nil
}

func (tlsTransport) DialContext(ctx context.Context, address string, local net.IP) (net.Conn, error) {
	c, err := tcpTransport{}.DialContext(ctx, address, local)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		InsecureSkipVerify: true, // see the type comment: identity comes from the tunnel protocol
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"http/1.1"},
	}
	if host, _, err := net.SplitHostPort(address); err == nil && net.ParseIP(host) == nil {
		cfg.ServerName = host
	}
	tc := tls.Client(c, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

func serverCertificate(certFile, keyFile string) (tls.Certificate, error) {
	if certFile != "" || keyFile != "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mctunnel-relay"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-signed certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
