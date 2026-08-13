package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/ratelimit"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

const (
	writeTimeout        = 10 * time.Second // single frame write deadline; tolerant of 500ms RTT stalls
	helloTimeout        = 10 * time.Second
	pingTimeout         = 30 * time.Second
	connWriteQueueDepth = 1024 // ~10s of audio at 100 fps; absorbs long-RTT bursts
)

var errPasswordWorkBusy = errors.New("password verification busy")

type Server struct {
	Reg     *room.Registry
	Metrics *metrics.Registry
	Log     *slog.Logger

	// Limits
	MaxConnections int
	RateLimits     RateLimits
	Version        string
	// TrustedProxyPrefixes gates use of Forwarded/X-Forwarded-For. Empty is
	// fail-closed: transport RemoteAddr is always used.
	TrustedProxyPrefixes []netip.Prefix

	// Optional: the WS subprotocol clients must request.
	Subprotocol string

	// Server-level password gating (v1.3+). When non-nil, clients must
	// include a matching serverPassword in their hello message. When nil,
	// the server is open (legacy behaviour). Stored only as bcrypt hash;
	// derived from config.Server.Password at startup.
	ServerPasswordHash []byte

	// UDP data plane advertisement. Empty UdpEndpoint means UDP is disabled
	// (Udp may still be set so DetachPeer is a no-op for code symmetry).
	UdpEndpoint string

	// UDP port the data plane listens on. When non-zero AND UdpEndpoint is
	// empty, the welcome message advertises {host-from-http-Host}:UdpPort,
	// which works correctly behind cloud NAT where the server has no idea
	// what its public hostname is.
	UdpPort int

	// UDP data plane (optional). When set, the WS welcome message advertises
	// the UDP endpoint + a per-session random token; clients that successfully
	// handshake on UDP will route audio there. Nil means UDP is disabled
	// and all audio continues to flow over WebSocket binary frames.
	Udp UdpAdvertiser

	// stats
	curConns     int64
	mu           sync.Mutex
	securityOnce sync.Once
	helloLimiter *ratelimit.Limiter
	peerLimiter  *ratelimit.Limiter
	passwordSem  chan struct{}
}

type RateLimits struct {
	HelloPerIPPerMinute        int
	RoomCreatePerPeerPerMinute int
	RoomJoinPerPeerPerMinute   int
	RoomListPerPeerPerMinute   int
	ControlPerPeerPerMinute    int
	AudioFramesPerPeerPerSec   int
	PasswordChecksConcurrent   int
}

func (s *Server) initSecurity() {
	s.securityOnce.Do(func() {
		s.helloLimiter = ratelimit.New()
		s.peerLimiter = ratelimit.New()
		cap := s.RateLimits.PasswordChecksConcurrent
		if cap <= 0 {
			cap = 4
		}
		s.passwordSem = make(chan struct{}, cap)
	})
}

func (s *Server) version() string {
	if s.Version == "" {
		return "dev"
	}
	return s.Version
}

// UdpAdvertiser is the small interface the WS layer needs from the UDP
// data plane to advertise it in the welcome message and bind peers as they
// authenticate. Implemented by udp.Server. Kept as an interface so the
// ws package doesn't import the udp package directly (which would create
// an import cycle since udp imports room).
type UdpAdvertiser interface {
	MintToken(peerID uuid.UUID) (string, error)
	AttachPeer(p *room.Peer)
	DetachPeer(p *room.Peer)
}

