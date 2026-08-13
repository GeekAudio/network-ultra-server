package room

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// SendFunc is the callback the WS layer registers so the room can push
// outbound frames to a peer without coupling to the websocket package.
type SendFunc func(textPayload []byte, isBinary bool) error

// UdpSendFunc is the callback the UDP layer registers so the room can push
// audio frames over UDP without coupling to the udp package. It receives the
// already-packed wire bytes (header + payload). Returns false if the peer
// has no UDP endpoint bound or the UDP socket is gone — caller should
// fall back to WS.
type UdpSendFunc func(payload []byte) bool

// Peer represents a single WebSocket-attached participant inside a Room.
type Peer struct {
	ID       uuid.UUID
	Username string
	Role     string // "send" | "recv"
	JoinedAt time.Time
	// RemoteIP is captured once from the authenticated WebSocket transport.
	// It is used only as a stable rate-limit dimension across reconnects.
	RemoteIP string

	// muted is per-peer self-mute (Send peer flagging itself).
	muted atomic.Bool

	// subscribed is the set of source peers this Recv peer wants to hear.
	// Stored as atomic.Pointer to a map[uuid.UUID]struct{}; nil = subscribe to all.
	subscribed atomic.Pointer[map[uuid.UUID]struct{}]

	// send is the WS-layer-provided callback. Nil if the peer is detached.
	mu   sync.RWMutex
	send SendFunc

	// UDP delivery: if set, audio fan-out prefers this; falls back to WS on
	// nil return. Network thread reads/writes; brief lock.
	udpSend UdpSendFunc

	// UDP source address (set on UDP hello, validated on every packet).
	// Stored as atomic.Pointer so reads are lock-free on the hot path.
	udpAddr atomic.Pointer[net.UDPAddr]

	// Last room reference (atomic-friendly) so we can clean up on disconnect.
	curRoom atomic.Pointer[Room]
}

func NewPeer(username, role string) *Peer {
	return &Peer{
		ID:       uuid.New(),
		Username: username,
		Role:     role,
		JoinedAt: time.Now(),
	}
}

func (p *Peer) Muted() bool     { return p.muted.Load() }
func (p *Peer) SetMuted(v bool) { p.muted.Store(v) }

// IsSubscribed returns true if this peer (presumed Recv) wants to hear src.
// nil subscription map means "subscribe all".
func (p *Peer) IsSubscribed(src uuid.UUID) bool {
	subs := p.subscribed.Load()
	if subs == nil {
		return true
	}
	_, ok := (*subs)[src]
	return ok
}

func (p *Peer) SetSubscribed(ids []uuid.UUID) {
	if ids == nil {
		p.subscribed.Store(nil)
		return
	}
	m := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	p.subscribed.Store(&m)
}

// AttachSender registers the WS-layer callback. Replaces any prior callback.
func (p *Peer) AttachSender(s SendFunc) {
	p.mu.Lock()
	p.send = s
	p.mu.Unlock()
}

// DetachSender clears the callback (called on disconnect).
func (p *Peer) DetachSender() {
	p.mu.Lock()
	p.send = nil
	p.mu.Unlock()
}

// SendText pushes a control message to this peer. Returns nil if the peer
// is detached (no error to fail-fast; caller decides).
func (p *Peer) SendText(payload []byte) error {
	p.mu.RLock()
	s := p.send
	p.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s(payload, false)
}

func (p *Peer) SendBinary(payload []byte) error {
	// Prefer UDP if the peer has bound a UDP source address. UDP path is
	// lock-free on the hot send: udpSend reads udpAddr atomically and
	// writes once. On any failure (nil func, addr expired, socket dead)
	// we fall through to the WS path so the listener keeps hearing audio
	// during brief UDP outages.
	p.mu.RLock()
	udp := p.udpSend
	ws := p.send
	p.mu.RUnlock()
	if udp != nil && udp(payload) {
		return nil
	}
	if ws == nil {
		return nil
	}
	return ws(payload, true)
}

// AttachUdpSender registers a UDP send callback. Pass nil to detach.
func (p *Peer) AttachUdpSender(s UdpSendFunc) {
	p.mu.Lock()
	p.udpSend = s
	p.mu.Unlock()
}

// SetUdpAddr records the peer's UDP source address (called when the UDP
// hello arrives). Pass nil to clear.
func (p *Peer) SetUdpAddr(addr *net.UDPAddr) { p.udpAddr.Store(addr) }
func (p *Peer) UdpAddr() *net.UDPAddr        { return p.udpAddr.Load() }

// CurrentRoom returns the room this peer is in, or nil.
func (p *Peer) CurrentRoom() *Room { return p.curRoom.Load() }

func (p *Peer) setRoom(r *Room) { p.curRoom.Store(r) }
