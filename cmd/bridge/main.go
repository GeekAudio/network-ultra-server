// network-ultra-bridge — local relay for DAW hosts whose outbound is firewalled.
//
//	Studio One ──► VST plugin ──ws://127.0.0.1:18900──► Bridge ──► remote server
//	                                                    │
//	                           ──udp://127.0.0.1:18902──┤
//	                                                    └──udp://server:18902
//
// Why we proxy BOTH WS and UDP:
//   - The plugin runs inside the DAW, which is often blocked outbound by
//     Windows Firewall (cracked Studio One on China retail builds is the
//     canonical case). The plugin can only reach 127.0.0.1.
//   - The bridge is a normal Windows app — not blocked. It does the heavy
//     lifting on both control plane (WebSocket) and data plane (UDP audio).
//
// Wire-level behaviour:
//   - Control: byte-for-byte WS relay, EXCEPT we validate the JSON `welcome`
//     frame coming back from the server and rewrite its advertised UDP endpoint
//     to an ephemeral loopback listener owned by this bridge connection.
//   - Audio out: the plugin's authenticated UDP hello binds one loopback source
//     address, then packets from only that address are forwarded to the exact
//     UDP endpoint advertised by the server.
//   - Audio in: only datagrams from that resolved upstream IP:port are accepted,
//     and the matching UDP welcome must complete the binding before packets are
//     forwarded back to the plugin.
//
// Lifecycle:
//   - Startup never terminates an existing listener by default. A busy listen
//     address makes this process exit non-zero so the caller can decide which
//     upstream/session owns the port. Legacy stale-listener migration requires
//     the explicit -reclaim-stale-listener operator opt-in.
//   - /healthz exposes upstream URL + pid + started timestamp + version
//     so a fresh plugin connect can verify the bridge is healthy AND
//     pointing at the right server before reusing it.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
)

const (
	subprotocol         = "network-ultra-v1"
	upstreamDialTimeout = 15 * time.Second
	udpDatagramMax      = 9000
)

// Overridden by release builds with -ldflags "-X main.bridgeVersion=<semver>".
var bridgeVersion = "dev"

// startedAt is captured once and reported via /healthz so callers can tell
// how long the bridge has been alive (useful for diagnosing stuck bridges).
var startedAt = time.Now()

// upstreamGlobal is set in run() and read by /healthz. Plain string is fine —
// it never changes after startup.
var upstreamGlobal string

// udpProxy bridges plugin UDP traffic to the remote server.
//
// We use TWO sockets because a single socket bound to 127.0.0.1 can never
// receive packets from the public internet (kernel routes them to a
// 0.0.0.0-bound socket, not to 127.0.0.1). The two-socket design:
//
//	loopbackConn  : bound to 127.0.0.1:18902
//	                receives from plugin, sends back to plugin
//
//	upstreamConn  : bound to 0.0.0.0:0  (ephemeral)
//	                sends to remote server, receives replies, forwards back
//	                to plugin via loopbackConn
//
// The plugin's source address is accepted only after a UDP hello matching the
// peer ID and opaque token from the WebSocket welcome. The final binding is not
// activated until the configured upstream endpoint replies with a matching UDP
// welcome. This prevents unrelated loopback processes and spoofed upstream
// datagrams from taking over a session.
type udpProxy struct {
	log *slog.Logger

	loopbackConn  *net.UDPConn // 127.0.0.1:18902
	upstreamConn  *net.UDPConn // 0.0.0.0:ephemeral
	upstream      *net.UDPAddr // server's UDP host:port
	expectedPeer  [16]byte
	expectedToken [proto.UdpTokenSize]byte

	pluginAddrMu sync.RWMutex
	pendingAddr  *net.UDPAddr
	pluginAddr   *net.UDPAddr

	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newUDPProxy(log *slog.Logger, listenAddr, upstreamAddr string, peerID [16]byte,
	token [proto.UdpTokenSize]byte) (*udpProxy, error) {
	la, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen %s: %w", listenAddr, err)
	}
	ua, err := net.ResolveUDPAddr("udp", upstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream %s: %w", upstreamAddr, err)
	}
	if la.IP == nil || !la.IP.IsLoopback() {
		return nil, errors.New("udp proxy listen address must be an explicit loopback IP")
	}
	if ua.IP == nil || ua.Port < 1 {
		return nil, errors.New("udp upstream must resolve to a non-zero IP:port")
	}
	loopback, err := net.ListenUDP("udp", la)
	if err != nil {
		return nil, fmt.Errorf("listen loopback %s: %w", listenAddr, err)
	}
	upstreamNetwork := "udp4"
	upstreamLocalIP := net.IPv4zero
	if ua.IP.To4() == nil {
		upstreamNetwork = "udp6"
		upstreamLocalIP = net.IPv6unspecified
	}
	upstreamConn, err := net.ListenUDP(upstreamNetwork, &net.UDPAddr{IP: upstreamLocalIP, Port: 0})
	if err != nil {
		_ = loopback.Close()
		return nil, fmt.Errorf("listen upstream socket: %w", err)
	}
	for _, c := range []*net.UDPConn{loopback, upstreamConn} {
		_ = c.SetReadBuffer(4 * 1024 * 1024)
		_ = c.SetWriteBuffer(4 * 1024 * 1024)
	}

	p := &udpProxy{
		log:           log,
		loopbackConn:  loopback,
		upstreamConn:  upstreamConn,
		upstream:      cloneUDPAddr(ua),
		expectedPeer:  peerID,
		expectedToken: token,
	}
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.loopFromPlugin()
	}()
	go func() {
		defer p.wg.Done()
		p.loopFromUpstream()
	}()
	return p, nil
}