// HandleHTTP upgrades incoming HTTP into a WebSocket connection and runs it.
func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	// This endpoint is exclusively for native clients. Reject every browser
	// Origin, including a value that matches Host: DNS rebinding or a forged
	// Host header must not turn the native control plane into a browser API.
	for _, origin := range r.Header.Values("Origin") {
		if strings.TrimSpace(origin) != "" {
			http.Error(w, "browser websocket origins are forbidden", http.StatusForbidden)
			return
		}
	}
	s.initSecurity()
	// Per-process cap.
	s.mu.Lock()
	if int(s.curConns) >= s.MaxConnections {
		s.mu.Unlock()
		http.Error(w, "server full", http.StatusServiceUnavailable)
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrServerFull)
		return
	}
	s.curConns++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.curConns--
		s.mu.Unlock()
	}()

	// Keep coder/websocket's own checks as defense in depth. The explicit gate
	// above is stricter and allows only native clients that omit Origin.
	opts := &websocket.AcceptOptions{}
	if s.Subprotocol != "" {
		opts.Subprotocols = []string{s.Subprotocol}
	}

	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.Log.Warn("ws upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	defer c.Close(websocket.StatusInternalError, "shutting down")
	// Control envelopes are small and audio frames are capped below 9 KiB.
	// A modest connection-level cap prevents a single peer from forcing an
	// 8 MiB allocation before field validation runs.
	c.SetReadLimit(64 * 1024)

	s.Metrics.Counter("nu_ws_connections_total").Inc()

	conn := newConn(c, s.Log)
	defer conn.close()

	remoteIP := s.clientIP(r)
	if err := s.run(r.Context(), conn, remoteIP, r.Host); err != nil {
		s.Log.Debug("session ended", "err", err, "remote", r.RemoteAddr)
	}
}

// run drives a single WS session: hello → loop dispatching messages.
//
// hostHeader is the HTTP Host the client used to reach us (e.g.
// "175.178.62.76:18900"). We use just the host part to advertise the UDP
// endpoint, so clients connecting via "localhost", "127.0.0.1", a public
// IP, or a domain all get back a UDP host they can actually reach.
func (s *Server) run(parent context.Context, conn *Conn, remoteIP, hostHeader string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// 1. Wait for hello.
	hctx, hcancel := context.WithTimeout(ctx, helloTimeout)
	defer hcancel()

	mt, payload, err := conn.read(hctx)
	if err != nil {
		return err
	}
	if mt != websocket.MessageText {
		return s.protoError(conn, "first frame must be hello (text)")
	}
	if !utf8.Valid(payload) {
		return s.protoError(conn, "hello must be valid UTF-8")
	}
	if !s.helloLimiter.Allow(remoteIP, s.RateLimits.HelloPerIPPerMinute, time.Minute) {
		s.sendError(conn, "", proto.ErrRateLimited, "too many hello attempts")
		return errors.New("hello rate limited")
	}

	env, err := proto.Decode(payload)
	if err != nil || env.Type != proto.TypeHello || env.V > proto.ProtocolVersion {
		return s.protoError(conn, "bad hello")
	}
	var hello proto.HelloData
	if err := json.Unmarshal(env.Data, &hello); err != nil {
		return s.protoError(conn, "bad hello data")
	}
	if !validUsername(hello.Username) {
		s.sendError(conn, env.ID, proto.ErrBadUsername, "username invalid")
		return errors.New("bad username")
	}
	hello.Username = strings.TrimSpace(hello.Username)
	if len([]byte(hello.ServerPassword)) > proto.MaxPasswordBytes {
		return s.protoError(conn, "serverPassword exceeds bcrypt 72-byte limit")
	}

	// v1.3+ server-level password gating. Empty hash = open server (legacy).
	// Empty client-supplied password against a non-empty hash => "required".
	// Wrong client-supplied password against a non-empty hash => "bad".
	if len(s.ServerPasswordHash) > 0 {
		if hello.ServerPassword == "" {
			s.sendError(conn, env.ID, proto.ErrServerPasswordRequired,
				"this server requires a password; please set it in the client and reconnect")
			return errors.New("server password required")
		}
		passwordErr := s.doPasswordWork(func() error {
			return bcrypt.CompareHashAndPassword(s.ServerPasswordHash, []byte(hello.ServerPassword))
		})
		if errors.Is(passwordErr, errPasswordWorkBusy) {
			s.sendError(conn, env.ID, proto.ErrRateLimited, "password verification busy")
			return errors.New("password verification saturated")
		}
		if passwordErr != nil {
			s.sendError(conn, env.ID, proto.ErrBadServerPassword, "bad server password")
			return errors.New("bad server password")
		}
	}

	peer := room.NewPeer(hello.Username, "")
	peer.RemoteIP = remoteIP
	s.Metrics.Gauge("nu_active_peers").Inc()
	defer s.Metrics.Gauge("nu_active_peers").Dec()

	peer.AttachSender(conn.SendFunc())
	defer peer.DetachSender()

	welcome := proto.WelcomeData{
		PeerID:        peer.ID.String(),
		ServerVersion: s.version(),
	}
	if s.Udp != nil {
		// Resolve the UDP endpoint to advertise. Priority:
		//   1. Explicit UdpEndpoint config (admin override; rare).
		//   2. Derive from the HTTP Host the client used to connect — this
		//      is the most reliable, because the client by definition can
		//      reach that host on at least the WS port. We just swap the
		//      port to the UDP port.
		// This avoids the cloud-server gotcha where `hostname -I` returns
		// an internal IP (e.g. 10.x.x.x) that public clients can't reach.
		if s.UdpEndpoint != "" {
			welcome.UdpEndpoint = s.UdpEndpoint
		} else if udpPort := s.UdpPort; udpPort > 0 && hostHeader != "" {
			h, _, err := net.SplitHostPort(hostHeader)
			if err != nil {
				h = hostHeader // bare host, no port
			}
			if h != "" {
				welcome.UdpEndpoint = net.JoinHostPort(h,
					strconv.Itoa(udpPort))
			}
		}
		if welcome.UdpEndpoint != "" {
			welcome.UdpToken, err = s.Udp.MintToken(peer.ID)
			if err != nil {
				return fmt.Errorf("mint UDP token: %w", err)
			}
		}
	}
	// Register only after token generation succeeds. A failed entropy source
	// closes this WS session without exposing a half-populated UDP capability or
	// briefly attaching a peer to the data plane.
	if s.Udp != nil {
		s.Udp.AttachPeer(peer)
		defer s.Udp.DetachPeer(peer)
	}
	if err := s.send(conn, proto.TypeWelcome, env.ID, welcome); err != nil {
		return err
	}

	s.Log.Info("peer authenticated", "peerId", peer.ID, "username", hello.Username, "host", remoteIP)

	// Server-side WebSocket ping every 15s. Many residential / mobile
	// networks silently drop idle TCP after ~30s; without a periodic
	// keepalive packet the stateful firewall RSTs us, the client sees
	// "peer close frame" and reconnects in a tight loop.
	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = conn.c.Ping(pctx)
				cancel()
			}
		}
	}()
	defer close(pingDone)

	// 2. Main loop.
	for {
		mt, payload, err := conn.read(ctx)
		if err != nil {
			s.cleanupPeer(peer, "disconnect")
			return err
		}
		switch mt {
		case websocket.MessageText:
			s.handleControl(ctx, conn, peer, payload)
		case websocket.MessageBinary:
			s.handleAudio(peer, payload)
		}
	}
}

