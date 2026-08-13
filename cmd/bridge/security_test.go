package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
)

func TestBridgeHealthzHasStableIdentity(t *testing.T) {
	oldVersion, oldUpstream := bridgeVersion, upstreamGlobal
	bridgeVersion, upstreamGlobal = "1.3.0", "wss://audio.example"
	t.Cleanup(func() {
		bridgeVersion, upstreamGlobal = oldVersion, oldUpstream
	})

	recorder := httptest.NewRecorder()
	handleHealthz(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"ok": true, "component": "network-ultra-bridge",
		"protocol": "network-ultra-bridge/v1", "version": "1.3.0",
		"upstream": "wss://audio.example",
	} {
		if got[key] != want {
			t.Fatalf("%s=%v want %v", key, got[key], want)
		}
	}
	if pid, ok := got["pid"].(float64); !ok || pid < 1 {
		t.Fatalf("invalid pid: %v", got["pid"])
	}
}

func TestBridgeRejectsCrossOriginBeforeDialingUpstream(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePlugin(r.Context(), w, r, "wss://upstream.invalid", log)
	}))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://attacker.example"}},
	})
	if c != nil {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade: err=%v status=%v", err, response)
	}
}

func TestBridgeAllowsNativeClientWithoutOrigin(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePlugin(r.Context(), w, r, "wss://upstream.invalid", log)
	}))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, response, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("native client without Origin was rejected: err=%v status=%v", err, response)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func TestBridgeRejectsSameOriginBrowserClient(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlePlugin(r.Context(), w, r, "wss://upstream.invalid", log)
	}))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{httpServer.URL}},
	})
	if c != nil {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("same-origin browser upgrade: err=%v status=%v", err, response)
	}
}

func TestBridgeEndpointPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, listen, upstream string
		allow                  bool
		wantErr                bool
	}{
		{"secure", "127.0.0.1:18900", "wss://audio.example", false, false},
		{"trusted compatibility", "[::1]:18900", "ws://10.0.0.2:18900", true, false},
		{"implicit upstream", "127.0.0.1:18900", "", false, true},
		{"public bridge", "0.0.0.0:18900", "wss://audio.example", false, true},
		{"hostname bridge", "localhost:18900", "wss://audio.example", false, true},
		{"plaintext upstream", "127.0.0.1:18900", "ws://audio.example", false, true},
		{"credential in URL", "127.0.0.1:18900", "wss://user:pass@audio.example", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBridgeEndpoints(tc.listen, tc.upstream, tc.allow)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestWelcomeRewriterUsesAdvertisedCustomUDPEndpoint(t *testing.T) {
	upstream := listenTestUDP(t)
	peerID := uuid.New()
	token := testUDPToken()
	payload := welcomePayload(t, peerID, token, upstream.LocalAddr().String(), true)
	rewriter := &welcomeUDPRewriter{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		listenAddr: "127.0.0.1:0",
	}
	t.Cleanup(rewriter.close)

	out, handled, changed := rewriter.rewrite(payload)
	if !handled || !changed || rewriter.proxy == nil {
		t.Fatalf("handled=%v changed=%v proxy=%v", handled, changed, rewriter.proxy)
	}
	if !sameUDPAddr(rewriter.proxy.upstream, upstream.LocalAddr().(*net.UDPAddr)) {
		t.Fatalf("proxy upstream=%v want advertised custom endpoint %v",
			rewriter.proxy.upstream, upstream.LocalAddr())
	}
	data := decodeWelcomeData(t, out)
	if data["udpToken"] != base64.StdEncoding.EncodeToString(token[:]) {
		t.Fatalf("token changed: %v", data["udpToken"])
	}
	advertised, err := net.ResolveUDPAddr("udp", data["udpEndpoint"].(string))
	if err != nil || !advertised.IP.IsLoopback() || advertised.Port == upstream.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("rewritten endpoint=%v err=%v", data["udpEndpoint"], err)
	}
}

func TestWelcomeRewriterDisablesPartialOrInvalidUDPMetadata(t *testing.T) {
	peerID := uuid.New()
	token := testUDPToken()
	validToken := base64.StdEncoding.EncodeToString(token[:])
	for _, tc := range []struct {
		name string
		data map[string]any
	}{
		{"endpoint only", map[string]any{"peerId": peerID.String(), "udpEndpoint": "127.0.0.1:18902"}},
		{"token only", map[string]any{"peerId": peerID.String(), "udpToken": validToken}},
		{"bad token", map[string]any{"peerId": peerID.String(), "udpEndpoint": "127.0.0.1:18902", "udpToken": "not-base64"}},
		{"bad peer", map[string]any{"peerId": "not-a-uuid", "udpEndpoint": "127.0.0.1:18902", "udpToken": validToken}},
		{"bad endpoint", map[string]any{"peerId": peerID.String(), "udpEndpoint": "missing-port", "udpToken": validToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"type": "welcome", "v": 1, "data": tc.data})
			if err != nil {
				t.Fatal(err)
			}
			rewriter := &welcomeUDPRewriter{
				log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				listenAddr: "127.0.0.1:0",
			}
			out, handled, changed := rewriter.rewrite(payload)
			t.Cleanup(rewriter.close)
			if !handled || !changed || rewriter.proxy != nil {
				t.Fatalf("handled=%v changed=%v proxy=%v", handled, changed, rewriter.proxy)
			}
			data := decodeWelcomeData(t, out)
			if _, exists := data["udpEndpoint"]; exists {
				t.Fatalf("unsafe udpEndpoint remained: %v", data)
			}
			if _, exists := data["udpToken"]; exists {
				t.Fatalf("unsafe udpToken remained: %v", data)
			}
		})
	}
}

