// audiotest spins up two clients (sender + receiver) in one process and
// validates the audio fan-out path end-to-end against a running Network
// Ultra Server.
//
// Usage:
//
//	go run ./cmd/audiotest -server ws://146.56.202.138:18900 -room audiotest -frames 100
//
// Flow:
//  1. Receiver joins room first, role=recv
//  2. Sender joins same room, role=send
//  3. Sender pushes N synthesised audio frames (sine wave, well-formed PCM payload)
//  4. Receiver collects every binary frame the server forwards
//  5. Compare seq + payload byte-for-byte; report drops/reorders/loss
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
)

const (
	frameSamples = 480
	channels     = 2
	bytesPerSamp = 4 // float32
)

func main() {
	server := flag.String("server", "ws://127.0.0.1:18900", "Server URL")
	room := flag.String("room", "audiotest", "Room name")
	frames := flag.Int("frames", 100, "How many audio frames the sender pushes")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Receiver joins first (so it sees sender's frames).
	recv, recvPeerID := dialAndJoin(ctx, *server, "Recv", *room, "recv")
	defer recv.Close(websocket.StatusNormalClosure, "bye")

	// Drain a couple of seconds for peer_joined messages on receiver side.
	receivedFrames := make(chan recvFrame, 2*(*frames))
	var receiverWG sync.WaitGroup
	receiverWG.Add(1)
	rctx, rcancel := context.WithCancel(ctx)
	go func() {
		defer receiverWG.Done()
		for {
			mt, data, err := recv.Read(rctx)
			if err != nil {
				return
			}
			switch mt {
			case websocket.MessageBinary:
				rf, ok := parseAudioFrame(data)
				if ok {
					select {
					case receivedFrames <- rf:
					default:
					}
				}
			case websocket.MessageText:
				env, _ := proto.Decode(data)
				logf("recv < %s", env.Type)
			}
		}
	}()

	// 2. Sender joins.
	time.Sleep(300 * time.Millisecond)
	send, sendPeerID := dialAndJoin(ctx, *server, "Send", *room, "send")
	defer send.Close(websocket.StatusNormalClosure, "bye")

	logf("sender peerId=%s, receiver peerId=%s", sendPeerID, recvPeerID)

	// 3. Push frames.
	type sentFrame struct {
		seq     uint16
		payload []byte
	}
	sent := make([]sentFrame, 0, *frames)
	for i := 0; i < *frames; i++ {
		seq := uint16(i + 1)
		payload := makeSinePayload(seq)
		body := packAudioFrame(sendPeerID, seq, payload)
		if err := send.Write(ctx, websocket.MessageBinary, body); err != nil {
			fail("send write: %v", err)
		}
		sent = append(sent, sentFrame{seq: seq, payload: payload})
		// Pace at ~10ms (real-time)
		time.Sleep(10 * time.Millisecond)
	}
	logf("sender finished pushing %d frames", *frames)

	// 4. Drain receiver for a tail window so all frames arrive.
	time.Sleep(800 * time.Millisecond)
	rcancel()
	receiverWG.Wait()
	close(receivedFrames)

	// 5. Compare.
	recvMap := make(map[uint16]recvFrame)
	for rf := range receivedFrames {
		// Filter to only frames originating from sender (server should never
		// echo back to sender, but recv side could see other peers if test
		// is concurrent).
		if rf.sourcePeer == sendPeerID {
			recvMap[rf.seq] = rf
		}
	}

	var ok, missing, mismatched int
	for _, sf := range sent {
		rf, present := recvMap[sf.seq]
		if !present {
			missing++
			continue
		}
		if len(rf.payload) != len(sf.payload) || !bytesEqual(rf.payload, sf.payload) {
			mismatched++
			continue
		}
		ok++
	}

	pct := func(n int) string { return fmt.Sprintf("%.1f%%", 100.0*float64(n)/float64(len(sent))) }
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  Sent:        %d frames\n", len(sent))
	fmt.Printf("  Received OK: %d (%s)\n", ok, pct(ok))
	fmt.Printf("  Missing:     %d (%s)\n", missing, pct(missing))
	fmt.Printf("  Mismatched:  %d (%s)\n", mismatched, pct(mismatched))
	fmt.Println("─────────────────────────────────────────")

	if missing > len(sent)/20 || mismatched > 0 {
		os.Exit(1)
	}
}

type recvFrame struct {
	sourcePeer uuid.UUID
	seq        uint16
	payload    []byte
}

func parseAudioFrame(data []byte) (recvFrame, bool) {
	if len(data) < 24 || data[0] != 0xA1 {
		return recvFrame{}, false
	}
	var rf recvFrame
	copy(rf.sourcePeer[:], data[4:20])
	rf.seq = binary.LittleEndian.Uint16(data[20:22])
	length := int(binary.LittleEndian.Uint16(data[22:24]))
	if 24+length > len(data) {
		return recvFrame{}, false
	}
	rf.payload = make([]byte, length)
	copy(rf.payload, data[24:24+length])
	return rf, true
}

func packAudioFrame(source uuid.UUID, seq uint16, payload []byte) []byte {
	buf := make([]byte, 24+len(payload))
	buf[0] = 0xA1
	copy(buf[4:20], source[:])
	binary.LittleEndian.PutUint16(buf[20:22], seq)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(len(payload)))
	copy(buf[24:], payload)
	return buf
}

