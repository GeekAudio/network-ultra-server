//go:build windows

package main

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestSameCanonicalExecutableRequiresExactPath(t *testing.T) {
	current := `C:\Program Files\Network Ultra\network-ultra-bridge.exe`
	for _, tc := range []struct {
		name, candidate string
		want            bool
	}{
		{"same path", `C:\Program Files\Network Ultra\network-ultra-bridge.exe`, true},
		{"case insensitive", `c:\program files\network ultra\NETWORK-ULTRA-BRIDGE.EXE`, true},
		{"same basename elsewhere", `C:\Users\attacker\network-ultra-bridge.exe`, false},
		{"prefix confusion", `C:\Program Files\Network Ultra Evil\network-ultra-bridge.exe`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameCanonicalExecutable(current, tc.candidate); got != tc.want {
				t.Fatalf("sameCanonicalExecutable(%q, %q)=%v want %v", current, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestAllPIDsStillHoldingRequiresEveryOpenedProcess(t *testing.T) {
	opened := []verifiedProcess{{pid: 11}, {pid: 22}}
	if !allPIDsStillHolding(opened, []int{22, 11, 33}) {
		t.Fatal("confirmed holders were rejected")
	}
	if allPIDsStillHolding(opened, []int{11, 33}) {
		t.Fatal("missing holder was accepted")
	}
}

func TestQueryCurrentProcessImagePath(t *testing.T) {
	handle, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	path, err := queryProcessImagePath(handle)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("current process image path is empty")
	}
}

func TestPidsHoldingAddrFindsExactCurrentListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	wantPID := os.Getpid()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		pids, err := pidsHoldingAddr(listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		for _, pid := range pids {
			if pid == wantPID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener %s did not appear under pid %d; got %v", listener.Addr(), wantPID, pids)
		}
	}
}
