// Package udp implements the UDP data plane for Network Ultra.
//
// Why a parallel UDP path when WebSocket already works:
//   - WebSocket runs on TCP, which on a single packet loss enters
//     head-of-line blocking and stalls all subsequent frames until the
//     retransmit succeeds. On long-RTT links (cross-region) we observed
//     1+ second of audio piling up in the jitter buffer after a momentary
//     stall — enough to make the listener hear a freeze followed by an
//     audible delay floor that never recovers.
//   - UDP discards lost packets immediately; the receiver simply hears one
//     10 ms gap that the jitter buffer absorbs as silence. No HOL aftermath.
//
// Design constraints:
//   - Audio frame format on the wire is BYTE-IDENTICAL to the WS binary
//     frame (same 24-byte AudioFrameHeader + payload). Server's room
//     forwarder doesn't care which transport the frame came from or which
//     one it leaves on — the choice is per-peer based on UDP availability.
//   - Authentication piggy-backs on the existing WS hello/welcome handshake.
//     The WS welcome carries a random token tied to the peer's WS session; the client
//     sends one UDP hello packet with this token to bind its source IP:port.
//     This means we never run a second auth flow on UDP.
//   - Falls back gracefully: if a peer never sends a UDP hello, all its
//     audio continues to flow over WebSocket. Coexisting peers in the
//     same room can use different transports.
package udp

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/ratelimit"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

// Maximum UDP datagram we'll accept. Audio frames cap at 24 + kMaxAudioPayload
// (= 24 + 8192 = 8216), so 9000 is comfortably above that.
const maxDatagramSize = 9000

// A client pings every 5 s. Three missed intervals expire the server-side UDP
// route so fan-out immediately falls back to WebSocket instead of silently
// black-holing behind a dead NAT mapping.
const (
	peerIdleTimeout = 15 * time.Second
	gcInterval      = 5 * time.Second
)

type binding struct {
	peer     *room.Peer
	addr     *net.UDPAddr
	token    [proto.UdpTokenSize]byte
	lastSeen time.Time
}

// Server is the UDP data-plane listener.
type Server struct {
	Log     *slog.Logger
	Metrics *metrics.Registry

	// AdvertisedHost is the hostname/IP we tell clients to send UDP to via
	// the WS welcome message. Defaults to the listen address on startup
	// but can be overridden when the server sits behind NAT/load-balancer.
	AdvertisedHost string

	conn *net.UDPConn

	// peerByAddr maps source UDP address -> peer. Lookup happens on every
	// incoming audio packet so it must be fast; we store under RLock and
	// take a brief write lock when binding/unbinding.
	mu         sync.RWMutex
	peerByAddr map[string]*binding // key = addr.String()
	peerByID   map[uuid.UUID]*room.Peer
	tokenByID  map[uuid.UUID][proto.UdpTokenSize]byte
	now        func() time.Time
	randReader io.Reader

	limits       Limits
	helloLimiter *ratelimit.Limiter
	audioLimiter *ratelimit.Limiter

	// Reuse output buffers via a per-goroutine sync.Pool to avoid alloc per
	// frame. Hot path: the room forwarder calls SendAudio at 100 fps × N
	// peers.
	outPool sync.Pool

	closeOnce sync.Once
	doneCh    chan struct{}
}

type Limits struct {
	HelloPerIPPerMinute      int
	AudioFramesPerPeerPerSec int
}

// NewServer initialises a new UDP data-plane server. Call Listen to bind
// the socket and start the read loop.
func NewServer(log *slog.Logger, mreg *metrics.Registry, limits Limits) *Server {
	return &Server{
		Log:          log,
		Metrics:      mreg,
		peerByAddr:   make(map[string]*binding),
		peerByID:     make(map[uuid.UUID]*room.Peer),
		tokenByID:    make(map[uuid.UUID][proto.UdpTokenSize]byte),
		now:          time.Now,
		randReader:   rand.Reader,
		limits:       limits,
		helloLimiter: ratelimit.New(),
		audioLimiter: ratelimit.New(),
		doneCh:       make(chan struct{}),
		outPool: sync.Pool{
			New: func() any {
				b := make([]byte, 0, 4096)
				return &b
			},
		},
	}
}

