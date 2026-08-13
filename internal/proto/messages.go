// Package proto holds the wire schema for the Network Ultra control + audio protocol.
//
// Control messages: WebSocket TEXT frame, JSON UTF-8.
// Audio messages:   WebSocket BINARY frame, 24-byte header + FLAC payload (see audio_frame.go).
package proto

import (
	"encoding/json"
	"errors"
)

const ProtocolVersion = 1

// Message types
const (
	// Client -> Server
	TypeHello      = "hello"
	TypeRoomCreate = "room_create"
	TypeRoomJoin   = "room_join"
	TypeRoomLeave  = "room_leave"
	TypeRoomList   = "room_list"
	TypePeerMute   = "peer_mute"
	TypeSubscribe  = "subscribe"
	TypePing       = "ping"

	// Server -> Client
	TypeWelcome         = "welcome"
	TypeError           = "error"
	TypeRoomJoined      = "room_joined"
	TypeRoomLeft        = "room_left"
	TypePeerJoined      = "peer_joined"
	TypePeerLeft        = "peer_left"
	TypePeerMuteChanged = "peer_mute_changed"
	TypeRoomListResult  = "room_list_result"
	TypeRoomListDelta   = "room_list_delta"
	TypePong            = "pong"
)

// Error codes (v1)
const (
	ErrRoomNameTaken  = "ROOM_NAME_TAKEN"
	ErrRoomNotFound   = "ROOM_NOT_FOUND"
	ErrRoomFull       = "ROOM_FULL"
	ErrServerFull     = "SERVER_FULL"
	ErrBadPassword    = "BAD_PASSWORD"
	ErrBadUsername    = "BAD_USERNAME"
	ErrRateLimited    = "RATE_LIMITED"
	ErrProtocolError  = "PROTOCOL_ERROR"
	ErrInternalError  = "INTERNAL_ERROR"
	ErrNotInRoom      = "NOT_IN_ROOM"
	ErrAlreadyInRoom  = "ALREADY_IN_ROOM"
	ErrUnsupportedVer = "UNSUPPORTED_VERSION"
	// v1.3+ server-level password gating
	ErrServerPasswordRequired = "SERVER_PASSWORD_REQUIRED"
	ErrBadServerPassword      = "BAD_SERVER_PASSWORD"
)

// Envelope is the outer JSON structure shared by all control messages.
type Envelope struct {
	Type string          `json:"type"`
	V    int             `json:"v"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data"`
}

// ----- Client -> Server payloads -----

type HelloData struct {
	Username       string `json:"username"`
	Client         string `json:"client"`
	Platform       string `json:"platform"`
	ServerPassword string `json:"serverPassword,omitempty"` // v1.3+
}

type RoomCreateData struct {
	RoomName   string `json:"roomName"`
	Visibility string `json:"visibility"` // "public" | "private"
	Password   string `json:"password,omitempty"`
}

type RoomJoinData struct {
	RoomName string `json:"roomName"`
	Password string `json:"password,omitempty"`
	Role     string `json:"role"` // "send" | "recv"
}

type PeerMuteData struct {
	Muted bool `json:"muted"`
}

type SubscribeData struct {
	SourcePeerIDs []string `json:"sourcePeerIds"`
}

type PingData struct {
	TS int64 `json:"ts"` // client unix-ms
}

// ----- Server -> Client payloads -----

type WelcomeData struct {
	PeerID        string `json:"peerId"`
	ServerVersion string `json:"serverVersion"`
	// UDP data plane: the client may send audio over UDP after this welcome.
	// Empty UdpEndpoint means UDP is disabled on this server (client should
	// fall back to WebSocket binary frames). UdpToken is an opaque random,
	// per-session token that must be presented in the first UDP packet so
	// the server can bind the source IP:port back to this peerId.
	UdpEndpoint string `json:"udpEndpoint,omitempty"` // "host:port" form
	UdpToken    string `json:"udpToken,omitempty"`    // base64-std encoded
}

type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PeerInfo struct {
	PeerID   string `json:"peerId"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Muted    bool   `json:"muted"`
	JoinedAt int64  `json:"joinedAt"` // unix-ms
}

type RoomJoinedData struct {
	RoomID   string     `json:"roomId"`
	RoomName string     `json:"roomName"`
	Peers    []PeerInfo `json:"peers"`
}

type RoomLeftData struct {
	Reason string `json:"reason"` // "leave" | "disconnect" | "kick" | "server_shutdown"
}

type PeerJoinedData = PeerInfo

type PeerLeftData struct {
	PeerID string `json:"peerId"`
	Reason string `json:"reason"`
}

type PeerMuteChangedData struct {
	PeerID string `json:"peerId"`
	Muted  bool   `json:"muted"`
}

type RoomListEntry struct {
	RoomName    string `json:"roomName"`
	PeerCount   int    `json:"peerCount"`
	MaxPeers    int    `json:"maxPeers"`
	HasPassword bool   `json:"hasPassword"`
	CreatedAt   int64  `json:"createdAt"`
}

type RoomListResultData struct {
	Rooms []RoomListEntry `json:"rooms"`
}

type RoomListDeltaData struct {
	Added   []RoomListEntry `json:"added,omitempty"`
	Removed []string        `json:"removed,omitempty"`
	Updated []RoomListEntry `json:"updated,omitempty"`
}

type PongData struct {
	ClientTS int64 `json:"clientTs"`
	ServerTS int64 `json:"serverTs"`
}

// Encode wraps a payload into an Envelope and serializes to JSON.
func Encode(typ, id string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{
		Type: typ,
		V:    ProtocolVersion,
		ID:   id,
		Data: raw,
	})
}

// Decode parses a JSON envelope and version-checks.
func Decode(b []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return env, err
	}
	if env.Type == "" {
		return env, errors.New("missing type")
	}
	if env.V == 0 {
		// Tolerate v=0 omitted, treat as 1
		env.V = 1
	}
	return env, nil
}
