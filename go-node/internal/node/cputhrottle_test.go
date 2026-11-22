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

func TestAdaptiveRateLifecycle(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rate")
	ct := newCPUThrottle(testThrottleConfig(file), slog.New(slog.NewTextHandler(io.Discard, nil)), "", file)
	tick := func(cpu, mbps float64, queued bool, repeat int) {
		t.Helper()
		for i := 0; i < repeat; i++ {
			if mbps >= 0 {
				pending := 0
				if queued {
					pending = 1
				}
				data := fmt.Sprintf("%d %d %d", time.Now().UnixNano(), int64(mbps*BytesPerMbps), pending)
				if err := os.WriteFile(file+".traffic", []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ct.stepLocked(cpu, false)
		}
	}
	want := func(mbps float64) {
		t.Helper()
		if got := readControlFile(t, file); got != int64(mbps*BytesPerMbps) {
			t.Fatalf("limit %d; want %.3f Mbps", got, mbps)
		}
	}
	tick(.95, 0, false, 3)
	want(0)
	tick(.95, 100, false, 1)
	tick(.95, 200, false, 1)
	tick(.95, 300, false, 1)
	want(180)
	tick(.95, 180, true, 3)
	want(162)
	tick(.95, 162, true, 3)
	want(145.8)
	tick(.80, 145, true, 4)
	want(145.8)
	tick(.99, 100, true, 100)
	want(10)
	tick(.6, 2, true, 3)
	want(11)
	tick(.6, 1, false, 4)
	want(11)
	tick(.6, -1, false, 6)
	want(11) // rereading one sample cannot release a limit
	tick(.6, 1, false, 5)
	want(0)
	tick(.99, -1, false, 3)
	want(45)
	ct.shutdown()
	want(0)
}

func TestTrafficSampleFreshness(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rate")
	ct := newCPUThrottle(testThrottleConfig(file), slog.Default(), "", file)
	now := time.Now()
	write := func(at time.Time, bps int64) {
		if err := os.WriteFile(file+".traffic", []byte(fmt.Sprintf("%d %d 0", at.UnixNano(), bps)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(now.Add(-5*time.Second), 100)
	if ct.traffic(now).fresh {
		t.Fatal("accepted stale sample")
	}
	write(now, 0)
	if s := ct.traffic(now); !s.fresh || s.mbps != 0 {
		t.Fatal(s)
	}
	if ct.traffic(now).fresh {
		t.Fatal("duplicate sample")
	}
	write(now.Add(time.Second), 125000)
	if s := ct.traffic(now.Add(time.Second)); !s.fresh || s.mbps != 1 {
		t.Fatal("new core sample", s)
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

	if err := os.WriteFile(patchedBin, []byte("binary data with XRAY_RATELIMIT_FILE_V2 embedded inside"), 0o755); err != nil {
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

func TestLegacyCoreDoesNotKeepFallbackLimit(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "legacy-xray")
	if err := os.WriteFile(binary, []byte("XRAY_RATELIMIT_FILE"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "limit")
	ct := newCPUThrottle(testThrottleConfig(path), slog.Default(), "", path)
	for i := 0; i < 3; i++ {
		ct.stepLocked(.99, false)
	}
	if readControlFile(t, path) == 0 {
		t.Fatal("missing pre-switch limit")
	}
	ct.SetSupported(HasRateLimitSupport(binary))
	for _, cpu := range []float64{.99, .99, .99, .30, .30, .30, .30, .30} {
		ct.stepLocked(cpu, false)
		if ct.active || readControlFile(t, path) != 0 {
			t.Fatal("legacy core retained an adaptive limit")
		}
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