// Listen binds the UDP socket on listenAddr and starts the read loop.
// listenAddr is the bind address (e.g. "0.0.0.0:18902"). Returns the
// resolved local address (so callers can advertise the correct port even
// when listenAddr uses :0).
func (s *Server) Listen(listenAddr string) error {
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	// Generous read buffer: at peak we're handling ~100 frames/sec/peer,
	// each up to ~1.5 KB, across maybe 20 peers — well under 4 MB.
	_ = conn.SetReadBuffer(4 * 1024 * 1024)
	_ = conn.SetWriteBuffer(4 * 1024 * 1024)
	s.conn = conn
	s.Log.Info("udp listening", "addr", conn.LocalAddr())

	go s.readLoop()
	go s.gcLoop()
	return nil
}

// LocalAddr returns the address the UDP listener is bound to.
func (s *Server) LocalAddr() net.Addr {
	if s.conn == nil {
		return nil
	}
	return s.conn.LocalAddr()
}

// Close shuts down the UDP listener.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.doneCh)
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
}

// MintToken produces a cryptographically random, per-session token. The token is
// returned base64-encoded for inclusion in the WS welcome JSON. Server
// validates the same token in the first UDP hello packet.
func (s *Server) MintToken(peerID uuid.UUID) (string, error) {
	var token [proto.UdpTokenSize]byte
	if _, err := io.ReadFull(s.randReader, token[:]); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokenByID[peerID] = token
	s.mu.Unlock()
	return base64.StdEncoding.EncodeToString(token[:]), nil
}

// AttachPeer registers a peer that has been authenticated via WS. The peer
// stays in our map until DetachPeer is called (typically on WS disconnect).
// Initial UDP source address is unknown; it gets bound on the first valid
// UDP hello.
func (s *Server) AttachPeer(p *room.Peer) {
	s.mu.Lock()
	s.peerByID[p.ID] = p
	s.mu.Unlock()
	// Wire up the send callback so the room forwarder routes audio here.
	p.AttachUdpSender(s.makeUdpSender(p))
}

// DetachPeer unbinds the peer from both the addr and id maps. Safe to call
// even if the peer never bound a UDP address.
func (s *Server) DetachPeer(p *room.Peer) {
	p.AttachUdpSender(nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peerByID, p.ID)
	delete(s.tokenByID, p.ID)
	if a := p.UdpAddr(); a != nil {
		key := a.String()
		if current := s.peerByAddr[key]; current != nil && current.peer == p {
			delete(s.peerByAddr, key)
		}
		// Only clear the endpoint we observed. A future extension may allow
		// rebind; this comparison prevents a stale detach/sweep from clearing it.
		if sameUDPAddr(p.UdpAddr(), a) {
			p.SetUdpAddr(nil)
		}
	}
}

// makeUdpSender returns a closure suitable for room.Peer.AttachUdpSender.
// The closure reads the peer's bound UDP address atomically and writes once;
// no locks on the hot path. Returns false on any failure so the caller
// falls back to WebSocket.
func (s *Server) makeUdpSender(p *room.Peer) room.UdpSendFunc {
	return func(payload []byte) bool {
		s.mu.Lock()
		addr := p.UdpAddr()
		if addr == nil || s.conn == nil {
			s.mu.Unlock()
			return false
		}
		key := addr.String()
		candidate := s.peerByAddr[key]
		if candidate == nil || candidate.peer != p || s.now().Sub(candidate.lastSeen) >= peerIdleTimeout {
			if candidate != nil && candidate.peer == p {
				delete(s.peerByAddr, key)
			}
			if sameUDPAddr(p.UdpAddr(), addr) {
				p.SetUdpAddr(nil)
				if token, ok := s.tokenByID[p.ID]; ok && candidate != nil && token == candidate.token {
					delete(s.tokenByID, p.ID)
				}
			}
			s.mu.Unlock()
			return false
		}
		addr = cloneUDPAddr(addr)
		s.mu.Unlock()
		_, err := s.conn.WriteToUDP(payload, addr)
		if err != nil {
			// One write error doesn't kill the binding (transient UDP
			// failures happen). The gcLoop will eventually reap stale
			// entries by idle timeout. Caller falls back to WS for this
			// frame.
			s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("write")
			return false
		}
		s.Metrics.Counter("nu_udp_audio_frames_sent_total").Inc()
		s.Metrics.Counter("nu_udp_bytes_sent_total").Add(uint64(len(payload)))
		return true
	}
}