func (s *Server) handleControl(ctx context.Context, conn *Conn, peer *room.Peer, payload []byte) {
	if !s.allowPeer(peer, "control", s.RateLimits.ControlPerPeerPerMinute, time.Minute) {
		s.sendError(conn, "", proto.ErrRateLimited, "control message rate exceeded")
		return
	}
	if !utf8.Valid(payload) {
		s.sendError(conn, "", proto.ErrProtocolError, "control message must be valid UTF-8")
		return
	}
	env, err := proto.Decode(payload)
	if err != nil {
		s.sendError(conn, "", proto.ErrProtocolError, "bad json")
		return
	}
	switch env.Type {
	case proto.TypeRoomCreate:
		s.handleRoomCreate(conn, peer, env)
	case proto.TypeRoomJoin:
		s.handleRoomJoin(conn, peer, env)
	case proto.TypeRoomLeave:
		s.handleRoomLeave(conn, peer, env)
	case proto.TypeRoomList:
		s.handleRoomList(conn, peer, env)
	case proto.TypePeerMute:
		s.handlePeerMute(conn, peer, env)
	case proto.TypeSubscribe:
		s.handleSubscribe(conn, peer, env)
	case proto.TypePing:
		s.handlePing(conn, env)
	default:
		s.sendError(conn, env.ID, proto.ErrProtocolError, "unknown type "+env.Type)
	}
	_ = ctx
}

