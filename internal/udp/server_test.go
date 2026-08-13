package udp

import (
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry(), Limits{
		HelloPerIPPerMinute: 10, AudioFramesPerPeerPerSec: 10,
	})
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	s.conn = c
	t.Cleanup(func() { _ = c.Close() })
	return s
}

func helloPacket(t *testing.T, p *room.Peer, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var token [proto.UdpTokenSize]byte
	copy(token[:], raw)
	return proto.PackUdpHello(proto.UdpHelloFrame{PeerID: [16]byte(p.ID), Token: token}, nil)
}

func mintToken(t *testing.T, s *Server, peerID uuid.UUID) string {
	t.Helper()
	token, err := s.MintToken(peerID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestHelloTokenCannotRebindPeerToNewEndpoint(t *testing.T) {
	s := testServer(t)
	p := room.NewPeer("peer", "send")
	s.AttachPeer(p)
	pkt := helloPacket(t, p, mintToken(t, s, p.ID))
	first := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30001}
	second := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30002}
	s.handleHello(pkt, first)
	s.handleHello(pkt, first) // lost welcome retry is allowed
	s.handleHello(pkt, second)
	if got := p.UdpAddr(); got == nil || got.String() != first.String() {
		t.Fatalf("binding changed: got %v want %v", got, first)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.peerByAddr[second.String()] != nil {
		t.Fatal("captured token rebound peer to attacker endpoint")
	}
}

func TestDetachInvalidatesToken(t *testing.T) {
	s := testServer(t)
	p := room.NewPeer("peer", "send")
	s.AttachPeer(p)
	pkt := helloPacket(t, p, mintToken(t, s, p.ID))
	s.DetachPeer(p)
	s.handleHello(pkt, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30003})
	if p.UdpAddr() != nil {
		t.Fatal("detached peer accepted old token")
	}
}

func TestInvalidHelloDoesNotCreateOrConsumeAuthenticatedBudget(t *testing.T) {
	s := testServer(t)
	s.limits.HelloPerIPPerMinute = 1
	p := room.NewPeer("peer", "send")
	s.AttachPeer(p)
	encoded := mintToken(t, s, p.ID)
	valid := helloPacket(t, p, encoded)
	invalid := append([]byte(nil), valid...)
	invalid[len(invalid)-1] ^= 0xff
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30004}
	s.handleHello(invalid, src)
	s.handleHello(valid, src)
	if got := p.UdpAddr(); got == nil || got.String() != src.String() {
		t.Fatalf("invalid unauthenticated hello consumed the peer budget: got %v", got)
	}
}

func TestIdleBindingExpiresAndPeerFallsBackToWebSocket(t *testing.T) {
	s := testServer(t)
	now := time.Unix(100, 0)
	s.now = func() time.Time { return now }
	p := room.NewPeer("peer", "recv")
	s.AttachPeer(p)
	pkt := helloPacket(t, p, mintToken(t, s, p.ID))
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30100}
	s.handleHello(pkt, src)

	var wsSends atomic.Int32
	p.AttachSender(func(_ []byte, binary bool) error {
		if binary {
			wsSends.Add(1)
		}
		return nil
	})
	if err := p.SendBinary([]byte("before-expiry")); err != nil {
		t.Fatal(err)
	}
	if wsSends.Load() != 0 {
		t.Fatal("live UDP binding unexpectedly used WebSocket")
	}

	now = now.Add(peerIdleTimeout)
	s.expireIdle(now)
	if p.UdpAddr() != nil {
		t.Fatalf("expired binding remained: %v", p.UdpAddr())
	}
	if err := p.SendBinary([]byte("after-expiry")); err != nil {
		t.Fatal(err)
	}
	if wsSends.Load() != 1 {
		t.Fatalf("WS fallback sends=%d want 1", wsSends.Load())
	}
}

func TestValidPingRefreshesBindingLease(t *testing.T) {
	s := testServer(t)
	now := time.Unix(200, 0)
	s.now = func() time.Time { return now }
	p := room.NewPeer("peer", "recv")
	s.AttachPeer(p)
	pkt := helloPacket(t, p, mintToken(t, s, p.ID))
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30101}
	s.handleHello(pkt, src)

	now = now.Add(10 * time.Second)
	s.handlePing(proto.PackUdpPing([16]byte(p.ID), nil), src)
	now = now.Add(10 * time.Second)
	s.expireIdle(now)
	if got := p.UdpAddr(); got == nil || got.String() != src.String() {
		t.Fatalf("active ping binding expired: %v", got)
	}
}

func TestStaleExpiryCannotClearNewBinding(t *testing.T) {
	s := testServer(t)
	p := room.NewPeer("peer", "recv")
	s.AttachPeer(p)
	oldAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30102}
	newAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30103}
	oldBinding := &binding{peer: p, addr: oldAddr, lastSeen: time.Unix(100, 0)}
	newBinding := &binding{peer: p, addr: newAddr, lastSeen: time.Unix(200, 0)}
	s.mu.Lock()
	s.peerByAddr[oldAddr.String()] = oldBinding
	s.peerByAddr[newAddr.String()] = newBinding
	p.SetUdpAddr(newAddr)
	s.mu.Unlock()

	s.expireIdle(time.Unix(200, 0))
	if got := p.UdpAddr(); got == nil || got.String() != newAddr.String() {
		t.Fatalf("stale expiry cleared new binding: %v", got)
	}
	s.mu.RLock()
	gotBinding := s.peerByAddr[newAddr.String()]
	s.mu.RUnlock()
	if gotBinding != newBinding {
		t.Fatal("new binding removed by stale expiry")
	}
}