// readLoop is the single read goroutine: it dispatches each datagram by its
// type byte to the appropriate handler.
func (s *Server) readLoop() {
	buf := make([]byte, maxDatagramSize)
	for {
		select {
		case <-s.doneCh:
			return
		default:
		}
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.Log.Debug("udp read error", "err", err)
			continue
		}
		// Defensive: empty or malformed datagrams. We never log these at
		// info level because anyone can spam UDP at us; we just bump the
		// metric and move on.
		if n < 1 {
			s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("empty")
			continue
		}
		typ := buf[0]
		switch typ {
		case proto.AudioFrameType:
			s.handleAudio(buf[:n], src)
		case proto.UdpFrameTypeHello:
			s.handleHello(buf[:n], src)
		case proto.UdpFrameTypePing:
			s.handlePing(buf[:n], src)
		default:
			s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_type")
		}
	}
}

// handleHello validates the token, binds the source addr, and replies with
// a UdpWelcome echoing the peerId.
func (s *Server) handleHello(pkt []byte, src *net.UDPAddr) {
	hello, err := proto.UnpackUdpHello(pkt)
	if err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_hello")
		return
	}

	// Verify the random token issued inside the authenticated WS session.
	// Any failure is silently dropped to keep us from
	// becoming a UDP amplifier for spoofed tokens.
	id := uuid.UUID(hello.PeerID)
	s.mu.Lock()
	p, ok := s.peerByID[id]
	token, tokenOK := s.tokenByID[id]
	if !ok || !tokenOK {
		s.mu.Unlock()
		// Peer not registered — its WS session must have ended (or never
		// arrived). Drop silently.
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_peer")
		return
	}
	if subtle.ConstantTimeCompare(hello.Token[:], token[:]) != 1 {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_token")
		return
	}
	// Only authenticated peers get limiter entries. A UDP source address can
	// be spoofed, so creating entries before token validation would allow an
	// unauthenticated cardinality/memory attack with arbitrary source IPs.
	if !s.helloLimiter.Allow(id.String(), s.limits.HelloPerIPPerMinute, time.Minute) {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("hello_rate_limit")
		return
	}
	// A token can be retried from the original endpoint (welcome may be lost),
	// but it can never rebind a live WS peer to a different source address.
	if oldAddr := p.UdpAddr(); oldAddr != nil && oldAddr.String() != src.String() {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("endpoint_rebind")
		return
	}
	if owner := s.peerByAddr[src.String()]; owner != nil && owner.peer.ID != p.ID {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("endpoint_in_use")
		return
	}
	bound := *src
	p.SetUdpAddr(&bound)
	s.peerByAddr[src.String()] = &binding{peer: p, addr: &bound, token: token, lastSeen: s.now()}
	s.mu.Unlock()

	// Reply so the client knows it's bound. The reply is also necessary for
	// some symmetric NATs to keep the inbound mapping alive.
	reply := proto.PackUdpWelcome(hello.PeerID, nil)
	if _, err := s.conn.WriteToUDP(reply, src); err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("welcome_write")
	}
	s.Metrics.Counter("nu_udp_handshake_total").Inc()
	s.Log.Info("udp peer bound", "peerId", id, "src", src)
}

