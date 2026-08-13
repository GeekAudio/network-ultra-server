package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

func TestClientIPIgnoresForwardingHeadersFromUntrustedRemote(t *testing.T) {
	s := &Server{TrustedProxyPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	r := httptest.NewRequest(http.MethodGet, "http://audio.example/", nil)
	r.RemoteAddr = "198.51.100.44:54321"
	r.Header.Set("Forwarded", "for=203.0.113.8")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.clientIP(r); got != "198.51.100.44" {
		t.Fatalf("untrusted remote spoofed client identity: got %q", got)
	}
}

func TestClientIPUsesNearestUntrustedForwardedHop(t *testing.T) {
	s := &Server{TrustedProxyPrefixes: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}}
	r := httptest.NewRequest(http.MethodGet, "http://audio.example/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	// The leftmost value is attacker-supplied. A trusted proxy chain appended
	// the real client and nearest proxy, so the right-to-left walk must stop at
	// 198.51.100.7 and ignore the spoofed prefix.
	r.Header.Set("Forwarded", "for=203.0.113.66, for=198.51.100.7;proto=https, for=10.1.2.3")
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Fatalf("client IP=%q", got)
	}
}

func TestClientIPNormalizesIPv4AndIPv6(t *testing.T) {
	s := &Server{TrustedProxyPrefixes: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}}
	for _, tc := range []struct {
		name, remote, header, value, want string
	}{
		{"mapped IPv4", "127.0.0.1:1", "X-Forwarded-For", "::ffff:192.0.2.9", "192.0.2.9"},
		{"compressed IPv6", "[::1]:2", "Forwarded", `for="[2001:0db8:0:0::1]:4711"`, "2001:db8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://audio.example/", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set(tc.header, tc.value)
			if got := s.clientIP(r); got != tc.want {
				t.Fatalf("client IP=%q want %q", got, tc.want)
			}
		})
	}
}

func TestMalformedForwardedFailsClosedWithoutXFFFallback(t *testing.T) {
	s := &Server{TrustedProxyPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	r := httptest.NewRequest(http.MethodGet, "http://audio.example/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Forwarded", `for="unterminated`)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("malformed Forwarded should fall back to transport address, got %q", got)
	}
}

func TestTrustedProxyClientBudgetIsSharedAcrossConnectionsButSplitByClient(t *testing.T) {
	s := &Server{
		Reg:            room.NewRegistry(4, 2),
		Metrics:        metrics.NewRegistry(),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConnections: 4,
		RateLimits:     RateLimits{HelloPerIPPerMinute: 1},
		TrustedProxyPrefixes: []netip.Prefix{
			netip.MustParsePrefix("127.0.0.0/8"),
		},
	}
	httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	connect := func(t *testing.T, clientIP string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"X-Forwarded-For": []string{clientIP}},
		})
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
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		env, err := proto.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != proto.TypeError {
			return env.Type
		}
		var data proto.ErrorData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data.Code
	}

	if got := connect(t, "203.0.113.10"); got != proto.TypeWelcome {
		t.Fatalf("first client response=%q", got)
	}
	if got := connect(t, "203.0.113.10"); got != proto.ErrRateLimited {
		t.Fatalf("same client bypassed reconnect budget: %q", got)
	}
	if got := connect(t, "203.0.113.11"); got != proto.TypeWelcome {
		t.Fatalf("different proxied client shared budget: %q", got)
	}
}
