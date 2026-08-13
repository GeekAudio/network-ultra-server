package ws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

func TestServerVersionUsesInjectedValue(t *testing.T) {
	s := &Server{Version: "1.3.7"}
	if got := s.version(); got != "1.3.7" {
		t.Fatalf("version=%q", got)
	}
	if got := (&Server{}).version(); got != "dev" {
		t.Fatalf("empty version should fail release gates as dev, got %q", got)
	}
}

func TestPasswordWorkFailsFastWhenConcurrencyIsSaturated(t *testing.T) {
	s := &Server{RateLimits: RateLimits{PasswordChecksConcurrent: 1}}
	s.initSecurity()
	s.passwordSem <- struct{}{}
	called := false
	err := s.doPasswordWork(func() error { called = true; return nil })
	<-s.passwordSem
	if !errors.Is(err, errPasswordWorkBusy) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestPeerRateLimitAlsoPersistsByRemoteIPAcrossReconnects(t *testing.T) {
	s := &Server{}
	a := room.NewPeer("a", "send")
	b := room.NewPeer("b", "send")
	a.RemoteIP, b.RemoteIP = "203.0.113.9", "203.0.113.9"
	if !s.allowPeer(a, "join", 1, time.Minute) {
		t.Fatal("first request denied")
	}
	if s.allowPeer(b, "join", 1, time.Minute) {
		t.Fatal("reconnect with new peer id bypassed IP budget")
	}
}

func TestPeerRateLimitIsPerActionAndPeer(t *testing.T) {
	s := &Server{}
	a := room.NewPeer("a", "send")
	b := room.NewPeer("b", "send")
	a.RemoteIP, b.RemoteIP = "203.0.113.10", "203.0.113.11"
	if !s.allowPeer(a, "create", 1, time.Minute) {
		t.Fatal("first request denied")
	}
	if s.allowPeer(a, "create", 1, time.Minute) {
		t.Fatal("second request should be denied")
	}
	if !s.allowPeer(a, "join", 1, time.Minute) || !s.allowPeer(b, "create", 1, time.Minute) {
		t.Fatal("actions and peers need independent budgets")
	}
}

func TestSharedIPRejectionDoesNotConsumeFreshPeerBudget(t *testing.T) {
	s := &Server{}
	a := room.NewPeer("a", "send")
	b := room.NewPeer("b", "send")
	a.RemoteIP, b.RemoteIP = "203.0.113.12", "203.0.113.12"
	if !s.allowPeer(a, "join", 1, time.Minute) {
		t.Fatal("first request denied")
	}
	if s.allowPeer(b, "join", 1, time.Minute) {
		t.Fatal("shared IP budget should reject the second peer")
	}
	b.RemoteIP = "203.0.113.13"
	if !s.allowPeer(b, "join", 1, time.Minute) {
		t.Fatal("rejected shared-IP attempt consumed the peer budget")
	}
}

func TestHandleHTTPRejectsCrossOriginButAllowsNativeClient(t *testing.T) {
	s := &Server{
		Reg:            room.NewRegistry(2, 2),
		Metrics:        metrics.NewRegistry(),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConnections: 4,
	}
	httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Dial the loopback TCP listener while presenting a matching attacker Host
	// and Origin. coder/websocket's same-host policy alone would accept this.
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, httpServer.Listener.Addr().String())
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	bad, response, err := websocket.Dial(ctx, "ws://attacker.example/", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
		Host:       "attacker.example",
		HTTPHeader: http.Header{"Origin": []string{"https://attacker.example"}},
	})
	if bad != nil {
		_ = bad.Close(websocket.StatusNormalClosure, "")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade: err=%v status=%v", err, response)
	}

	native, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("native client without Origin was rejected: %v", err)
	}
	_ = native.Close(websocket.StatusNormalClosure, "")
}

type failingUdpAdvertiser struct {
	mu          sync.Mutex
	attached    int
	detached    int
	mintCalls   int
	mintFailure error
}

func (f *failingUdpAdvertiser) MintToken(uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintCalls++
	return "", f.mintFailure
}

func (f *failingUdpAdvertiser) AttachPeer(*room.Peer) {
	f.mu.Lock()
	f.attached++
	f.mu.Unlock()
}

func (f *failingUdpAdvertiser) DetachPeer(*room.Peer) {
	f.mu.Lock()
	f.detached++
	f.mu.Unlock()
}

