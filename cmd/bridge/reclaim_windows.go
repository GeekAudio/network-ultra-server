//go:build windows

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	processTerminate               = 0x0001
	processQueryLimitedInformation = 0x1000
	synchronize                    = 0x00100000
	tcpTableOwnerPIDListener       = 3
	errorInsufficientBuffer        = 122
	waitObject0                    = 0
	waitTimeout                    = 258
	terminateWaitMillis            = 5000
)

var (
	procGetExtendedTCPTable        = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")
	procQueryFullProcessImageNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
)

type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

type mibTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwningPID     uint32
}

type verifiedProcess struct {
	pid    int
	handle syscall.Handle
	path   string
}

// portIsFree returns true if we can bind a TCP listener on addr right now.
func portIsFree(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	time.Sleep(50 * time.Millisecond)
	return true
}

// reclaimPort enumerates only the exact listening IP:port, opens every holder
// once with QUERY|TERMINATE|SYNCHRONIZE, verifies its image path through that
// same handle, rechecks socket ownership, and terminates through the same
// handle. PID reuse therefore cannot redirect termination to a new process.
func reclaimPort(addr string, log *slog.Logger) error {
	pids, err := pidsHoldingAddr(addr)
	if err != nil {
		return fmt.Errorf("enumerate exact listener: %w", err)
	}
	if len(pids) == 0 {
		return errors.New("address appears busy but no exact listener holder was found")
	}

	currentHandle, err := syscall.GetCurrentProcess()
	if err != nil {
		return fmt.Errorf("GetCurrentProcess: %w", err)
	}
	currentPath, err := queryProcessImagePath(currentHandle)
	if err != nil {
		return fmt.Errorf("query own executable: %w", err)
	}

	opened := make([]verifiedProcess, 0, len(pids))
	defer func() {
		for _, process := range opened {
			_ = syscall.CloseHandle(process.handle)
		}
	}()
	for _, pid := range pids {
		if pid == os.Getpid() {
			continue
		}
		handle, err := syscall.OpenProcess(
			processQueryLimitedInformation|processTerminate|synchronize,
			false,
			uint32(pid),
		)
		if err != nil {
			return fmt.Errorf("open listener pid %d with fixed handle: %w", pid, err)
		}
		path, err := queryProcessImagePath(handle)
		if err != nil {
			_ = syscall.CloseHandle(handle)
			return fmt.Errorf("query listener pid %d image: %w", pid, err)
		}
		if !sameCanonicalExecutable(currentPath, path) {
			_ = syscall.CloseHandle(handle)
			return fmt.Errorf("address held by %q (pid %d), not exact executable %q; refusing to terminate", path, pid, currentPath)
		}
		opened = append(opened, verifiedProcess{pid: pid, handle: handle, path: path})
	}
	if len(opened) == 0 {
		return errors.New("no stale bridge listener eligible for termination")
	}

	confirmed, err := pidsHoldingAddr(addr)
	if err != nil {
		return fmt.Errorf("recheck exact listener ownership: %w", err)
	}
	if !allPIDsStillHolding(opened, confirmed) {
		return errors.New("listener ownership changed during verification; refusing to terminate")
	}

	for _, process := range opened {
		log.Info("terminating verified stale bridge", "pid", process.pid, "exe", process.path)
		if err := syscall.TerminateProcess(process.handle, 1); err != nil {
			return fmt.Errorf("terminate verified pid %d: %w", process.pid, err)
		}
		result, err := syscall.WaitForSingleObject(process.handle, terminateWaitMillis)
		if err != nil {
			return fmt.Errorf("wait for verified pid %d: %w", process.pid, err)
		}
		if result == waitTimeout {
			return fmt.Errorf("verified pid %d did not exit within timeout", process.pid)
		}
		if result != waitObject0 {
			return fmt.Errorf("wait for verified pid %d returned %#x", process.pid, result)
		}
	}
	return nil
}