// loopFromPlugin accepts exactly one local source. Before it is bound, only a
// hello carrying this WebSocket session's peer ID and token is forwarded.
func (p *udpProxy) loopFromPlugin() {
	buf := make([]byte, udpDatagramMax)
	for {
		n, src, err := p.loopbackConn.ReadFromUDP(buf)
		if err != nil {
			if !errIsUseOfClosed(err) {
				p.log.Debug("udp loopback read error", "err", err)
			}
			return
		}
		p.pluginAddrMu.Lock()
		bound := p.pluginAddr
		pending := p.pendingAddr
		if bound != nil {
			if !sameUDPAddr(src, bound) {
				p.pluginAddrMu.Unlock()
				p.log.Debug("udp datagram from different local source dropped", "source", src)
				continue
			}
		} else {
			hello, helloErr := proto.UnpackUdpHello(buf[:n])
			if helloErr != nil || !sameFixedBytes(hello.PeerID[:], p.expectedPeer[:]) ||
				!sameFixedBytes(hello.Token[:], p.expectedToken[:]) {
				p.pluginAddrMu.Unlock()
				p.log.Debug("unauthenticated local udp datagram dropped", "source", src)
				continue
			}
			if pending != nil && !sameUDPAddr(src, pending) {
				p.pluginAddrMu.Unlock()
				p.log.Debug("udp hello from different local source dropped", "source", src)
				continue
			}
			if pending == nil {
				p.pendingAddr = cloneUDPAddr(src)
			}
		}
		p.pluginAddrMu.Unlock()

		if _, err := p.upstreamConn.WriteToUDP(buf[:n], p.upstream); err != nil {
			p.log.Debug("udp upstream write error", "err", err)
		}
	}
}

// loopFromUpstream rejects packets from every source except the resolved
// endpoint advertised in the WebSocket welcome. The first accepted packet must
// be a UDP welcome for the expected peer, which completes the local binding.
func (p *udpProxy) loopFromUpstream() {
	buf := make([]byte, udpDatagramMax)
	for {
		n, src, err := p.upstreamConn.ReadFromUDP(buf)
		if err != nil {
			if !errIsUseOfClosed(err) {
				p.log.Debug("udp upstream read error", "err", err)
			}
			return
		}
		if !sameUDPAddr(src, p.upstream) {
			p.log.Debug("udp datagram from unexpected upstream source dropped", "source", src)
			continue
		}

		p.pluginAddrMu.Lock()
		dst := p.pluginAddr
		if dst == nil {
			if n != proto.UdpWelcomeSize || buf[0] != proto.UdpFrameTypeWelcome ||
				!sameFixedBytes(buf[4:20], p.expectedPeer[:]) || p.pendingAddr == nil {
				p.pluginAddrMu.Unlock()
				p.log.Debug("unexpected udp packet before binding dropped", "source", src)
				continue
			}
			dst = cloneUDPAddr(p.pendingAddr)
			p.pluginAddr = dst
			p.pendingAddr = nil
		}
		p.pluginAddrMu.Unlock()
		if _, err := p.loopbackConn.WriteToUDP(buf[:n], dst); err != nil {
			p.log.Debug("udp loopback write error", "err", err)
		}
	}
}

