// testclient is a small CLI tool that exercises the Network Ultra Server
// protocol end-to-end. It serves as the reference behaviour the C++ VST
// client needs to match byte-for-byte.
//
// Usage:
//
//	go run ./cmd/testclient -server ws://146.56.202.138:18900 -username Akimi -room demo
//
// What it does:
//  1. Open WebSocket
//  2. Send hello, expect welcome
//  3. Create room (or join if it exists)
//  4. Send N synthesised audio frames (silence, well-formed)
//  5. Print every server message as JSON
//  6. Send room_leave + close
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
)

func main() {
	server := flag.String("server", "ws://127.0.0.1:18900", "Server URL (ws:// or wss://)")
	username := flag.String("username", "test-cli", "Username (1..32 chars)")
	room := flag.String("room", "demo", "Room name to create / join")
	role := flag.String("role", "send", "Role: send | recv")
	password := flag.String("password", "", "Optional room password (must match if room exists)")
	visibility := flag.String("visibility", "public", "Room visibility (only used when creating): public | private")
	frames := flag.Int("frames", 5, "Number of synthetic audio frames to push (0 = none, just hold connection)")
	holdSec := flag.Int("hold", 3, "Seconds to hold connection after frames sent")
	flag.Parse()

	u, err := url.Parse(*server)
	if err != nil {
		fail("bad -server: %v", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		fail("server URL must start with ws:// or wss://")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n[interrupt] cancelling...")
		cancel()
	}()

	// ---- Connect ----
	dialCtx, dialCancel := context.WithTimeout(ctx, 8*time.Second)
	defer dialCancel()

	c, _, err := websocket.Dial(dialCtx, *server, &websocket.DialOptions{
		Subprotocols: []string{"network-ultra-v1"},
	})
	if err != nil {
		fail("ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "bye")
	c.SetReadLimit(1 << 20) // 1 MiB
	logf("connected to %s", *server)

	// ---- 1. hello ----
	if err := sendText(ctx, c, proto.TypeHello, "hello-1", proto.HelloData{
		Username: *username,
		Client:   "testclient/1.0",
		Platform: runtime.GOOS + "-" + runtime.GOARCH,
	}); err != nil {
		fail("send hello: %v", err)
	}

	// ---- 2. expect welcome ----
	env, err := readText(ctx, c)
	if err != nil {
		fail("read welcome: %v", err)
	}
	if env.Type != proto.TypeWelcome {
		fail("expected welcome, got %s: %s", env.Type, string(env.Data))
	}
	var welcome proto.WelcomeData
	_ = json.Unmarshal(env.Data, &welcome)
	logf("welcome peerId=%s serverVersion=%s", welcome.PeerID, welcome.ServerVersion)

	myPeerID, err := uuid.Parse(welcome.PeerID)
	if err != nil {
		fail("server returned invalid peerId: %v", err)
	}

	// ---- 3. create room (graceful: if it exists, fall back to join) ----
	if err := sendText(ctx, c, proto.TypeRoomCreate, "create-1", proto.RoomCreateData{
		RoomName:   *room,
		Visibility: *visibility,
		Password:   *password,
	}); err != nil {
		fail("send room_create: %v", err)
	}

	for {
		env, err = readText(ctx, c)
		if err != nil {
			fail("read after room_create: %v", err)
		}
		switch env.Type {
		case proto.TypeRoomJoined:
			var rj proto.RoomJoinedData
			_ = json.Unmarshal(env.Data, &rj)
			logf("room_joined name=%s id=%s peers=%d", rj.RoomName, rj.RoomID, len(rj.Peers))
		case proto.TypeError:
			var e proto.ErrorData
			_ = json.Unmarshal(env.Data, &e)
			if e.Code == proto.ErrRoomNameTaken {
				logf("room exists, joining instead")
				if err := sendText(ctx, c, proto.TypeRoomJoin, "join-1", proto.RoomJoinData{
					RoomName: *room,
					Password: *password,
					Role:     *role,
				}); err != nil {
					fail("send room_join: %v", err)
				}
				continue
			}
			fail("server error %s: %s", e.Code, e.Message)
		case proto.TypePeerJoined, proto.TypePeerLeft, proto.TypeRoomListDelta:
			// ignore async deltas
			continue
		default:
			fail("unexpected message after room_create: %s data=%s", env.Type, string(env.Data))
		}
		break
	}

	// ---- 4. push synthesised audio frames ----
	for i := 0; i < *frames; i++ {
		seq := uint16(i + 1)
		body := makeAudioFrame(myPeerID, seq, 480) // 480 zero samples worth of payload
		if err := c.Write(ctx, websocket.MessageBinary, body); err != nil {
			fail("send audio frame %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if *frames > 0 {
		logf("sent %d audio frame(s)", *frames)
	}

	// ---- 5. hold + drain server messages ----
	if *holdSec > 0 {
		logf("holding connection for %ds, dumping server messages...", *holdSec)
		holdCtx, holdCancel := context.WithTimeout(ctx, time.Duration(*holdSec)*time.Second)
		drainLoop(holdCtx, c)
		holdCancel()
	}

	// ---- 6. graceful leave ----
	_ = sendText(ctx, c, proto.TypeRoomLeave, "leave-1", struct{}{})
	logf("done")
}

func sendText(ctx context.Context, c *websocket.Conn, typ, id string, payload any) error {
	body, err := proto.Encode(typ, id, payload)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Write(wctx, websocket.MessageText, body); err != nil {
		return err
	}
	logf("> %s id=%s data=%s", typ, id, prettyDataPreview(body, 200))
	return nil
}

func readText(ctx context.Context, c *websocket.Conn) (proto.Envelope, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mt, data, err := c.Read(rctx)
	if err != nil {
		return proto.Envelope{}, err
	}
	if mt != websocket.MessageText {
		return proto.Envelope{}, fmt.Errorf("expected text frame, got %v", mt)
	}
	env, err := proto.Decode(data)
	if err != nil {
		return env, err
	}
	logf("< %s id=%s data=%s", env.Type, env.ID, prettyDataPreview(data, 200))
	return env, nil
}

func drainLoop(ctx context.Context, c *websocket.Conn) {
	// IMPORTANT: coder/websocket maps ctx cancellation to conn close (not just
	// per-read deadline). So we must NOT cancel a per-iteration context.
	// Use the parent ctx and let Read block; when parent expires the conn
	// will close which is fine.
	for {
		mt, data, err := c.Read(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return
			}
			logf("(drain read err: %v)", err)
			return
		}
		switch mt {
		case websocket.MessageText:
			env, _ := proto.Decode(data)
			logf("< %s id=%s data=%s", env.Type, env.ID, prettyDataPreview(data, 200))
		case websocket.MessageBinary:
			logf("< (binary frame, %d bytes)", len(data))
		}
	}
}

// makeAudioFrame builds a 24-byte header + N*8 byte zero payload (placeholder
// for FLAC). Useful only to validate framing; server forwards as-is to recv
// peers in the same room.
func makeAudioFrame(sourcePeerID uuid.UUID, seq uint16, samples int) []byte {
	const hdrSize = 24
	payloadLen := samples * 2 * 4 // stereo float32 (placeholder, not actual FLAC)
	if payloadLen > 8192 {
		payloadLen = 8192
	}
	buf := make([]byte, hdrSize+payloadLen)
	buf[0] = 0xA1 // type
	copy(buf[4:20], sourcePeerID[:])
	binary.LittleEndian.PutUint16(buf[20:22], seq)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(payloadLen))
	// payload stays zero
	return buf
}

func prettyDataPreview(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

func logf(format string, args ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