func (s *Server) handleAudio(peer *room.Peer, payload []byte) {
	if !s.allowPeer(peer, "ws_audio", s.RateLimits.AudioFramesPerPeerPerSec, time.Second) {
		s.Metrics.LabeledCounter("nu_audio_frames_dropped_total", "reason").Inc("rate_limit")
		return
	}
	hdr, audio, err := proto.Unpack(payload)
	if err != nil {
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrProtocolError)
		return
	}
	rm := peer.CurrentRoom()
	if rm == nil {
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrNotInRoom)
		return
	}
	// Anti-spoof: source must be self.
	if hdr.SourcePeerID != [16]byte(peer.ID) {
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrProtocolError)
		return
	}
	if peer.Muted() {
		s.Metrics.LabeledCounter("nu_audio_frames_dropped_total", "reason").Inc("muted")
		return
	}

	// Copy payload because nhooyr reuses the read buffer.
	cp := make([]byte, len(payload))
	copy(cp, payload)

	frame := &room.Frame{
		SourcePeerID: peer.ID,
		Payload:      cp,
	}
	if !rm.Forward(frame) {
		s.Metrics.LabeledCounter("nu_audio_frames_dropped_total", "reason").Inc("backpressure")
		return
	}
	s.Metrics.Counter("nu_audio_frames_forwarded_total").Inc()
	s.Metrics.Counter("nu_audio_bytes_forwarded_total").Add(uint64(len(payload)))
	_ = audio
	_ = hdr
}

func (s *Server) handleRoomCreate(conn *Conn, peer *room.Peer, env proto.Envelope) {
	if !s.allowPeer(peer, "room_create", s.RateLimits.RoomCreatePerPeerPerMinute, time.Minute) {
		s.sendError(conn, env.ID, proto.ErrRateLimited, "room create rate exceeded")
		return
	}
	if peer.CurrentRoom() != nil {
		s.sendError(conn, env.ID, proto.ErrAlreadyInRoom, "leave first")
		return
	}
	var d proto.RoomCreateData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	if !validRoomName(d.RoomName) {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "roomName must be 1..64 Unicode characters without control characters")
		return
	}
	d.RoomName = strings.TrimSpace(d.RoomName)
	if len([]byte(d.Password)) > proto.MaxPasswordBytes {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "password exceeds bcrypt 72-byte limit")
		return
	}
	if d.Visibility != "public" && d.Visibility != "private" {
		d.Visibility = "private"
	}
	var passwordHash []byte
	if d.Password != "" {
		err := s.doPasswordWork(func() error {
			var hashErr error
			passwordHash, hashErr = room.HashPassword(d.Password)
			return hashErr
		})
		if errors.Is(err, errPasswordWorkBusy) {
			s.sendError(conn, env.ID, proto.ErrRateLimited, "password hashing busy")
			return
		}
		if err != nil {
			s.sendError(conn, env.ID, proto.ErrInternalError, "password hashing failed")
			return
		}
	}
	rm, err := s.Reg.Create(d.RoomName, d.Visibility, passwordHash)
	if err != nil {
		switch err {
		case room.ErrRoomNameTaken:
			s.sendError(conn, env.ID, proto.ErrRoomNameTaken, err.Error())
		case room.ErrServerFull:
			s.sendError(conn, env.ID, proto.ErrServerFull, err.Error())
		default:
			s.sendError(conn, env.ID, proto.ErrInternalError, err.Error())
		}
		return
	}
	s.Metrics.Counter("nu_room_create_total").Inc()
	s.Metrics.Gauge("nu_active_rooms").Set(int64(s.Reg.CountRooms()))

	// Auto-join the creator (role defaults to "send" — unimportant for control flow).
	s.joinPeerToRoom(conn, peer, rm, "send", env.ID)
}

