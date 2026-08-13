package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestBridgeBusyListenerIsNotReclaimedByDefault(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	log := discardBridgeLogger()
	if err := maybeReclaimStaleListener(false, held.Addr().String(), log); err != nil {
		t.Fatalf("disabled reclaim returned an error: %v", err)
	}

	srv := &http.Server{Addr: held.Addr().String(), Handler: http.NewServeMux()}
	if code := serveBridge(srv, make(chan os.Signal), log); code == 0 {
		t.Fatal("listener bind failure returned success; service manager would not restart the bridge")
	}

	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := held.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		accepted <- acceptErr
	}()
	conn, err := net.DialTimeout("tcp", held.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("existing listener was disrupted by default startup path: %v", err)
	}
	_ = conn.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("existing listener could not accept after bind failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("existing listener stopped responding after bridge bind failure")
	}
}

func TestBridgeSignalsShutdownSuccessfully(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			stop := make(chan os.Signal, 1)
			stop <- sig
			srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()}
			if code := serveBridge(srv, stop, discardBridgeLogger()); code != 0 {
				t.Fatalf("signal %v returned exit code %d, want 0", sig, code)
			}
		})
	}
}

func discardBridgeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