func (p *udpProxy) close() {
	p.closeOnce.Do(func() {
		if p.loopbackConn != nil {
			_ = p.loopbackConn.Close()
		}
		if p.upstreamConn != nil {
			_ = p.upstreamConn.Close()
		}
		p.wg.Wait()
	})
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}
}

func sameUDPAddr(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.Zone == b.Zone && a.IP.Equal(b.IP)
}

func sameFixedBytes(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

// errIsUseOfClosed checks for the "use of closed network connection" error.
func errIsUseOfClosed(err error) bool {
	if err == nil {
		return false
	}
	// Best-effort string match; net package does not export the sentinel.
	return err.Error() == "use of closed network connection" ||
		(net.ErrClosed != nil && err == net.ErrClosed)
}

func main() {
	os.Exit(run())
}

func run() int {
	showVersion := flag.Bool("version", false, "print machine-readable semver and exit")
	listen := flag.String("listen", "127.0.0.1:18900",
		"Local address to accept plugin connections on")
	upstream := flag.String("upstream", "", "Remote Network Ultra Server wss:// URL (required)")
	allowInsecureUpstream := flag.Bool("allow-insecure-upstream", false,
		"allow ws:// only on a trusted network; this does not encrypt UDP")
	reclaimStaleListener := flag.Bool("reclaim-stale-listener", false,
		"explicitly terminate an exact-path stale bridge holding -listen (migration only)")
	logLevel := flag.String("log-level", "info", "debug | info | warn | error")
	flag.Parse()
	if *showVersion {
		fmt.Println(bridgeVersion)
		return 0
	}
	if err := validateBridgeEndpoints(*listen, *upstream, *allowInsecureUpstream); err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}

	log := setupLogger(*logLevel)
	upstreamGlobal = *upstream
	log.Info("starting", "version", bridgeVersion, "ws-listen", *listen, "upstream", *upstream)

	if err := maybeReclaimStaleListener(*reclaimStaleListener, *listen, log); err != nil {
		log.Error("explicit stale-listener reclaim failed", "err", err, "addr", *listen)
		return 2
	}

	// Each plugin connection gets its own UDP proxy (ephemeral loopback +
	// upstream sockets), so multiple plugin instances on the same DAW —
	// e.g. one Send, one Recv — can each have an independent UDP binding
	// on the server. A single shared proxy would alias them: server would
	// bind only the latest hello's source addr and the others would
	// silently drop to WS fallback.

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /healthz registered above takes precedence by net/http rules.
		// Anything else is a WS upgrade attempt from the plugin.
		handlePlugin(r.Context(), w, r, *upstream, log)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	return serveBridge(srv, stop, log)
}

// maybeReclaimStaleListener isolates the legacy migration capability behind
// an explicit operator decision. The default path returns without probing the
// address or inspecting/terminating any process; serveBridge then performs the
// authoritative bind and returns non-zero if another session owns it.
func maybeReclaimStaleListener(enabled bool, listen string, log *slog.Logger) error {
	if !enabled {
		return nil
	}
	if !portIsFree(listen) {
		log.Warn("listen port busy, attempting explicit reclaim", "addr", listen)
		if err := reclaimPort(listen, log); err != nil {
			return err
		}
		// Give Windows a moment to actually free the TCP socket after kill.
		for i := 0; i < 20 && !portIsFree(listen); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if !portIsFree(listen) {
			return errors.New("listen address still busy after explicit reclaim attempt")
		}
		log.Info("port reclaimed successfully", "addr", listen)
	}
	return nil
}

func serveBridge(srv *http.Server, stop <-chan os.Signal, log *slog.Logger) int {
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Error("listen error", "err", err)
		return 1
	}

	ready := make(chan struct{})
	listener = &acceptReadyListener{Listener: listener, ready: ready}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()
	// Do not let an already-buffered termination signal overtake Serve startup.
	// Shutdown called before Serve enters Accept can otherwise return while a
	// listener goroutine starts afterwards and survives main.
	select {
	case <-ready:
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("serve error", "err", serveErr)
			return 1
		}
		return 0
	}

	exitCode, sig, serveErr := waitForBridgeTermination(stop, errCh)
	if sig != nil {
		log.Info("shutting down", "signal", sig.String())
	} else if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Error("listen error", "err", serveErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", "err", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	log.Info("bye")
	return exitCode
}

type acceptReadyListener struct {
	net.Listener
	ready chan struct{}
	once  sync.Once
}

