package xray

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// xrayBinary returns the binary to test against (XRAY_BINARY), or skips.
func xrayBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("XRAY_BINARY")
	if bin == "" {
		t.Skip("set XRAY_BINARY to an xray v26.10.21 binary to run the supervisor tests")
	}
	return bin
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// apiConfig is the api part generateApiConfig injects, on a TCP listener
// so the test also runs on hosts without abstract unix sockets.
func apiConfig(port int) []byte {
	return fmt.Appendf(nil, `{"log":{"loglevel":"warning"},"stats":{},
"api":{"services":["HandlerService","StatsService","RoutingService"],"tag":"REMNAWAVE_API"},
"inbounds":[{"tag":"REMNAWAVE_API_INBOUND","listen":"127.0.0.1","port":%d,"protocol":"tunnel"}],
"outbounds":[{"protocol":"freedom"}],
"routing":{"rules":[{"inboundTag":["REMNAWAVE_API_INBOUND"],"outboundTag":"REMNAWAVE_API"}]}}`, port)
}

func TestStartReadyStop(t *testing.T) {
	bin := xrayBinary(t)
	raw, semver, err := Version(context.Background(), bin)
	if err != nil || !strings.HasPrefix(raw, "Xray ") || semver == "" {
		t.Fatalf("Version = %q, %q, %v", raw, semver, err)
	}

	p := NewProcess(bin, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	port := freePort(t)
	api, err := Dial(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()

	if err := p.Start(apiConfig(port)); err != nil {
		t.Fatal(err)
	}
	if err := WaitReady(context.Background(), api, p); err != nil {
		t.Fatalf("WaitReady: %v\n%s", err, strings.Join(p.Tail(20), "\n"))
	}
	st := p.Status()
	if !st.Up || st.PID == 0 {
		t.Fatalf("status = %+v", st)
	}
	s, err := api.SysStats(context.Background())
	if err != nil || s.NumGoroutine == 0 {
		t.Fatalf("SysStats = %v, %v", s, err)
	}

	// Restart: Start stops the running xray first and the new one takes over the port.
	if err := p.Start(apiConfig(port)); err != nil {
		t.Fatal(err)
	}
	if err := WaitReady(context.Background(), api, p); err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if p.Status().PID == st.PID {
		t.Error("restart should run a new process")
	}

	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if p.Status().Up {
		t.Error("still up after Stop")
	}
	if _, err := api.SysStats(context.Background()); err == nil {
		t.Error("api should be gone after Stop")
	}
}

func TestCrashIsDetectedAndNotRestarted(t *testing.T) {
	bin := xrayBinary(t)
	p := NewProcess(bin, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	port := freePort(t)
	api, _ := Dial(fmt.Sprintf("127.0.0.1:%d", port))
	defer api.Close()

	// An invalid config makes xray exit right away; WaitReady must notice
	// the dead process instead of polling for 30 retries.
	if err := p.Start([]byte(`{"inbounds":[{"protocol":"definitely-not-a-protocol"}]}`)); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	err := WaitReady(context.Background(), api, p)
	if !errors.Is(err, ErrProcessDown) {
		t.Fatalf("want ErrProcessDown, got %v", err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Errorf("crash detection took %v", time.Since(t0))
	}
	if tail := strings.Join(p.Tail(5), "\n"); !strings.Contains(tail, "Failed to start") {
		t.Errorf("log tail should explain the failure, got:\n%s", tail)
	}
	time.Sleep(300 * time.Millisecond)
	if p.Status().Up {
		t.Error("xray must not be restarted automatically (s6 -o semantics)")
	}
}

// TestCountsStartsAndCrashes runs a stand-in binary (a shell script that
// sleeps), so it needs no xray: a Stop is not a crash, a SIGKILL from
// outside is.
func TestCountsStartsAndCrashes(t *testing.T) {
	bin := t.TempDir() + "/fake-xray"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewProcess(bin, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s, c := p.Counts(); s != 0 || c != 0 {
		t.Fatalf("fresh: %d %d", s, c)
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(nil); err != nil { // restart = stop + start
		t.Fatal(err)
	}
	_ = p.Stop()
	if s, c := p.Counts(); s != 2 || c != 0 {
		t.Fatalf("after two starts and a stop: starts %d crashes %d", s, c)
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	pid := p.Status().PID
	proc, _ := os.FindProcess(pid)
	_ = proc.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for p.Status().Up && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s, c := p.Counts(); s != 3 || c != 1 {
		t.Fatalf("after SIGKILL: starts %d crashes %d", s, c)
	}
}