func (s *Server) handleRoomJoin(conn *Conn, peer *room.Peer, env proto.Envelope) {
	if !s.allowPeer(peer, "room_join", s.RateLimits.RoomJoinPerPeerPerMinute, time.Minute) {
		s.sendError(conn, env.ID, proto.ErrRateLimited, "room join rate exceeded")
		return
	}
	if peer.CurrentRoom() != nil {
		s.sendError(conn, env.ID, proto.ErrAlreadyInRoom, "leave first")
		return
	}
	var d proto.RoomJoinData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	if !validRoomName(d.RoomName) {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "roomName must be 1..64 Unicode characters without control characters")
		return
	}
	d.RoomName = strings.TrimSpace(d.RoomName)
	if len([]byte(d.Password)) > proto.MaxPasswordBytes {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "password exceeds bcrypt 72-byte limit")
		return
	}
	rm := s.Reg.Find(d.RoomName)
	if rm == nil {
		s.sendError(conn, env.ID, proto.ErrRoomNotFound, "no such room")
		return
	}
	if rm.HasPassword {
		err := s.doPasswordWork(func() error { return rm.CheckPassword(d.Password) })
		if errors.Is(err, errPasswordWorkBusy) {
			s.sendError(conn, env.ID, proto.ErrRateLimited, "password verification busy")
			return
		}
		if err != nil {
			s.sendError(conn, env.ID, proto.ErrBadPassword, "bad password")
			return
		}
	}
	role := d.Role
	if role != "send" && role != "recv" {
		role = "send"
	}
	s.joinPeerToRoom(conn, peer, rm, role, env.ID)
}

func (s *Server) joinPeerToRoom(conn *Conn, peer *room.Peer, rm *room.Room, role, reqID string) {
	peer.Role = role
	others, err := rm.Add(peer)
	if err != nil {
		if errors.Is(err, room.ErrRoomNotFound) {
			s.sendError(conn, reqID, proto.ErrRoomNotFound, "room expired before join")
		} else {
			s.sendError(conn, reqID, proto.ErrRoomFull, "room full")
		}
		return
	}

	// Convert peer snapshots → wire PeerInfo.
	peers := make([]proto.PeerInfo, 0, len(others))
	for _, p := range others {
		peers = append(peers, proto.PeerInfo{
			PeerID:   p.ID.String(),
			Username: p.Username,
			Role:     p.Role,
			Muted:    p.Muted,
			JoinedAt: p.JoinedAt.UnixMilli(),
		})
	}

	_ = s.send(conn, proto.TypeRoomJoined, reqID, proto.RoomJoinedData{
		RoomID:   rm.ID.String(),
		RoomName: rm.Name,
		Peers:    peers,
	})

	// Notify other peers in the room.
	s.broadcastToOthers(rm, peer.ID, proto.TypePeerJoined, "", proto.PeerInfo{
		PeerID:   peer.ID.String(),
		Username: peer.Username,
		Role:     peer.Role,
		Muted:    peer.Muted(),
		JoinedAt: peer.JoinedAt.UnixMilli(),
	})

	s.Reg.PublishUpdate(rm)
}

func (s *Server) handleRoomLeave(conn *Conn, peer *room.Peer, env proto.Envelope) {
	rm := peer.CurrentRoom()
	if rm == nil {
		s.sendError(conn, env.ID, proto.ErrNotInRoom, "not in room")
		return
	}
	s.cleanupPeer(peer, "leave")
	_ = s.send(conn, proto.TypeRoomLeft, env.ID, proto.RoomLeftData{Reason: "leave"})
}

func (s *Server) handleRoomList(conn *Conn, peer *room.Peer, env proto.Envelope) {
	if !s.allowPeer(peer, "room_list", s.RateLimits.RoomListPerPeerPerMinute, time.Minute) {
		s.sendError(conn, env.ID, proto.ErrRateLimited, "room list rate exceeded")
		return
	}
	list := s.Reg.PublicList()
	wire := make([]proto.RoomListEntry, 0, len(list))
	for _, e := range list {
		wire = append(wire, proto.RoomListEntry{
			RoomName:    e.RoomName,
			PeerCount:   e.PeerCount,
			MaxPeers:    e.MaxPeers,
			HasPassword: e.HasPassword,
			CreatedAt:   e.CreatedAt.UnixMilli(),
		})
	}
	_ = s.send(conn, proto.TypeRoomListResult, env.ID, proto.RoomListResultData{Rooms: wire})
	_ = peer
}

func (s *Server) handlePeerMute(conn *Conn, peer *room.Peer, env proto.Envelope) {
	var d proto.PeerMuteData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	peer.SetMuted(d.Muted)
	if rm := peer.CurrentRoom(); rm != nil {
		s.broadcastToOthers(rm, peer.ID, proto.TypePeerMuteChanged, "", proto.PeerMuteChangedData{
			PeerID: peer.ID.String(),
			Muted:  d.Muted,
		})
	}
	_ = conn
}

