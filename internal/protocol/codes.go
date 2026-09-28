package protocol

import "fmt"

// Code is a reason sent in CLOSE frames and in the data connection status byte. Only numbers go
// over the wire: the relay can never put arbitrary text into the host's chat.
type Code byte

const (
	CodeOK                 Code = 0 // data: accepted; CLOSE: normal close
	CodeProtocolError      Code = 1
	CodeUnsupportedVersion Code = 2
	CodeAuthFailed         Code = 3 // unknown user or wrong secret (deliberately indistinguishable)
	CodeBanned             Code = 4 // too many failed attempts from this address
	CodeServerFull         Code = 5 // max_hosts or per-user tunnel limit reached
	CodeNoFreePort         Code = 6
	CodeReplaced           Code = 7 // a newer tunnel of the same user took over
	CodeRevoked            Code = 8 // the user was removed/disabled or its secret changed
	CodeShutdown           Code = 9
	CodeTimeout            Code = 10 // keepalive timeout
	CodeUnknownConn        Code = 11 // data: conn id unknown, expired or already used
	CodeInternal           Code = 12
)

var codeNames = map[Code]string{
	CodeOK:                 "ok",
	CodeProtocolError:      "protocol error",
	CodeUnsupportedVersion: "unsupported protocol version",
	CodeAuthFailed:         "authentication failed",
	CodeBanned:             "temporarily banned",
	CodeServerFull:         "relay is full",
	CodeNoFreePort:         "no free external port",
	CodeReplaced:           "replaced by a newer tunnel",
	CodeRevoked:            "access revoked",
	CodeShutdown:           "relay is shutting down",
	CodeTimeout:            "keepalive timeout",
	CodeUnknownConn:        "unknown or expired connection",
	CodeInternal:           "internal relay error",
}

func (c Code) String() string {
	if s, ok := codeNames[c]; ok {
		return s
	}
	return fmt.Sprintf("code %d", byte(c))
}

// Retryable reports whether a host should reconnect by itself after the relay closed the tunnel
// with this code. Retrying a bad secret would only extend a ban; a replaced tunnel must not fight
// the one that replaced it.
func (c Code) Retryable() bool {
	switch c {
	case CodeAuthFailed, CodeBanned, CodeUnsupportedVersion, CodeReplaced, CodeRevoked:
		return false
	}
	return true
}
