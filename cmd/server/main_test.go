package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestListenerFailureRequiresNonzeroProcessExit(t *testing.T) {
	for _, component := range []string{"websocket", "health"} {
		t.Run(component, func(t *testing.T) {
			stop := make(chan os.Signal)
			failures := make(chan serveFailure, 1)
			wantErr := errors.New(component + " listener failed")
			failures <- serveFailure{component: component, err: wantErr}
			code, signal, got := waitForTermination(stop, failures)
			if code == 0 || signal != nil || got.component != component || !errors.Is(got.err, wantErr) {
				t.Fatalf("code=%d signal=%v failure=%+v", code, signal, got)
			}
		})
	}
}

func TestSignalShutdownReturnsSuccess(t *testing.T) {
	for _, wantSignal := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		stop := make(chan os.Signal, 1)
		failures := make(chan serveFailure)
		stop <- wantSignal
		code, gotSignal, failure := waitForTermination(stop, failures)
		if code != 0 || gotSignal != wantSignal || failure.err != nil {
			t.Fatalf("signal=%v: code=%d gotSignal=%v failure=%+v", wantSignal, code, gotSignal, failure)
		}
	}
}
