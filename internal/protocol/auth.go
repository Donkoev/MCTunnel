package protocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
)

// Every MAC starts with its own label, so a MAC computed for one purpose can never be replayed
// for another (a data MAC as a control proof, the relay's proof as the host's, ...).
var (
	labelControl = []byte("mctunnel/v1/control\x00")
	labelServer  = []byte("mctunnel/v1/server\x00")
	labelData    = []byte("mctunnel/v1/data\x00")
)

// ControlMAC is the host's proof of the secret, sent in AUTH:
// HMAC-SHA256(secret, "mctunnel/v1/control\0" ‖ server_nonce ‖ client_nonce ‖ len(user) ‖ user).
func ControlMAC(secret []byte, serverNonce, clientNonce Nonce, user string) MAC {
	return sum(secret, labelControl, serverNonce[:], clientNonce[:], []byte{byte(len(user))}, []byte(user))
}

// ServerProof is the relay's proof of the secret, sent in WELCOME only after the host proved
// itself: same input as ControlMAC under a different label.
func ServerProof(secret []byte, serverNonce, clientNonce Nonce, user string) MAC {
	return sum(secret, labelServer, serverNonce[:], clientNonce[:], []byte{byte(len(user))}, []byte(user))
}

// DataMAC authenticates the data connection answering OPEN(connID, nonce):
// HMAC-SHA256(secret, "mctunnel/v1/data\0" ‖ conn_id (u64 BE) ‖ nonce).
func DataMAC(secret []byte, connID uint64, nonce Nonce) MAC {
	var id [8]byte
	binary.BigEndian.PutUint64(id[:], connID)
	return sum(secret, labelData, id[:], nonce[:])
}

// EqualMAC compares in constant time.
func EqualMAC(a, b MAC) bool {
	return hmac.Equal(a[:], b[:])
}

// NewNonce returns 32 bytes from the OS CSPRNG.
func NewNonce() Nonce {
	var n Nonce
	if _, err := rand.Read(n[:]); err != nil {
		// crypto/rand never fails on supported platforms; continuing with a zero nonce would not be safe.
		panic("mctunnel: crypto/rand failed: " + err.Error())
	}
	return n
}

func sum(key []byte, parts ...[]byte) MAC {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	var m MAC
	h.Sum(m[:0])
	return m
}
