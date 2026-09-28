//go:build linux

package proxy

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT from linux/tcp.h (not defined by package syscall on every
// architecture).
const tcpUserTimeout = 0x12

// SetUserTimeout sets TCP_USER_TIMEOUT on a TCP connection: the kernel drops it when sent data
// stays unacknowledged for d, which also covers a peer that keeps advertising a zero window.
// The socket stays a plain *net.TCPConn, so splice(2) still applies. No-op for other conns and
// on other systems.
func SetUserTimeout(c net.Conn, d time.Duration) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(d/time.Millisecond))
	})
}