func (s *Server) handleSubscribe(conn *Conn, peer *room.Peer, env proto.Envelope) {
	var d proto.SubscribeData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	ids, err := parseSubscriptionIDs(d.SourcePeerIDs)
	if err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, err.Error())
		return
	}
	peer.SetSubscribed(ids)
	_ = conn
}

func (s *Server) handlePing(conn *Conn, env proto.Envelope) {
	var d proto.PingData
	_ = json.Unmarshal(env.Data, &d)
	_ = s.send(conn, proto.TypePong, env.ID, proto.PongData{
		ClientTS: d.TS,
		ServerTS: time.Now().UnixMilli(),
	})
}

func (s *Server) cleanupPeer(peer *room.Peer, reason string) {
	rm := peer.CurrentRoom()
	if rm == nil {
		return
	}
	rm.Remove(peer.ID)
	s.Metrics.Gauge("nu_active_rooms").Set(int64(s.Reg.CountRooms()))
	s.broadcastToOthers(rm, peer.ID, proto.TypePeerLeft, "", proto.PeerLeftData{
		PeerID: peer.ID.String(),
		Reason: reason,
	})
	s.Reg.PublishUpdate(rm)
}

// broadcastToOthers sends a control message to every peer in the room except
// the source peer.
func (s *Server) broadcastToOthers(rm *room.Room, except uuid.UUID, typ, id string, payload any) {
	body, err := proto.Encode(typ, id, payload)
	if err != nil {
		return
	}
	rm.ForEachPeer(func(p *room.Peer) {
		if p.ID == except {
			return
		}
		_ = p.SendText(body)
	})
}

func (s *Server) send(conn *Conn, typ, id string, payload any) error {
	body, err := proto.Encode(typ, id, payload)
	if err != nil {
		return err
	}
	return conn.write(websocket.MessageText, body)
}

func (s *Server) sendError(conn *Conn, reqID, code, msg string) {
	_ = s.send(conn, proto.TypeError, reqID, proto.ErrorData{Code: code, Message: msg})
	s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(code)
	// Make sure the frame is actually flushed to the wire before the caller
	// returns and the deferred conn close fires. Without this, fatal-error
	// frames (BAD_USERNAME / SERVER_PASSWORD_REQUIRED / etc.) get queued but
	// the writeLoop is cancelled by the close(doneCh) before it can drain,
	// and the client just sees a bare "peer close frame" with no envelope.
	conn.flushWriteQueue()
}

func (s *Server) protoError(conn *Conn, reason string) error {
	s.sendError(conn, "", proto.ErrProtocolError, reason)
	return errors.New(reason)
}

func validUsername(u string) bool {
	return validHumanLabel(u, proto.MaxUsernameRunes)
}

func validRoomName(name string) bool {
	return validHumanLabel(name, proto.MaxRoomNameRunes)
}

func validHumanLabel(value string, maxRunes int) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	trimmed := strings.TrimSpace(value)
	n := utf8.RuneCountInString(trimmed)
	return n >= 1 && n <= maxRunes
}

func parseSubscriptionIDs(raw []string) ([]uuid.UUID, error) {
	if len(raw) > proto.MaxPeersPerRoom {
		return nil, fmt.Errorf("sourcePeerIds exceeds protocol limit of %d", proto.MaxPeersPerRoom)
	}
	ids := make([]uuid.UUID, 0, len(raw))
	seen := make(map[uuid.UUID]struct{}, len(raw))
	for _, text := range raw {
		id, err := uuid.Parse(text)
		if err != nil {
			return nil, errors.New("sourcePeerIds contains an invalid UUID")
		}
		if _, exists := seen[id]; exists {
			return nil, errors.New("sourcePeerIds contains a duplicate UUID")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Server) allowPeer(peer *room.Peer, action string, limit int, window time.Duration) bool {
	s.initSecurity()
	return s.peerLimiter.AllowPair(
		"peer:"+peer.ID.String()+":"+action,
		"ip:"+peer.RemoteIP+":"+action,
		limit,
		window,
	)
}

func (s *Server) doPasswordWork(fn func() error) error {
	s.initSecurity()
	select {
	case s.passwordSem <- struct{}{}:
		defer func() { <-s.passwordSem }()
		return fn()
	default:
		return errPasswordWorkBusy
	}
}
