//go:build !linux

package proxy

import (
	"net"
	"time"
)

// SetUserTimeout sets TCP_USER_TIMEOUT on Linux (see usertimeout_linux.go); elsewhere it does
// nothing.
func SetUserTimeout(c net.Conn, d time.Duration) {}