func TestWelcomeRewriterPreservesUDPDisabledContract(t *testing.T) {
	payload := []byte(`{"type":"welcome","v":1,"data":{"peerId":"peer","serverVersion":"1.3.0"}}`)
	rewriter := &welcomeUDPRewriter{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		listenAddr: "127.0.0.1:0",
	}
	out, handled, changed := rewriter.rewrite(payload)
	if !handled || changed || rewriter.proxy != nil || string(out) != string(payload) {
		t.Fatalf("handled=%v changed=%v proxy=%v out=%s", handled, changed, rewriter.proxy, out)
	}
}

func TestUDPProxyRequiresMatchingHelloAndWelcomeToBind(t *testing.T) {
	upstream := listenTestUDP(t)
	peerID := uuid.New()
	peer := [16]byte(peerID)
	token := testUDPToken()
	proxy := newTestUDPProxy(t, upstream, peer, token)
	plugin := listenTestUDP(t)
	proxyAddr := proxy.loopbackConn.LocalAddr().(*net.UDPAddr)

	if _, err := plugin.WriteToUDP([]byte("audio-before-hello"), proxyAddr); err != nil {
		t.Fatal(err)
	}
	expectUDPTimeout(t, upstream, "unauthenticated pre-hello packet")

	badToken := token
	badToken[0] ^= 0xff
	if _, err := plugin.WriteToUDP(proto.PackUdpHello(proto.UdpHelloFrame{PeerID: peer, Token: badToken}, nil), proxyAddr); err != nil {
		t.Fatal(err)
	}
	expectUDPTimeout(t, upstream, "hello with wrong token")

	if _, err := plugin.WriteToUDP(proto.PackUdpHello(proto.UdpHelloFrame{PeerID: peer, Token: token}, nil), proxyAddr); err != nil {
		t.Fatal(err)
	}
	_, proxyUpstreamAddr := readUDP(t, upstream)

	wrongPeer := peer
	wrongPeer[0] ^= 0xff
	if _, err := upstream.WriteToUDP(proto.PackUdpWelcome(wrongPeer, nil), proxyUpstreamAddr); err != nil {
		t.Fatal(err)
	}
	expectUDPTimeout(t, plugin, "welcome with wrong peer")

	welcome := proto.PackUdpWelcome(peer, nil)
	if _, err := upstream.WriteToUDP(welcome, proxyUpstreamAddr); err != nil {
		t.Fatal(err)
	}
	got, _ := readUDP(t, plugin)
	if string(got) != string(welcome) {
		t.Fatalf("welcome=%x want %x", got, welcome)
	}
}

