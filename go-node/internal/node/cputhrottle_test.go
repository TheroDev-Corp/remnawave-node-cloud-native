package node

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
)

func testThrottleConfig(controlFile string) *config.Config {
	return &config.Config{
		CPUThrottleEnabled:     true,
		CPUThrottleHighPct:     90,
		CPUThrottleLowPct:      75,
		CPUThrottleRateMbps:    50,
		CPUThrottleMinMbps:     10,
		CPUThrottleControlFile: controlFile,
	}
}

func readControlFile(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read control file %s: %v", path, err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("parse control file %q: %v", string(b), err)
	}
	return v
}

func TestReadProcStat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stat")
	// user=100 nice=10 system=40 idle=800 iowait=20 irq=10 softirq=10 steal=10 (sum=1000, idle+iowait=820)
	// trailing guest=999 guest_nice=999 must be ignored.
	content := "cpu  100 10 40 800 20 10 10 10 999 999\ncpu0 50 5 20 400 10 5 5 5 0 0\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	total, idle, ok := readProcStat(p)
	if !ok || total != 1000 || idle != 820 {
		t.Fatalf("got total=%d idle=%d ok=%v, want 1000, 820, true", total, idle, ok)
	}
}

func TestCPUThrottleTickFromProcStat(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := os.WriteFile(statPath, []byte("cpu  100 0 0 900 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, statPath, ctrlPath)
	ct.tick() // baseline
	if ratio, active, rate := ct.Stats(); ratio != 0 || active || rate != 0 {
		t.Fatalf("after baseline: ratio=%v active=%v rate=%v", ratio, active, rate)
	}

	// +950 busy, +50 idle => 95% CPU
	if err := os.WriteFile(statPath, []byte("cpu  1050 0 0 950 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct.tick()
	ratio, active, _ := ct.Stats()
	if ratio < 0.949 || ratio > 0.951 || active {
		t.Fatalf("after 1st high tick: ratio=%v active=%v", ratio, active)
	}
}

func TestCPUThrottleAIMDLifecycle(t *testing.T) {
	dir := t.TempDir()
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, "", ctrlPath)

	step := func(r float64) (bool, int64) {
		ct.mu.Lock()
		ct.stepLocked(r)
		ct.mu.Unlock()
		_, active, rate := ct.Stats()
		return active, rate
	}

	// 2 ticks >= 90% should NOT engage yet; 1 low tick resets streak.
	if active, _ := step(0.92); active {
		t.Fatal("engaged on tick 1")
	}
	if active, _ := step(0.92); active {
		t.Fatal("engaged on tick 2")
	}
	if active, _ := step(0.80); active {
		t.Fatal("engaged after streak broken")
	}

	// 3 consecutive ticks >= 90% engages at RateMbps (50 Mbps = 6,250,000 B/s).
	step(0.91)
	step(0.92)
	active, rate := step(0.93)
	if !active || rate != 50*125_000 {
		t.Fatalf("expected engage at 6250000 B/s, got active=%v rate=%d", active, rate)
	}
	if got := readControlFile(t, ctrlPath); got != 50*125_000 {
		t.Fatalf("control file = %d, want %d", got, 50*125_000)
	}

	// Sustained >= 90% steps down by 0.75x every 3 ticks, clamped at MinMbps (10 Mbps = 1,250,000 B/s).
	for i := 0; i < 3; i++ {
		step(0.95)
	}
	_, rate = ct.rateSnapshot()
	want37_5 := int64(37.5 * 125_000)
	if rate != want37_5 {
		t.Fatalf("after 1st decrease: rate=%d, want %d", rate, want37_5)
	}

	// Drive down to MinMbps floor.
	for i := 0; i < 20; i++ {
		step(0.99)
	}
	if active, rate = ct.rateSnapshot(); !active || rate != 10*125_000 {
		t.Fatalf("expected floor 1250000 B/s, got active=%v rate=%d", active, rate)
	}

	// Deadband (75% < cpu < 90%) holds rate steady.
	for i := 0; i < 6; i++ {
		active, rate = step(0.82)
		if !active || rate != 10*125_000 {
			t.Fatalf("deadband changed rate: active=%v rate=%d", active, rate)
		}
	}

	// Sustained <= 75% steps up additively (+5 Mbps per 3 ticks) up to 50 Mbps, then releases to 0.
	for i := 0; i < 3; i++ {
		step(0.60)
	}
	if active, rate = ct.rateSnapshot(); !active || rate != 15*125_000 {
		t.Fatalf("after 1st increase: active=%v rate=%d, want %d", active, rate, 15*125_000)
	}

	// Continue <= 75% until released.
	for i := 0; i < 35; i++ {
		active, rate = step(0.50)
		if !active {
			break
		}
	}
	if active || rate != 0 {
		t.Fatalf("expected release to unlimited, got active=%v rate=%d", active, rate)
	}
	if got := readControlFile(t, ctrlPath); got != 0 {
		t.Fatalf("control file after release = %d, want 0", got)
	}

	// Engage again and verify shutdown resets control file to 0.
	step(0.95)
	step(0.95)
	step(0.95)
	if got := readControlFile(t, ctrlPath); got == 0 {
		t.Fatal("expected non-zero control file before shutdown")
	}
	ct.shutdown()
	if got := readControlFile(t, ctrlPath); got != 0 {
		t.Fatalf("control file after shutdown = %d, want 0", got)
	}
}

func (ct *CPUThrottle) rateSnapshot() (bool, int64) {
	_, active, rate := ct.Stats()
	return active, rate
}

func TestStartCPUThrottleDisabled(t *testing.T) {
	cfg := &config.Config{CPUThrottleEnabled: false}
	if ct := StartCPUThrottle(cfg, nil); ct != nil {
		ct.Stop()
		t.Fatal("expected nil when disabled")
	}
}