func makeSinePayload(seq uint16) []byte {
	out := make([]byte, frameSamples*channels*bytesPerSamp)
	const freq = 440.0
	const sr = 48000.0
	for i := 0; i < frameSamples; i++ {
		// continuous phase across frames so byte-perfect comparison works
		// (we'll only ever compare same-seq round-trip)
		t := float64(int(seq)*frameSamples+i) / sr
		s := float32(0.5 * math.Sin(2*math.Pi*freq*t))
		off := i * channels * bytesPerSamp
		binary.LittleEndian.PutUint32(out[off:], math.Float32bits(s))
		binary.LittleEndian.PutUint32(out[off+4:], math.Float32bits(s))
	}
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dialAndJoin(ctx context.Context, server, username, room, role string) (*websocket.Conn, uuid.UUID) {
	c, _, err := websocket.Dial(ctx, server, &websocket.DialOptions{
		Subprotocols: []string{"network-ultra-v1"},
	})
	if err != nil {
		fail("dial: %v", err)
	}
	c.SetReadLimit(1 << 20)

	hello, _ := proto.Encode(proto.TypeHello, "h", proto.HelloData{
		Username: username,
		Client:   "audiotest/1.0",
		Platform: runtime.GOOS,
	})
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		fail("hello: %v", err)
	}
	mt, body, err := c.Read(ctx)
	if err != nil {
		fail("read welcome: %v", err)
	}
	if mt != websocket.MessageText {
		fail("bad welcome frame type")
	}
	env, _ := proto.Decode(body)
	if env.Type != proto.TypeWelcome {
		fail("expected welcome, got %s", env.Type)
	}
	var w proto.WelcomeData
	_ = json.Unmarshal(env.Data, &w)
	myID, err := uuid.Parse(w.PeerID)
	if err != nil {
		fail("bad peerId: %v", err)
	}

	// First try to create; if taken, join.
	create, _ := proto.Encode(proto.TypeRoomCreate, "c", proto.RoomCreateData{
		RoomName:   room,
		Visibility: "public",
	})
	if err := c.Write(ctx, websocket.MessageText, create); err != nil {
		fail("create: %v", err)
	}
	for {
		mt, body, err = c.Read(ctx)
		if err != nil {
			fail("read after create: %v", err)
		}
		env, _ = proto.Decode(body)
		switch env.Type {
		case proto.TypeRoomJoined:
			return c, myID
		case proto.TypeError:
			var e proto.ErrorData
			_ = json.Unmarshal(env.Data, &e)
			if e.Code == proto.ErrRoomNameTaken {
				join, _ := proto.Encode(proto.TypeRoomJoin, "j", proto.RoomJoinData{
					RoomName: room,
					Role:     role,
				})
				if err := c.Write(ctx, websocket.MessageText, join); err != nil {
					fail("join: %v", err)
				}
				continue
			}
			fail("create rejected: %s %s", e.Code, e.Message)
		case proto.TypePeerJoined, proto.TypePeerLeft, proto.TypeRoomListDelta:
			continue
		default:
			fail("unexpected after create: %s", env.Type)
		}
	}
}

func logf(format string, args ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
