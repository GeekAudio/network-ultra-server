package ws

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

func TestMutedSourceWebSocketAudioIsDroppedAndUnmuteRestores(t *testing.T) {
	reg := room.NewRegistry(2, 8)
	rm, err := reg.Create("mute-ws", "private", nil)
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

	mreg := metrics.NewRegistry()
	s := &Server{Metrics: mreg, Log: slog.Default()}
	pkt, err := proto.Pack(proto.AudioFrameHeader{
		SourcePeerID: [16]byte(source.ID),
		Seq:          1,
	}, []byte("audio"), nil)
	if err != nil {
		t.Fatal(err)
	}

	source.SetMuted(true)
	s.handleAudio(source, pkt)
	assertRoomForwarderBarrier(t, rm, received)

	source.SetMuted(false)
	s.handleAudio(source, pkt)
	select {
	case got := <-received:
		if string(got) != string(pkt) {
			t.Fatalf("unmuted payload=%x want=%x", got, pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("unmuted WebSocket audio did not resume")
	}

	var rendered strings.Builder
	mreg.WriteText(&rendered)
	if !strings.Contains(rendered.String(), `nu_audio_frames_dropped_total{reason="muted"} 1`) {
		t.Fatalf("missing muted drop metric:\n%s", rendered.String())
	}
}

func assertRoomForwarderBarrier(t *testing.T, rm *room.Room, received <-chan []byte) {
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
