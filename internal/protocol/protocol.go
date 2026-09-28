// Package protocol implements the MCTunnel v1 wire format shared by the relay and the host
// clients (the Fabric mod implements the same format in Java). See docs/PROTOCOL.md.
//
// Every host→relay connection starts with a 6-byte preamble: "MCTN", version, kind.
// A control connection then carries frames: type u8 | length u16 (big endian) | payload.
// A data connection carries one fixed-size header, a 1-byte status reply and then raw bytes.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version byte = 1

	KindControl byte = 1
	KindData    byte = 2

	PreambleSize    = 6
	NonceSize       = 32
	MACSize         = 32 // HMAC-SHA256
	MaxUserLen      = 32
	MaxFramePayload = 256
	FrameHeaderSize = 3
	DataHeaderSize  = 8 + MACSize // conn id + MAC, follows the preamble
)

// Magic starts every host→relay connection.
var Magic = [4]byte{'M', 'C', 'T', 'N'}

// Frame types.
const (
	TypeHello     byte = 0x01 // host → relay
	TypeChallenge byte = 0x02 // relay → host
	TypeAuth      byte = 0x03 // host → relay
	TypeWelcome   byte = 0x04 // relay → host
	TypeOpen      byte = 0x05 // relay → host
	TypePing      byte = 0x06 // both
	TypePong      byte = 0x07 // both
	TypeClose     byte = 0x08 // both
)

var (
	// ErrProtocol wraps every violation of the wire format; the connection must be dropped.
	ErrProtocol = errors.New("protocol violation")

	ErrBadMagic           = fmt.Errorf("%w: bad magic", ErrProtocol)
	ErrUnsupportedVersion = fmt.Errorf("%w: unsupported version", ErrProtocol)
	ErrBadKind            = fmt.Errorf("%w: unknown connection kind", ErrProtocol)
	ErrFrameTooLarge      = fmt.Errorf("%w: frame too large", ErrProtocol)
	ErrUnknownType        = fmt.Errorf("%w: unknown frame type", ErrProtocol)
	ErrBadLength          = fmt.Errorf("%w: bad frame length", ErrProtocol)
	ErrBadUser            = fmt.Errorf("%w: bad user name", ErrProtocol)
)

type (
	Nonce [NonceSize]byte
	MAC   [MACSize]byte
)

// Message is a decoded control frame.
type Message interface {
	Type() byte
	appendPayload(b []byte) []byte
}

// Hello opens the control handshake: who the host claims to be, plus its half of the freshness.
type Hello struct {
	User        string
	ClientNonce Nonce
}

// Challenge is the relay's nonce the host must prove the secret against.
type Challenge struct {
	ServerNonce Nonce
}

// Auth carries the host's proof: ControlMAC.
type Auth struct {
	MAC MAC
}

// Welcome confirms the tunnel: the external port chosen by the relay and the relay's own proof.
type Welcome struct {
	ExternalPort uint16
	ServerProof  MAC
}

// Open asks the host to open a data connection for a newly connected player.
// It deliberately has no address or port: the host always forwards to its own LAN port.
type Open struct {
	ConnID uint64
	Nonce  Nonce
}

// Ping must be answered with a Pong carrying the same payload.
type Ping struct {
	Payload uint64
}

type Pong struct {
	Payload uint64
}

// Close announces that the sender is closing the control connection and why.
type Close struct {
	Code Code
}

func (*Hello) Type() byte     { return TypeHello }
func (*Challenge) Type() byte { return TypeChallenge }
func (*Auth) Type() byte      { return TypeAuth }
func (*Welcome) Type() byte   { return TypeWelcome }
func (*Open) Type() byte      { return TypeOpen }
func (*Ping) Type() byte      { return TypePing }
func (*Pong) Type() byte      { return TypePong }
func (*Close) Type() byte     { return TypeClose }

func (m *Hello) appendPayload(b []byte) []byte {
	b = append(b, byte(len(m.User)))
	b = append(b, m.User...)
	return append(b, m.ClientNonce[:]...)
}

func (m *Challenge) appendPayload(b []byte) []byte { return append(b, m.ServerNonce[:]...) }
func (m *Auth) appendPayload(b []byte) []byte      { return append(b, m.MAC[:]...) }

func (m *Welcome) appendPayload(b []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, m.ExternalPort)
	return append(b, m.ServerProof[:]...)
}

func (m *Open) appendPayload(b []byte) []byte {
	b = binary.BigEndian.AppendUint64(b, m.ConnID)
	return append(b, m.Nonce[:]...)
}

func (m *Ping) appendPayload(b []byte) []byte  { return binary.BigEndian.AppendUint64(b, m.Payload) }
func (m *Pong) appendPayload(b []byte) []byte  { return binary.BigEndian.AppendUint64(b, m.Payload) }
func (m *Close) appendPayload(b []byte) []byte { return append(b, byte(m.Code)) }