func (l *acceptReadyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.ready) })
	return l.Listener.Accept()
}

func waitForBridgeTermination(stop <-chan os.Signal, errCh <-chan error) (int, os.Signal, error) {
	select {
	case sig := <-stop:
		return 0, sig, nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return 1, nil, err
		}
		return 0, nil, err
	}
}

// handleHealthz responds with a small JSON snapshot used by the plugin to
// decide whether the bridge is healthy and pointing at the right upstream.
// Plain HTTP, no auth — bound to loopback only.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	resp := map[string]any{
		"ok":        true,
		"component": "network-ultra-bridge",
		"protocol":  "network-ultra-bridge/v1",
		"version":   bridgeVersion,
		"pid":       os.Getpid(),
		"upstream":  upstreamGlobal,
		"startedAt": startedAt.UTC().Format(time.RFC3339),
		"uptimeMs":  time.Since(startedAt).Milliseconds(),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func handlePlugin(parent context.Context, w http.ResponseWriter, r *http.Request,
	upstreamURL string, log *slog.Logger) {
	// The bridge is a native-plugin endpoint, not a browser API. Same-origin is
	// not sufficient because a loopback/DNS-rebinding Host can make an attacker
	// origin appear same-host to the WebSocket library.
	for _, origin := range r.Header.Values("Origin") {
		if strings.TrimSpace(origin) != "" {
			http.Error(w, "browser websocket origins are forbidden", http.StatusForbidden)
			return
		}
	}
	plugin, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{subprotocol},
	})
	if err != nil {
		log.Warn("plugin accept failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	defer plugin.Close(websocket.StatusInternalError, "shutting down")
	plugin.SetReadLimit(64 * 1024)

	log.Info("plugin connected", "remote", r.RemoteAddr)

	upstreamHost := extractHostFromWsURL(upstreamURL)

	dialCtx, dialCancel := context.WithTimeout(parent, upstreamDialTimeout)
	defer dialCancel()

	directHTTP := &http.Client{
		Transport: &http.Transport{Proxy: nil},
	}

	dialHeaders := http.Header{}
	if upstreamHost != "" {
		dialHeaders.Set("Host", upstreamHost)
	}

	upstream, _, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{
		Subprotocols: []string{subprotocol},
		HTTPClient:   directHTTP,
		HTTPHeader:   dialHeaders,
		Host:         upstreamHost,
	})
	if err != nil {
		log.Warn("upstream dial failed", "err", err, "url", upstreamURL)
		_ = plugin.Close(websocket.StatusGoingAway, "upstream unavailable")
		return
	}
	defer upstream.Close(websocket.StatusInternalError, "shutting down")
	upstream.SetReadLimit(64 * 1024)

	log.Info("upstream connected", "remote", r.RemoteAddr, "upstream", upstreamURL)

	relayCtx, relayCancel := context.WithCancel(parent)
	defer relayCancel()

	var sniffOnce atomic.Bool
	rewriter := &welcomeUDPRewriter{log: log, listenAddr: "127.0.0.1:0"}
	defer rewriter.close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relayPassthrough(relayCtx, "plugin->upstream", plugin, upstream, log)
		relayCancel()
	}()
	go func() {
		defer wg.Done()
		relayWithRewrite(relayCtx, "upstream->plugin", upstream, plugin, rewriter, &sniffOnce, log)
		relayCancel()
	}()
	wg.Wait()

	log.Info("session closed", "remote", r.RemoteAddr)
}

// relayPassthrough copies WS messages src→dst with no rewriting.
func relayPassthrough(ctx context.Context, dir string, src, dst *websocket.Conn, log *slog.Logger) {
	for {
		mt, r, err := src.Reader(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Debug(dir+" reader closed", "err", err)
			}
			return
		}
		w, err := dst.Writer(ctx, mt)
		if err != nil {
			log.Debug(dir+" writer open failed", "err", err)
			return
		}
		if _, err := io.Copy(w, r); err != nil {
			log.Debug(dir+" copy failed", "err", err)
			_ = w.Close()
			return
		}
		if err := w.Close(); err != nil {
			log.Debug(dir+" writer close failed", "err", err)
			return
		}
	}
}

