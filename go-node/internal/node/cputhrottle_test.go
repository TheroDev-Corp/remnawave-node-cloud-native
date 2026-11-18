package node

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	vals, ok := readProcStat(p)
	if !ok {
		t.Fatal("readProcStat failed")
	}
	var total uint64
	for _, v := range vals {
		total += v
	}
	idle := vals[3] + vals[4]
	if total != 1000 || idle != 820 {
		t.Fatalf("got total=%d idle=%d, want 1000, 820", total, idle)
	}
}

func TestReadProcStatBackwardIowait(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Baseline: user=100, idle=800, iowait=50 (sum=950)
	if err := os.WriteFile(statPath, []byte("cpu  100 0 0 800 50 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, statPath, ctrlPath)
	ct.tick() // baseline

	// Second sample: iowait decreases 50 -> 40 (goes backward on NO_HZ kernel),
	// user increases 100 -> 200 (busy). Delta user = 100, Delta iowait clamped to 0.
	// dTotal = 100, dIdle = 0 => 100% CPU. Sample must NOT be discarded!
	if err := os.WriteFile(statPath, []byte("cpu  200 0 0 800 40 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct.tick()
	ratio, _, _ := ct.Stats()
	if ratio < 0.99 {
		t.Fatalf("expected ratio ~1.0 with backward iowait, got %v", ratio)
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
		ct.stepLocked(r, false)
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

func TestCPUThrottleThroughputAIMD(t *testing.T) {
	dir := t.TempDir()
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, "", ctrlPath)

	var throughput float64 = -1
	ct.SetThroughputFunc(func() float64 {
		return throughput
	})

	step := func(r float64) (bool, int64) {
		ct.mu.Lock()
		ct.stepLocked(r, false)
		ct.mu.Unlock()
		_, active, rate := ct.Stats()
		return active, rate
	}

	// 1. Initial engage with measured throughput:
	// Traffic is 40 Mbps. 40 * 0.75 = 30 Mbps (between min 10 and max 50).
	throughput = 40.0
	step(0.95)
	step(0.95)
	active, rate := step(0.95)
	if !active || rate != 30*125_000 {
		t.Fatalf("expected initial engage at 30 Mbps (3750000 B/s), got active=%v rate=%d", active, rate)
	}

	// 2. Saturated traffic ceiling hold:
	// Step up to maxRate (50 Mbps).
	for i := 0; i < 30; i++ {
		step(0.60)
		if ct.rateMbps >= 50 {
			break
		}
	}
	if ct.rateMbps != 50 {
		t.Fatalf("expected rate to reach maxRate 50 Mbps, got %v", ct.rateMbps)
	}

	// Traffic is still saturating the ceiling (45 Mbps >= 50 * 0.80 = 40 Mbps).
	throughput = 45.0
	for i := 0; i < 10; i++ {
		active, rate = step(0.60)
		if !active || rate != 50*125_000 {
			t.Fatalf("saturated ceiling hold failed on tick %d: active=%v rate=%d", i, active, rate)
		}
	}

	// 3. Unsaturated traffic releases after 5 ticks:
	// Traffic drops to 20 Mbps (< 40 Mbps).
	throughput = 20.0
	for i := 0; i < 5; i++ {
		active, rate = step(0.60)
	}
	if active || rate != 0 {
		t.Fatalf("expected release to unlimited when unsaturated, got active=%v rate=%d", active, rate)
	}

	// 4. Unsaturated release before reaching maxRate:
	// Re-engage at 30 Mbps, then drop traffic to 5 Mbps (< 30 * 0.80 = 24 Mbps).
	throughput = 40.0
	step(0.95)
	step(0.95)
	step(0.95) // engaged at 30 Mbps
	throughput = 5.0
	for i := 0; i < 5; i++ {
		active, rate = step(0.60)
	}
	if active || rate != 0 {
		t.Fatalf("expected premature release when traffic dropped way below limit, got active=%v rate=%d", active, rate)
	}
}

func TestCPUThrottleCgroupV2(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	cgroupStatPath := filepath.Join(dir, "cpu.stat")
	cgroupMaxPath := filepath.Join(dir, "cpu.max")
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Host stat: 20% host CPU (low)
	if err := os.WriteFile(statPath, []byte("cpu  200 0 0 800 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cgroup.max: 1 core quota (100000 usec per 100000 usec period)
	if err := os.WriteFile(cgroupMaxPath, []byte("100000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cgroup.stat initial baseline: usage_usec = 1,000,000
	if err := os.WriteFile(cgroupStatPath, []byte("usage_usec 1000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, statPath, ctrlPath)
	ct.SetCgroupPaths(cgroupStatPath, cgroupMaxPath)

	ct.tick() // baseline

	// Second sample: wall time advances, container uses 950,000 usec in 1s -> 95% quota used
	ct.mu.Lock()
	ct.prevWallTime = time.Now().Add(-1 * time.Second)
	ct.mu.Unlock()
	if err := os.WriteFile(statPath, []byte("cpu  400 0 0 1600 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cgroupStatPath, []byte("usage_usec 1950000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct.tick()

	ratio, _, _ := ct.Stats()
	if ratio < 0.90 {
		t.Fatalf("expected effectiveRatio to reflect cgroup quota (>0.90), got %v", ratio)
	}

	// Test container share check:
	// Host CPU is 95%, but container uses only 10,000 usec over 1s (cgroupCoresUsed = 0.01 < 0.05).
	// Container share check must suppress throttling engage!
	ct.mu.Lock()
	ct.active = false
	ct.highStreak = 0
	ct.mu.Unlock()

	// Set cgroup max to max (unlimited)
	if err := os.WriteFile(cgroupMaxPath, []byte("max 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		ct.mu.Lock()
		ct.prevWallTime = time.Now().Add(-1 * time.Second)
		ct.prevUsageUsec = 1950000 + uint64(i)*10000
		ct.mu.Unlock()

		userVal := 400 + (i+1)*950
		idleVal := 1600 + (i+1)*50
		_ = os.WriteFile(statPath, []byte(fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0\n", userVal, idleVal)), 0o644)
		_ = os.WriteFile(cgroupStatPath, []byte(fmt.Sprintf("usage_usec %d\n", 1950000+(i+1)*10000)), 0o644)

		ct.tick()
	}

	_, active, _ := ct.Stats()
	if active {
		t.Fatal("expected container share check (<0.05 cores) to prevent throttling engage")
	}
}

func TestHasRateLimitSupport(t *testing.T) {
	dir := t.TempDir()
	patchedBin := filepath.Join(dir, "xray-patched")
	unpatchedBin := filepath.Join(dir, "xray-stock")
	symlinkBin := filepath.Join(dir, "xray-symlink")

	if err := os.WriteFile(patchedBin, []byte("binary data with XRAY_RATELIMIT_FILE embedded inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unpatchedBin, []byte("binary data with no such token"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(patchedBin, symlinkBin); err != nil {
		t.Fatal(err)
	}

	if !HasRateLimitSupport(patchedBin) {
		t.Error("expected HasRateLimitSupport = true for patched binary")
	}
	if HasRateLimitSupport(unpatchedBin) {
		t.Error("expected HasRateLimitSupport = false for unpatched binary")
	}
	if !HasRateLimitSupport(symlinkBin) {
		t.Error("expected HasRateLimitSupport = true for symlink to patched binary")
	}
	if HasRateLimitSupport(filepath.Join(dir, "non-existent")) {
		t.Error("expected HasRateLimitSupport = false for non-existent file")
	}
}

func TestSetSupported(t *testing.T) {
	dir := t.TempDir()
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, "", ctrlPath)

	step := func(r float64) (bool, int64) {
		ct.mu.Lock()
		ct.stepLocked(r, false)
		ct.mu.Unlock()
		_, active, rate := ct.Stats()
		return active, rate
	}

	// Engage throttling
	step(0.95)
	step(0.95)
	active, rate := step(0.95)
	if !active || rate == 0 {
		t.Fatal("expected throttle to engage")
	}

	// Disable support: active and rateBytes must be forced to 0, and control file cleared
	ct.SetSupported(false)
	_, active, rate = ct.Stats()
	if active || rate != 0 {
		t.Fatalf("expected active=false rate=0 after SetSupported(false), got active=%v rate=%d", active, rate)
	}
	if got := readControlFile(t, ctrlPath); got != 0 {
		t.Fatalf("expected control file 0 after SetSupported(false), got %d", got)
	}

	// Subsequent ticks must not engage
	step(0.95)
	step(0.95)
	step(0.95)
	_, active, _ = ct.Stats()
	if active {
		t.Fatal("expected no engage while supported = false")
	}
}

func TestStartCPUThrottleDisabled(t *testing.T) {
	cfg := &config.Config{CPUThrottleEnabled: false}
	if ct := StartCPUThrottle(cfg, nil); ct != nil {
		ct.Stop()
		t.Fatal("expected nil when disabled")
	}
}

func TestCPUThrottleTickIdleExceedsTotal(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	ctrlPath := filepath.Join(dir, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := os.WriteFile(statPath, []byte("cpu  100 0 0 900 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct := newCPUThrottle(testThrottleConfig(ctrlPath), log, statPath, ctrlPath)
	ct.tick() // baseline: total=1000, idle=900

	// dTotal = 100, dIdle = 200 (dIdle > dTotal) must clamp ratio to 0 without uint64 underflow.
	if err := os.WriteFile(statPath, []byte("cpu  0 0 0 1100 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ct.tick()
	ratio, active, rate := ct.Stats()
	if ratio != 0 || active || rate != 0 {
		t.Fatalf("expected ratio=0 active=false rate=0, got ratio=%v active=%v rate=%d", ratio, active, rate)
	}
}

func TestCPUThrottleWriteRateFileFailureKeepsState(t *testing.T) {
	dir := t.TempDir()
	// Use a file path as the parent directory so MkdirAll/CreateTemp fails deterministically.
	blockerFile := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	badCtrlPath := filepath.Join(blockerFile, "rw-ratelimit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ct := newCPUThrottle(testThrottleConfig(badCtrlPath), log, "", badCtrlPath)
	ct.lastWritten = 0

	ct.mu.Lock()
	ct.stepLocked(0.95, false)
	ct.stepLocked(0.95, false)
	ct.stepLocked(0.95, false) // attempts to engage and write badCtrlPath
	ct.mu.Unlock()

	_, active, rate := ct.Stats()
	if active || rate != 0 {
		t.Fatalf("expected previous state preserved on write failure, got active=%v rate=%d", active, rate)
	}
}