func TestUDPProxyDropsUnexpectedUpstreamSource(t *testing.T) {
	proxy, upstream, plugin, proxyUpstreamAddr := establishTestUDPBinding(t)
	attacker := listenTestUDP(t)
	if _, err := attacker.WriteToUDP([]byte("attacker"), proxyUpstreamAddr); err != nil {
		t.Fatal(err)
	}
	expectUDPTimeout(t, plugin, "attacker-source upstream packet")

	legitimate := []byte("legitimate-server-packet")
	if _, err := upstream.WriteToUDP(legitimate, proxyUpstreamAddr); err != nil {
		t.Fatal(err)
	}
	got, _ := readUDP(t, plugin)
	if string(got) != string(legitimate) {
		t.Fatalf("got=%q want=%q", got, legitimate)
	}
	proxy.pluginAddrMu.RLock()
	bound := proxy.pluginAddr != nil
	proxy.pluginAddrMu.RUnlock()
	if !bound {
		t.Fatal("expected completed plugin binding")
	}
}

func TestUDPProxyRejectsDifferentLocalSourceAfterBinding(t *testing.T) {
	proxy, upstream, plugin, _ := establishTestUDPBinding(t)
	secondPlugin := listenTestUDP(t)
	proxyAddr := proxy.loopbackConn.LocalAddr().(*net.UDPAddr)
	hello := proto.PackUdpHello(proto.UdpHelloFrame{PeerID: proxy.expectedPeer, Token: proxy.expectedToken}, nil)
	if _, err := secondPlugin.WriteToUDP(hello, proxyAddr); err != nil {
		t.Fatal(err)
	}
	expectUDPTimeout(t, upstream, "takeover hello from different local source")

	marker := []byte("bound-plugin-packet")
	if _, err := plugin.WriteToUDP(marker, proxyAddr); err != nil {
		t.Fatal(err)
	}
	got, _ := readUDP(t, upstream)
	if string(got) != string(marker) {
		t.Fatalf("got=%q want=%q", got, marker)
	}
}

func establishTestUDPBinding(t *testing.T) (*udpProxy, *net.UDPConn, *net.UDPConn, *net.UDPAddr) {
	t.Helper()
	upstream := listenTestUDP(t)
	peerID := uuid.New()
	peer := [16]byte(peerID)
	token := testUDPToken()
	proxy := newTestUDPProxy(t, upstream, peer, token)
	plugin := listenTestUDP(t)
	hello := proto.PackUdpHello(proto.UdpHelloFrame{PeerID: peer, Token: token}, nil)
	if _, err := plugin.WriteToUDP(hello, proxy.loopbackConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	_, proxyUpstreamAddr := readUDP(t, upstream)
	welcome := proto.PackUdpWelcome(peer, nil)
	if _, err := upstream.WriteToUDP(welcome, proxyUpstreamAddr); err != nil {
		t.Fatal(err)
	}
	if got, _ := readUDP(t, plugin); string(got) != string(welcome) {
		t.Fatalf("welcome=%x want=%x", got, welcome)
	}
	return proxy, upstream, plugin, proxyUpstreamAddr
}

func newTestUDPProxy(t *testing.T, upstream *net.UDPConn, peer [16]byte,
	token [proto.UdpTokenSize]byte) *udpProxy {
	t.Helper()
	proxy, err := newUDPProxy(slog.New(slog.NewTextHandler(io.Discard, nil)),
		"127.0.0.1:0", upstream.LocalAddr().String(), peer, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.close)
	return proxy
}

func listenTestUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readUDP(t *testing.T, conn *net.UDPConn) ([]byte, *net.UDPAddr) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, udpDatagramMax)
	n, addr, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buf[:n]...), cloneUDPAddr(addr)
}

func expectUDPTimeout(t *testing.T, conn *net.UDPConn, description string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, udpDatagramMax)
	if n, src, err := conn.ReadFromUDP(buf); err == nil {
		t.Fatalf("%s was forwarded: n=%d src=%v payload=%x", description, n, src, buf[:n])
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("%s read error=%v, want timeout", description, err)
	}
}

func testUDPToken() [proto.UdpTokenSize]byte {
	var token [proto.UdpTokenSize]byte
	for i := range token {
		token[i] = byte(i + 1)
	}
	return token
}

func welcomePayload(t *testing.T, peerID uuid.UUID, token [proto.UdpTokenSize]byte,
	endpoint string, includeUDP bool) []byte {
	t.Helper()
	data := map[string]any{"peerId": peerID.String(), "serverVersion": "1.3.0"}
	if includeUDP {
		data["udpEndpoint"] = endpoint
		data["udpToken"] = base64.StdEncoding.EncodeToString(token[:])
	}
	payload, err := json.Marshal(map[string]any{"type": "welcome", "v": 1, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func decodeWelcomeData(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}