// relayWithRewrite copies WS messages src→dst. For the first welcome it asks
// welcomeUDPRewriter to create a per-session UDP proxy from the endpoint,
// token, and peer ID advertised by the real server. Invalid or partial UDP
// metadata is stripped so the plugin safely falls back to WebSocket audio.
func relayWithRewrite(ctx context.Context, dir string, src, dst *websocket.Conn,
	rewriter *welcomeUDPRewriter, sniffOnce *atomic.Bool, log *slog.Logger) {
	for {
		mt, payload, err := src.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Debug(dir+" read closed", "err", err)
			}
			return
		}

		if mt == websocket.MessageText && !sniffOnce.Load() {
			if rewritten, handled, changed := rewriter.rewrite(payload); handled {
				payload = rewritten
				sniffOnce.Store(true)
				if rewriter.proxy != nil {
					log.Info("welcome udpEndpoint rewritten",
						"advertise", rewriter.proxy.loopbackConn.LocalAddr().String(),
						"upstream", rewriter.proxy.upstream)
				} else if changed {
					log.Warn("invalid or partial welcome UDP metadata stripped; using WebSocket fallback")
				}
			}
		}

		if err := dst.Write(ctx, mt, payload); err != nil {
			log.Debug(dir+" write failed", "err", err)
			return
		}
	}
}

type welcomeUDPRewriter struct {
	log        *slog.Logger
	listenAddr string
	proxy      *udpProxy
}

// rewrite preserves unknown welcome fields, but advertises a local UDP proxy
// only when endpoint, token, and peer ID form one complete valid session.
// Returns payload, whether a welcome was handled, and whether its JSON changed.
func (r *welcomeUDPRewriter) rewrite(payload []byte) ([]byte, bool, bool) {
	// Use map[string]any to keep all unknown fields verbatim.
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		return payload, false, false
	}
	if t, _ := env["type"].(string); t != "welcome" {
		return payload, false, false
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		return payload, true, false
	}

	epValue, epExists := data["udpEndpoint"]
	tokenValue, tokenExists := data["udpToken"]
	if !epExists && !tokenExists {
		return payload, true, false
	}
	ep, epOK := epValue.(string)
	tokenText, tokenOK := tokenValue.(string)
	peerText, peerOK := data["peerId"].(string)
	rawToken, tokenErr := base64.StdEncoding.DecodeString(tokenText)
	peerUUID, peerErr := uuid.Parse(peerText)
	validToken := tokenOK && tokenErr == nil && len(rawToken) == proto.UdpTokenSize &&
		base64.StdEncoding.EncodeToString(rawToken) == tokenText
	if !epOK || ep == "" || !validToken || !peerOK || peerErr != nil {
		return stripWelcomeUDP(env, data, payload), true, true
	}

	var peerID [16]byte
	copy(peerID[:], peerUUID[:])
	var token [proto.UdpTokenSize]byte
	copy(token[:], rawToken)
	proxy, err := newUDPProxy(r.log, r.listenAddr, ep, peerID, token)
	if err != nil {
		r.log.Warn("udp proxy init failed; using WebSocket fallback", "endpoint", ep, "err", err)
		return stripWelcomeUDP(env, data, payload), true, true
	}
	data["udpEndpoint"] = proxy.loopbackConn.LocalAddr().String()
	out, err := json.Marshal(env)
	if err != nil {
		proxy.close()
		return stripWelcomeUDP(env, data, payload), true, true
	}
	r.proxy = proxy
	return out, true, true
}

func stripWelcomeUDP(env, data map[string]any, original []byte) []byte {
	delete(data, "udpEndpoint")
	delete(data, "udpToken")
	out, err := json.Marshal(env)
	if err != nil {
		return original
	}
	return out
}

func (r *welcomeUDPRewriter) close() {
	if r.proxy != nil {
		r.proxy.close()
	}
}

func extractHostFromWsURL(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

func validateBridgeEndpoints(listen, upstream string, allowInsecure bool) error {
	listenHost, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid -listen address: %w", err)
	}
	listenHost = strings.Trim(listenHost, "[]")
	listenIP := net.ParseIP(listenHost)
	if listenIP == nil || !listenIP.IsLoopback() {
		return errors.New("-listen must use an explicit loopback address")
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("-upstream must be a server root URL such as wss://audio.example")
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if allowInsecure {
			return nil
		}
		return errors.New("ws:// upstream is plaintext; use wss:// or explicitly pass -allow-insecure-upstream on a trusted network")
	default:
		return errors.New("-upstream must use wss:// (or ws:// with explicit trusted-network opt-in)")
	}
}

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"network-ultra-bridge: local WS+UDP relay for firewalled DAW hosts.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
}