// AppendFrame appends m as a complete frame (header + payload) to b.
func AppendFrame(b []byte, m Message) []byte {
	start := len(b)
	b = append(b, m.Type(), 0, 0)
	b = m.appendPayload(b)
	binary.BigEndian.PutUint16(b[start+1:], uint16(len(b)-start-FrameHeaderSize))
	return b
}

// WriteMessage writes m as one frame with a single Write call (one TCP segment with TCP_NODELAY).
func WriteMessage(w io.Writer, m Message) error {
	var buf [FrameHeaderSize + MaxFramePayload]byte
	_, err := w.Write(AppendFrame(buf[:0], m))
	return err
}

// ReadMessage reads and decodes one frame. The declared length is checked before anything is
// read, so a peer can never make us buffer more than MaxFramePayload bytes.
// A clean close between frames returns io.EOF; a close inside a frame returns io.ErrUnexpectedEOF.
func ReadMessage(r io.Reader) (Message, error) {
	var hdr [FrameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	if n > MaxFramePayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	var buf [MaxFramePayload]byte
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return Decode(hdr[0], buf[:n])
}

// Decode parses a frame payload. Every type has an exact (or, for Hello, bounded) length.
func Decode(typ byte, p []byte) (Message, error) {
	switch typ {
	case TypeHello:
		if len(p) < 1 {
			return nil, ErrBadLength
		}
		ul := int(p[0])
		if ul < 1 || ul > MaxUserLen || len(p) != 1+ul+NonceSize {
			return nil, ErrBadLength
		}
		user := string(p[1 : 1+ul])
		if !ValidUser(user) {
			return nil, ErrBadUser
		}
		m := &Hello{User: user}
		copy(m.ClientNonce[:], p[1+ul:])
		return m, nil
	case TypeChallenge:
		if len(p) != NonceSize {
			return nil, ErrBadLength
		}
		m := &Challenge{}
		copy(m.ServerNonce[:], p)
		return m, nil
	case TypeAuth:
		if len(p) != MACSize {
			return nil, ErrBadLength
		}
		m := &Auth{}
		copy(m.MAC[:], p)
		return m, nil
	case TypeWelcome:
		if len(p) != 2+MACSize {
			return nil, ErrBadLength
		}
		m := &Welcome{ExternalPort: binary.BigEndian.Uint16(p)}
		copy(m.ServerProof[:], p[2:])
		return m, nil
	case TypeOpen:
		if len(p) != 8+NonceSize {
			return nil, ErrBadLength
		}
		m := &Open{ConnID: binary.BigEndian.Uint64(p)}
		copy(m.Nonce[:], p[8:])
		return m, nil
	case TypePing, TypePong:
		if len(p) != 8 {
			return nil, ErrBadLength
		}
		v := binary.BigEndian.Uint64(p)
		if typ == TypePing {
			return &Ping{Payload: v}, nil
		}
		return &Pong{Payload: v}, nil
	case TypeClose:
		if len(p) != 1 {
			return nil, ErrBadLength
		}
		return &Close{Code: Code(p[0])}, nil
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownType, typ)
	}
}

// ValidUser reports whether s is an acceptable user name: 1..32 characters of [A-Za-z0-9._-].
func ValidUser(s string) bool {
	if len(s) < 1 || len(s) > MaxUserLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// AppendPreamble appends the connection preamble for the given kind.
func AppendPreamble(b []byte, kind byte) []byte {
	return append(b, Magic[0], Magic[1], Magic[2], Magic[3], Version, kind)
}

// ReadPreamble reads the preamble and returns the connection kind.
func ReadPreamble(r io.Reader) (byte, error) {
	var p [PreambleSize]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return 0, err
	}
	if [4]byte(p[:4]) != Magic {
		return 0, ErrBadMagic
	}
	if p[4] != Version {
		return 0, fmt.Errorf("%w: %d", ErrUnsupportedVersion, p[4])
	}
	if p[5] != KindControl && p[5] != KindData {
		return 0, fmt.Errorf("%w: %d", ErrBadKind, p[5])
	}
	return p[5], nil
}

// DataHeader follows the preamble on a data connection.
type DataHeader struct {
	ConnID uint64
	MAC    MAC
}

// AppendDataHeader appends the data connection header (without the preamble).
func AppendDataHeader(b []byte, h DataHeader) []byte {
	b = binary.BigEndian.AppendUint64(b, h.ConnID)
	return append(b, h.MAC[:]...)
}

// ReadDataHeader reads exactly DataHeaderSize bytes, never more: whatever follows belongs to the
// tunnelled stream.
func ReadDataHeader(r io.Reader) (DataHeader, error) {
	var p [DataHeaderSize]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return DataHeader{}, err
	}
	h := DataHeader{ConnID: binary.BigEndian.Uint64(p[:8])}
	copy(h.MAC[:], p[8:])
	return h, nil
}