// handlePing replies only to an authenticated, endpoint-bound peer. This
// avoids turning the listener into an unauthenticated UDP reflection service.
func (s *Server) handlePing(pkt []byte, src *net.UDPAddr) {
	ping, err := proto.UnpackUdpPing(pkt)
	if err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_ping")
		return
	}
	s.mu.Lock()
	b := s.peerByAddr[src.String()]
	if b == nil || [16]byte(b.peer.ID) != ping.PeerID {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_ping_source")
		return
	}
	b.lastSeen = s.now()
	s.mu.Unlock()
	pong := proto.PackUdpPong(ping.PeerID, nil)
	_, _ = s.conn.WriteToUDP(pong, src)
	s.Metrics.Counter("nu_udp_pings_total").Inc()
}

// handleAudio looks up the peer by source addr, anti-spoofs the source
// peerId in the header against what we expect, and forwards via the
// existing room forwarder (which fans out to other peers using their
// chosen transport).
func (s *Server) handleAudio(pkt []byte, src *net.UDPAddr) {
	s.mu.Lock()
	b := s.peerByAddr[src.String()]
	if b == nil {
		s.mu.Unlock()
		// Unknown source. Could be a delayed packet from a peer that
		// re-bound, or a spoof. Drop silently.
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_source")
		return
	}
	p := b.peer
	if !s.audioLimiter.Allow(p.ID.String(), s.limits.AudioFramesPerPeerPerSec, time.Second) {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("audio_rate_limit")
		return
	}
	hdr, _, err := proto.Unpack(pkt)
	if err != nil {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_audio")
		return
	}
	// Anti-spoof: the peerId in the header must match the peer the source
	// addr is bound to.
	if hdr.SourcePeerID != [16]byte(p.ID) {
		s.mu.Unlock()
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("spoof_peer_id")
		return
	}
	b.lastSeen = s.now()
	s.mu.Unlock()
	if p.Muted() {
		s.Metrics.LabeledCounter("nu_audio_frames_dropped_total", "reason").Inc("muted")
		return
	}
	rm := p.CurrentRoom()
	if rm == nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("not_in_room")
		return
	}
	// Only now copy the frame for the asynchronous room forwarder. Unknown or
	// malformed datagrams therefore never force a per-packet allocation.
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	rm.Forward(&room.Frame{
		SourcePeerID: p.ID,
		Payload:      cp,
	})
	s.Metrics.Counter("nu_udp_audio_frames_recv_total").Inc()
	s.Metrics.Counter("nu_udp_bytes_recv_total").Add(uint64(len(pkt)))
}

// gcLoop periodically reaps peers whose UDP binding has gone idle. WS
// disconnect path also detaches us, but residential NATs sometimes
// silently rebind without notice; the timeout guards against ghost
// entries.
func (s *Server) gcLoop() {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.expireIdle(s.now())
		case <-s.doneCh:
			return
		}
	}
}

func (s *Server) expireIdle(now time.Time) {
	s.mu.Lock()
	for key, candidate := range s.peerByAddr {
		if now.Sub(candidate.lastSeen) < peerIdleTimeout {
			continue
		}
		// Both map identity and the peer's current endpoint must still match
		// the expired record. This makes a stale sweep harmless after detach or
		// any future authenticated rebind.
		if current := s.peerByAddr[key]; current != candidate {
			continue
		}
		delete(s.peerByAddr, key)
		if sameUDPAddr(candidate.peer.UdpAddr(), candidate.addr) {
			candidate.peer.SetUdpAddr(nil)
			if token, ok := s.tokenByID[candidate.peer.ID]; ok && token == candidate.token {
				delete(s.tokenByID, candidate.peer.ID)
			}
		}
		s.Metrics.Counter("nu_udp_bindings_expired_total").Inc()
	}
	s.mu.Unlock()
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.Zone == b.Zone && a.IP.Equal(b.IP)
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}
}