func TestDetachRemovesLeaseAndStaleSweepIsHarmless(t *testing.T) {
	s := testServer(t)
	now := time.Unix(300, 0)
	s.now = func() time.Time { return now }
	p := room.NewPeer("peer", "recv")
	s.AttachPeer(p)
	pkt := helloPacket(t, p, mintToken(t, s, p.ID))
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30104}
	s.handleHello(pkt, src)
	s.DetachPeer(p)
	now = now.Add(peerIdleTimeout * 2)
	s.expireIdle(now)
	if p.UdpAddr() != nil {
		t.Fatalf("detached peer retained endpoint: %v", p.UdpAddr())
	}
	s.mu.RLock()
	_, exists := s.peerByAddr[src.String()]
	s.mu.RUnlock()
	if exists {
		t.Fatal("detached lease remained in address map")
	}
}

func TestExpiredTokenCannotRebindAfterLeaseExpiry(t *testing.T) {
	s := testServer(t)
	now := time.Unix(400, 0)
	s.now = func() time.Time { return now }
	p := room.NewPeer("peer", "recv")
	s.AttachPeer(p)
	oldToken := mintToken(t, s, p.ID)
	oldHello := helloPacket(t, p, oldToken)
	oldAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30105}
	s.handleHello(oldHello, oldAddr)
	now = now.Add(peerIdleTimeout)
	s.expireIdle(now)

	attackerAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30106}
	s.handleHello(oldHello, attackerAddr)
	if p.UdpAddr() != nil {
		t.Fatalf("expired captured token rebound endpoint: %v", p.UdpAddr())
	}
	newToken := mintToken(t, s, p.ID)
	s.handleHello(helloPacket(t, p, newToken), oldAddr)
	if got := p.UdpAddr(); got == nil || got.String() != oldAddr.String() {
		t.Fatalf("fresh token did not re-establish endpoint: %v", got)
	}
}

func TestMintTokenEntropyFailureReturnsErrorWithoutStateLeak(t *testing.T) {
	s := testServer(t)
	p := room.NewPeer("peer", "recv")
	s.AttachPeer(p)
	s.randReader = errorReader{err: errors.New("entropy unavailable")}

	token, err := s.MintToken(p.ID)
	if err == nil || token != "" {
		t.Fatalf("MintToken() token=%q err=%v", token, err)
	}
	s.mu.RLock()
	_, tokenExists := s.tokenByID[p.ID]
	bindingCount := len(s.peerByAddr)
	s.mu.RUnlock()
	if tokenExists || bindingCount != 0 || p.UdpAddr() != nil {
		t.Fatalf("failed mint leaked state: token=%v bindings=%d addr=%v", tokenExists, bindingCount, p.UdpAddr())
	}
}

func TestMutedSourceUDPAudioIsDroppedAndUnmuteRestores(t *testing.T) {
	s := testServer(t)
	reg := room.NewRegistry(2, 8)
	rm, err := reg.Create("mute-udp", "private", nil)
	if err != nil {
		t.Fatal(err)
	}
	source := room.NewPeer("source", "send")
	receiver := room.NewPeer("receiver", "recv")
	if _, err := rm.Add(source); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.Add(receiver); err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 4)
	receiver.AttachSender(func(payload []byte, binary bool) error {
		if binary {
			received <- append([]byte(nil), payload...)
		}
		return nil
	})

	s.AttachPeer(source)
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30200}
	s.handleHello(helloPacket(t, source, mintToken(t, s, source.ID)), src)
	pkt, err := proto.Pack(proto.AudioFrameHeader{
		SourcePeerID: [16]byte(source.ID),
		Seq:          1,
	}, []byte("audio"), nil)
	if err != nil {
		t.Fatal(err)
	}

	source.SetMuted(true)
	s.handleAudio(pkt, src)
	assertUDPForwarderBarrier(t, rm, received)

	source.SetMuted(false)
	s.handleAudio(pkt, src)
	select {
	case got := <-received:
		if string(got) != string(pkt) {
			t.Fatalf("unmuted payload=%x want=%x", got, pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("unmuted UDP audio did not resume")
	}

	var rendered strings.Builder
	s.Metrics.WriteText(&rendered)
	if !strings.Contains(rendered.String(), `nu_audio_frames_dropped_total{reason="muted"} 1`) {
		t.Fatalf("missing muted drop metric:\n%s", rendered.String())
	}
}

func assertUDPForwarderBarrier(t *testing.T, rm *room.Room, received <-chan []byte) {
	t.Helper()
	done := make(chan struct{})
	if !rm.Forward(&room.Frame{
		// A missing source is intentionally fail-closed. Done is the FIFO
		// barrier proving all earlier frames have been consumed.
		Done: func() { close(done) },
	}) {
		t.Fatal("could not enqueue forwarder barrier")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("room forwarder barrier timed out")
	}
	select {
	case got := <-received:
		t.Fatalf("muted audio escaped before barrier: %x", got)
	default:
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
