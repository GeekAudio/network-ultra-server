package proto

// UDP control-frame wire formats (only used on the UDP data plane; never
// sent over WebSocket).
//
// Audio frames on UDP reuse the existing AudioFrameHeader (type 0xA1) so the
// server's room.Forward path is identical for both transports.
//
// Type assignments:
//   0xA1  AudioFrame (unchanged)         — bidirectional
//   0xC0  UdpHello                       — client → server
//   0xC1  UdpWelcome                     — server → client
//   0xC2  UdpPing (NAT keepalive)        — client → server
//   0xC3  UdpPong                        — server → client
//
// All multi-byte ints are little-endian to match AudioFrameHeader.

import (
	"errors"
)

const (
	UdpFrameTypeHello   byte = 0xC0
	UdpFrameTypeWelcome byte = 0xC1
	UdpFrameTypePing    byte = 0xC2
	UdpFrameTypePong    byte = 0xC3

	UdpHelloSize   = 52 // 1 type + 3 reserved + 16 peerId + 32 token
	UdpWelcomeSize = 20 // 1 type + 3 reserved + 16 peerId (echo back)
	UdpPingSize    = 20 // 1 type + 3 reserved + 16 peerId
	UdpPongSize    = 20 // same as ping (server echoes)

	UdpTokenSize = 32 // raw per-session random bytes — opaque to client
)

var (
	ErrUdpTooShort = errors.New("udp frame: too short")
	ErrUdpBadType  = errors.New("udp frame: invalid type byte")
	ErrUdpBadSize  = errors.New("udp frame: size mismatch")
)

// UdpHelloFrame is sent by the client immediately after WS welcome to bind
// its source IP:port to its peer ID on the server's UDP listener.
type UdpHelloFrame struct {
	PeerID [16]byte
	Token  [UdpTokenSize]byte
}

func PackUdpHello(f UdpHelloFrame, dst []byte) []byte {
	if cap(dst) < UdpHelloSize {
		dst = make([]byte, UdpHelloSize)
	} else {
		dst = dst[:UdpHelloSize]
	}
	dst[0] = UdpFrameTypeHello
	dst[1], dst[2], dst[3] = 0, 0, 0
	copy(dst[4:20], f.PeerID[:])
	copy(dst[20:52], f.Token[:])
	return dst
}

func UnpackUdpHello(src []byte) (UdpHelloFrame, error) {
	var f UdpHelloFrame
	if len(src) != UdpHelloSize {
		return f, ErrUdpBadSize
	}
	if src[0] != UdpFrameTypeHello {
		return f, ErrUdpBadType
	}
	copy(f.PeerID[:], src[4:20])
	copy(f.Token[:], src[20:52])
	return f, nil
}

// UdpWelcomeFrame echoes the peerId so the client knows the server bound
// the source addr correctly. After this the client may start sending audio.
func PackUdpWelcome(peerID [16]byte, dst []byte) []byte {
	if cap(dst) < UdpWelcomeSize {
		dst = make([]byte, UdpWelcomeSize)
	} else {
		dst = dst[:UdpWelcomeSize]
	}
	dst[0] = UdpFrameTypeWelcome
	dst[1], dst[2], dst[3] = 0, 0, 0
	copy(dst[4:20], peerID[:])
	return dst
}

// UdpPingFrame keeps the NAT mapping alive (every 5 s on the client side).
// Server replies with UdpPongFrame so the client also knows its outbound
// path is healthy.
type UdpPingFrame struct {
	PeerID [16]byte
}

func PackUdpPing(peerID [16]byte, dst []byte) []byte {
	if cap(dst) < UdpPingSize {
		dst = make([]byte, UdpPingSize)
	} else {
		dst = dst[:UdpPingSize]
	}
	dst[0] = UdpFrameTypePing
	dst[1], dst[2], dst[3] = 0, 0, 0
	copy(dst[4:20], peerID[:])
	return dst
}

func UnpackUdpPing(src []byte) (UdpPingFrame, error) {
	var f UdpPingFrame
	if len(src) != UdpPingSize {
		return f, ErrUdpBadSize
	}
	if src[0] != UdpFrameTypePing {
		return f, ErrUdpBadType
	}
	copy(f.PeerID[:], src[4:20])
	return f, nil
}

func PackUdpPong(peerID [16]byte, dst []byte) []byte {
	if cap(dst) < UdpPongSize {
		dst = make([]byte, UdpPongSize)
	} else {
		dst = dst[:UdpPongSize]
	}
	dst[0] = UdpFrameTypePong
	dst[1], dst[2], dst[3] = 0, 0, 0
	copy(dst[4:20], peerID[:])
	return dst
}

// PeekUdpType returns the type byte without parsing further. Used by the UDP
// listener loop to dispatch between hello / ping / audio handlers.
func PeekUdpType(src []byte) (byte, error) {
	if len(src) < 1 {
		return 0, ErrUdpTooShort
	}
	return src[0], nil
}
