package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

func TestHumanLabelUnicodeAndControlBoundaries(t *testing.T) {
	if !validUsername(strings.Repeat("名", proto.MaxUsernameRunes)) {
		t.Fatal("32-rune Chinese username was rejected")
	}
	if validUsername(strings.Repeat("名", proto.MaxUsernameRunes+1)) {
		t.Fatal("33-rune username was accepted")
	}
	if !validRoomName("  " + strings.Repeat("房", proto.MaxRoomNameRunes) + "  ") {
		t.Fatal("trimmed 64-rune Chinese room name was rejected")
	}
	if validRoomName(strings.Repeat("房", proto.MaxRoomNameRunes+1)) {
		t.Fatal("65-rune room name was accepted")
	}
	for _, invalid := range []string{"   ", "name\nadmin", "room\x00name", string([]byte{0xff})} {
		if validUsername(invalid) || validRoomName(invalid) {
			t.Fatalf("invalid human label accepted: %q", invalid)
		}
	}
}

func TestProtocolRejectsOversizedRoomNameAndPasswordsBeforeWork(t *testing.T) {
	s, c, ctx := connectedValidationServer(t, strings.Repeat("名", proto.MaxUsernameRunes))

	for _, tc := range []struct {
		name string
		typ  string
		data any
	}{
		{"public room name", proto.TypeRoomCreate, proto.RoomCreateData{
			RoomName: strings.Repeat("房", proto.MaxRoomNameRunes+1), Visibility: "public",
		}},
		{"create password", proto.TypeRoomCreate, proto.RoomCreateData{
			RoomName: "valid", Visibility: "private", Password: strings.Repeat("p", proto.MaxPasswordBytes+1),
		}},
		{"join password", proto.TypeRoomJoin, proto.RoomJoinData{
			RoomName: "missing", Password: strings.Repeat("p", proto.MaxPasswordBytes+1), Role: "send",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := proto.Encode(tc.typ, tc.name, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Write(ctx, websocket.MessageText, request); err != nil {
				t.Fatal(err)
			}
			if code := readProtocolError(t, c, ctx); code != proto.ErrProtocolError {
				t.Fatalf("code=%q", code)
			}
		})
	}
	if s.Reg.CountRooms() != 0 {
		t.Fatalf("invalid create persisted %d rooms", s.Reg.CountRooms())
	}
}

func TestSubscribeRequiresAtMostEightUniqueUUIDs(t *testing.T) {
	_, c, ctx := connectedValidationServer(t, "receiver")
	valid := make([]string, proto.MaxPeersPerRoom+1)
	for i := range valid {
		valid[i] = uuid.NewString()
	}
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"too many", valid},
		{"invalid", []string{"not-a-uuid"}},
		{"duplicate", []string{valid[0], valid[0]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := proto.Encode(proto.TypeSubscribe, tc.name, proto.SubscribeData{SourcePeerIDs: tc.ids})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Write(ctx, websocket.MessageText, request); err != nil {
				t.Fatal(err)
			}
			if code := readProtocolError(t, c, ctx); code != proto.ErrProtocolError {
				t.Fatalf("code=%q", code)
			}
		})
	}
}

func TestHelloAcceptsThirtyTwoChineseRunesAndRejectsThirtyThree(t *testing.T) {
	s := validationServer()
	httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	for _, tc := range []struct {
		name, username, want string
	}{
		{"max", strings.Repeat("名", proto.MaxUsernameRunes), proto.TypeWelcome},
		{"too long", strings.Repeat("名", proto.MaxUsernameRunes+1), proto.ErrBadUsername},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(websocket.StatusNormalClosure, "")
			hello, err := proto.Encode(proto.TypeHello, "hello", proto.HelloData{Username: tc.username})
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
			got := env.Type
			if env.Type == proto.TypeError {
				var data proto.ErrorData
				if err := json.Unmarshal(env.Data, &data); err != nil {
					t.Fatal(err)
				}
				got = data.Code
			}
			if got != tc.want {
				t.Fatalf("response=%q want %q", got, tc.want)
			}
		})
	}
}

func connectedValidationServer(t *testing.T, username string) (*Server, *websocket.Conn, context.Context) {
	t.Helper()
	s := validationServer()
	httpServer := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	t.Cleanup(httpServer.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	hello, err := proto.Encode(proto.TypeHello, "hello", proto.HelloData{Username: username})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	return s, c, ctx
}

func validationServer() *Server {
	return &Server{
		Reg:            room.NewRegistry(8, proto.MaxPeersPerRoom),
		Metrics:        metrics.NewRegistry(),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConnections: 8,
	}
}

func readProtocolError(t *testing.T, c *websocket.Conn, ctx context.Context) string {
	t.Helper()
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != proto.TypeError {
		t.Fatalf("type=%q want error", env.Type)
	}
	var data proto.ErrorData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Code
}
