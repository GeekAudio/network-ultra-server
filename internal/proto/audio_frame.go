package proto

import (
	"encoding/binary"
	"errors"
)

// AudioFrame wire layout (little-endian):
//
//   Offset  Bytes  Field
//   0x00    1      type (0xA1)
//   0x01    3      reserved
//   0x04    16     sourcePeerId (UUID raw bytes)
//   0x14    2      seq (uint16 LE)
//   0x16    2      length (uint16 LE)
//   0x18    N      flacPayload
//
//   total = 24 + N bytes

const (
	AudioFrameType  byte = 0xA1
	AudioHeaderSize      = 24
	MaxAudioPayload      = 8192
)

// AudioFrameHeader is the parsed view of the 24-byte header.
type AudioFrameHeader struct {
	SourcePeerID [16]byte
	Seq          uint16
	Length       uint16
}

var (
	ErrTooShort       = errors.New("audio frame: too short")
	ErrBadType        = errors.New("audio frame: invalid type byte")
	ErrPayloadTooBig  = errors.New("audio frame: payload exceeds MaxAudioPayload")
	ErrLengthMismatch = errors.New("audio frame: declared length does not match actual size")
)

// Pack writes header + payload into dst. Caller ensures cap(dst) >= 24+len(payload).
// Returns the slice with len = 24+len(payload).
func Pack(h AudioFrameHeader, payload []byte, dst []byte) ([]byte, error) {
	if len(payload) > MaxAudioPayload {
		return nil, ErrPayloadTooBig
	}
	total := AudioHeaderSize + len(payload)
	if cap(dst) < total {
		dst = make([]byte, total)
	} else {
		dst = dst[:total]
	}
	dst[0] = AudioFrameType
	dst[1], dst[2], dst[3] = 0, 0, 0
	copy(dst[4:20], h.SourcePeerID[:])
	binary.LittleEndian.PutUint16(dst[20:22], h.Seq)
	binary.LittleEndian.PutUint16(dst[22:24], uint16(len(payload)))
	copy(dst[24:], payload)
	return dst, nil
}

// Unpack reads the header from src and returns header + payload slice.
// The returned payload slice aliases src; do not retain past src's lifetime
// unless you copy it.
func Unpack(src []byte) (AudioFrameHeader, []byte, error) {
	var h AudioFrameHeader
	if len(src) < AudioHeaderSize {
		return h, nil, ErrTooShort
	}
	if src[0] != AudioFrameType {
		return h, nil, ErrBadType
	}
	copy(h.SourcePeerID[:], src[4:20])
	h.Seq = binary.LittleEndian.Uint16(src[20:22])
	h.Length = binary.LittleEndian.Uint16(src[22:24])
	if h.Length > MaxAudioPayload {
		return h, nil, ErrPayloadTooBig
	}
	if int(h.Length)+AudioHeaderSize != len(src) {
		return h, nil, ErrLengthMismatch
	}
	return h, src[AudioHeaderSize:], nil
}