func TestUdpTokenMintFailureClosesSessionWithoutWelcome(t *testing.T) {
	udp := &failingUdpAdvertiser{mintFailure: errors.New("entropy unavailable")}
	s := &Server{
		Reg: room.NewRegistry(2, 2), Metrics: metrics.NewRegistry(),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConnections: 4,
		RateLimits:     RateLimits{HelloPerIPPerMinute: 10},
		Udp:            udp,
		UdpEndpoint:    "127.0.0.1:18902",
	}
	httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	hello, _ := proto.Encode(proto.TypeHello, "h", proto.HelloData{Username: "native"})
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, raw, err := c.Read(ctx); err == nil {
		t.Fatalf("mint failure leaked a welcome frame: %s", raw)
	}
	udp.mu.Lock()
	attached, detached, mintCalls := udp.attached, udp.detached, udp.mintCalls
	udp.mu.Unlock()
	if attached != 0 || detached != 0 || mintCalls != 1 {
		t.Fatalf("UDP lifecycle attached=%d detached=%d mint=%d", attached, detached, mintCalls)
	}
}

func TestMalformedControlFramesConsumeControlBudget(t *testing.T) {
	s := &Server{
		Reg:            room.NewRegistry(2, 2),
		Metrics:        metrics.NewRegistry(),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConnections: 4,
		RateLimits:     RateLimits{ControlPerPeerPerMinute: 1},
	}
	httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	hello, err := proto.Encode(proto.TypeHello, "hello", proto.HelloData{Username: "native"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err != nil { // welcome
		t.Fatal(err)
	}

	readErrorCode := func(payload []byte) string {
		t.Helper()
		if err := c.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		env, err := proto.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		var data proto.ErrorData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data.Code
	}
	if got := readErrorCode([]byte("not-json")); got != proto.ErrProtocolError {
		t.Fatalf("first malformed frame code=%q", got)
	}
	if got := readErrorCode([]byte("still-not-json")); got != proto.ErrRateLimited {
		t.Fatalf("malformed frame bypassed budget, code=%q", got)
	}
}

func TestSaturatedPasswordGateCoversServerAndRoomBcrypt(t *testing.T) {
	newServer := func(reg *room.Registry) *Server {
		return &Server{
			Reg: reg, Metrics: metrics.NewRegistry(),
			Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
			MaxConnections: 4,
			RateLimits: RateLimits{
				HelloPerIPPerMinute: 10, ControlPerPeerPerMinute: 10,
				RoomCreatePerPeerPerMinute: 10, RoomJoinPerPeerPerMinute: 10,
				PasswordChecksConcurrent: 1,
			},
		}
	}
	readCode := func(t *testing.T, c *websocket.Conn, ctx context.Context) string {
		t.Helper()
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		env, err := proto.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		var data proto.ErrorData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data.Code
	}

	t.Run("server password", func(t *testing.T) {
		s := newServer(room.NewRegistry(2, 2))
		hash, err := room.HashPassword("secret")
		if err != nil {
			t.Fatal(err)
		}
		s.ServerPasswordHash = hash
		s.initSecurity()
		s.passwordSem <- struct{}{}
		defer func() { <-s.passwordSem }()
		httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
		defer httpServer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		hello, _ := proto.Encode(proto.TypeHello, "h", proto.HelloData{Username: "native", ServerPassword: "secret"})
		if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
			t.Fatal(err)
		}
		if got := readCode(t, c, ctx); got != proto.ErrRateLimited {
			t.Fatalf("server bcrypt saturation code=%q", got)
		}
	})

	for _, tc := range []struct {
		name, typ string
		data      any
		prepare   func(t *testing.T, reg *room.Registry)
	}{
		{"room create", proto.TypeRoomCreate, proto.RoomCreateData{RoomName: "new", Password: "secret"}, nil},
		{"room join", proto.TypeRoomJoin, proto.RoomJoinData{RoomName: "protected", Password: "secret"}, func(t *testing.T, reg *room.Registry) {
			hash, err := room.HashPassword("secret")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reg.Create("protected", "private", hash); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := room.NewRegistry(3, 2)
			if tc.prepare != nil {
				tc.prepare(t, reg)
			}
			s := newServer(reg)
			httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(websocket.StatusNormalClosure, "")
			hello, _ := proto.Encode(proto.TypeHello, "h", proto.HelloData{Username: "native"})
			if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.Read(ctx); err != nil { // welcome
				t.Fatal(err)
			}
			s.passwordSem <- struct{}{}
			defer func() { <-s.passwordSem }()
			request, err := proto.Encode(tc.typ, "request", tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Write(ctx, websocket.MessageText, request); err != nil {
				t.Fatal(err)
			}
			if got := readCode(t, c, ctx); got != proto.ErrRateLimited {
				t.Fatalf("room bcrypt saturation code=%q", got)
			}
			if tc.typ == proto.TypeRoomCreate && reg.Find("new") != nil {
				t.Fatal("room was created without acquiring bcrypt budget")
			}
		})
	}
}