func queryProcessImagePath(handle syscall.Handle) (string, error) {
	for size := uint32(512); size <= 32768; size *= 2 {
		buf := make([]uint16, size)
		chars := size
		r1, _, callErr := procQueryFullProcessImageNameW.Call(
			uintptr(handle),
			0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&chars)),
		)
		if r1 != 0 {
			return normalizeProcessImagePath(syscall.UTF16ToString(buf[:chars]))
		}
		if callErr != syscall.ERROR_INSUFFICIENT_BUFFER {
			return "", callErr
		}
	}
	return "", errors.New("process image path exceeds Windows limit")
}

func normalizeProcessImagePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, `\\?\`)
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("process image path is not absolute")
	}
	return filepath.Clean(path), nil
}

func sameCanonicalExecutable(current, candidate string) bool {
	return strings.EqualFold(filepath.Clean(current), filepath.Clean(candidate))
}

func allPIDsStillHolding(opened []verifiedProcess, confirmed []int) bool {
	set := make(map[int]struct{}, len(confirmed))
	for _, pid := range confirmed {
		set[pid] = struct{}{}
	}
	for _, process := range opened {
		if _, ok := set[process.pid]; !ok {
			return false
		}
	}
	return true
}

func pidsHoldingAddr(addr string) ([]int, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("reclaim address must use an explicit loopback IP")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("reclaim port must be between 1 and 65535")
	}
	if ip4 := ip.To4(); ip4 != nil {
		return pidsFromTCP4Table(ip4, port)
	}
	return pidsFromTCP6Table(ip.To16(), port)
}

func tcpTable(addressFamily uint32) ([]byte, error) {
	var size uint32
	result, _, _ := procGetExtendedTCPTable.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(addressFamily),
		tcpTableOwnerPIDListener,
		0,
	)
	if result != errorInsufficientBuffer || size < 4 {
		return nil, fmt.Errorf("GetExtendedTcpTable size returned %d (size %d)", result, size)
	}
	buf := make([]byte, size)
	result, _, _ = procGetExtendedTCPTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(addressFamily),
		tcpTableOwnerPIDListener,
		0,
	)
	if result != 0 {
		return nil, syscall.Errno(result)
	}
	return buf[:size], nil
}

func pidsFromTCP4Table(target net.IP, targetPort int) ([]int, error) {
	buf, err := tcpTable(syscall.AF_INET)
	if err != nil {
		return nil, err
	}
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSize := unsafe.Sizeof(mibTCPRowOwnerPID{})
	if uint64(4)+uint64(count)*uint64(rowSize) > uint64(len(buf)) {
		return nil, errors.New("truncated IPv4 TCP owner table")
	}
	set := make(map[int]struct{})
	for index := uint32(0); index < count; index++ {
		row := (*mibTCPRowOwnerPID)(unsafe.Add(unsafe.Pointer(&buf[4]), uintptr(index)*rowSize))
		ip := net.IPv4(byte(row.LocalAddr), byte(row.LocalAddr>>8), byte(row.LocalAddr>>16), byte(row.LocalAddr>>24))
		if ip.Equal(target) && networkPort(row.LocalPort) == targetPort {
			set[int(row.OwningPID)] = struct{}{}
		}
	}
	return sortedPIDs(set), nil
}

func pidsFromTCP6Table(target net.IP, targetPort int) ([]int, error) {
	buf, err := tcpTable(syscall.AF_INET6)
	if err != nil {
		return nil, err
	}
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSize := unsafe.Sizeof(mibTCP6RowOwnerPID{})
	if uint64(4)+uint64(count)*uint64(rowSize) > uint64(len(buf)) {
		return nil, errors.New("truncated IPv6 TCP owner table")
	}
	set := make(map[int]struct{})
	for index := uint32(0); index < count; index++ {
		row := (*mibTCP6RowOwnerPID)(unsafe.Add(unsafe.Pointer(&buf[4]), uintptr(index)*rowSize))
		if net.IP(row.LocalAddr[:]).Equal(target) && networkPort(row.LocalPort) == targetPort {
			set[int(row.OwningPID)] = struct{}{}
		}
	}
	return sortedPIDs(set), nil
}

func networkPort(raw uint32) int {
	return int(byte(raw))<<8 | int(byte(raw>>8))
}

func sortedPIDs(set map[int]struct{}) []int {
	pids := make([]int, 0, len(set))
	for pid := range set {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}
