package node

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
)

const (
	cpuSampleInterval       = time.Second
	engageConsecutiveTicks  = 3
	adjustIntervalTicks     = 3
	decreaseFactor          = 0.75
	releaseConsecutiveTicks = 5
	BytesPerMbps            = 125_000.0 // 1 Mbps = 1,000,000 bits/s = 125,000 bytes/s
	bytesPerMbps            = BytesPerMbps
)

var activeCPUThrottle atomic.Pointer[CPUThrottle]

// ActiveCPUThrottle returns the currently running CPUThrottle instance, or nil.
func ActiveCPUThrottle() *CPUThrottle {
	return activeCPUThrottle.Load()
}

// CPUThrottle samples host CPU usage from /proc/stat and applies an adaptive
// AIMD speed limit to Xray via an atomically updated control file.
type CPUThrottle struct {
	cfg         *config.Config
	log         *slog.Logger
	statPath    string
	controlFile string

	cgroupStatPath string
	cgroupMaxPath  string

	mu            sync.Mutex
	hasPrev       bool
	prevVals      [8]uint64
	hasPrevCgroup bool
	prevUsageUsec uint64
	prevWallTime  time.Time

	cpuRatio      float64
	active        bool
	supported     bool
	rateMbps      float64
	rateBytes     int64
	lastWritten   int64
	highStreak    int
	lowStreak     int
	releaseStreak int
	throughputFn  func() float64

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// StartCPUThrottle starts the adaptive CPU throttling controller when enabled
// in cfg. It returns nil if CPU throttling is disabled.
func StartCPUThrottle(cfg *config.Config, log *slog.Logger) *CPUThrottle {
	if cfg == nil || !cfg.CPUThrottleEnabled {
		activeCPUThrottle.Store(nil)
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	ct := newCPUThrottle(cfg, log, "/proc/stat", cfg.CPUThrottleControlFile)
	if err := ct.writeRateFile(0); err != nil {
		log.Warn("CPU throttle: failed to initialize control file", "path", ct.controlFile, "err", err)
	} else {
		ct.lastWritten = 0
	}
	if vals, ok := readProcStat(ct.statPath); ok {
		ct.hasPrev = true
		ct.prevVals = vals
	}
	if usage, ok := readCgroupStat(ct.cgroupStatPath); ok {
		ct.hasPrevCgroup = true
		ct.prevUsageUsec = usage
		ct.prevWallTime = time.Now()
	}
	activeCPUThrottle.Store(ct)

	go ct.run()
	log.Info("CPU throttle controller started",
		"high_pct", cfg.CPUThrottleHighPct,
		"low_pct", cfg.CPUThrottleLowPct,
		"rate_mbps", cfg.CPUThrottleRateMbps,
		"min_mbps", cfg.CPUThrottleMinMbps,
		"control_file", cfg.CPUThrottleControlFile,
	)
	return ct
}

func newCPUThrottle(cfg *config.Config, log *slog.Logger, statPath, controlFile string) *CPUThrottle {
	return &CPUThrottle{
		cfg:            cfg,
		log:            log,
		statPath:       statPath,
		controlFile:    controlFile,
		cgroupStatPath: "/sys/fs/cgroup/cpu.stat",
		cgroupMaxPath:  "/sys/fs/cgroup/cpu.max",
		supported:      true,
		lastWritten:    -1,
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
}

// SetThroughputFunc configures the traffic measurement callback returning
// current traffic in Mbps, or -1 if unavailable.
func (ct *CPUThrottle) SetThroughputFunc(fn func() float64) {
	if ct == nil {
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.throughputFn = fn
}

// SetCgroupPaths overrides the default cgroup v2 cpu.stat and cpu.max paths.
func (ct *CPUThrottle) SetCgroupPaths(statPath, maxPath string) {
	if ct == nil {
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.cgroupStatPath = statPath
	ct.cgroupMaxPath = maxPath
	ct.hasPrevCgroup = false
}

// SetSupported enables or disables throttling based on whether the active
// Xray core binary supports rate limiting.
func (ct *CPUThrottle) SetSupported(supported bool) {
	if ct == nil {
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.supported = supported
	if !supported {
		ct.active = false
		ct.rateMbps = 0
		ct.rateBytes = 0
		ct.highStreak = 0
		ct.lowStreak = 0
		ct.releaseStreak = 0
		if ct.lastWritten != 0 {
			_ = ct.writeRateFile(0)
			ct.lastWritten = 0
		}
		ct.log.Warn("CPU throttle: Xray core binary lacks rate limit support; throttling disabled")
	}
}

func (ct *CPUThrottle) run() {
	defer close(ct.doneCh)
	t := time.NewTicker(cpuSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ct.stopCh:
			ct.shutdown()
			return
		case <-t.C:
			ct.tick()
		}
	}
}

// Stop stops the controller goroutine and resets the control file to 0
// (unlimited). Safe to call multiple times or on a nil receiver.
func (ct *CPUThrottle) Stop() {
	if ct == nil {
		return
	}
	ct.stopOnce.Do(func() { close(ct.stopCh) })
	<-ct.doneCh
}

// Stats returns the latest sampled CPU usage ratio (0..1), whether throttling
// is currently active, and the current rate limit in bytes/sec (0 = unlimited).
func (ct *CPUThrottle) Stats() (cpuRatio float64, active bool, rateBytesPerSec int64) {
	if ct == nil {
		return 0, false, 0
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.cpuRatio, ct.active, ct.rateBytes
}

func (ct *CPUThrottle) shutdown() {
	ct.mu.Lock()
	ct.active = false
	ct.rateMbps = 0
	ct.rateBytes = 0
	err := ct.writeRateFile(0)
	if err == nil {
		ct.lastWritten = 0
	}
	ct.mu.Unlock()
	if err != nil {
		ct.log.Warn("CPU throttle: failed to reset control file on shutdown", "path", ct.controlFile, "err", err)
	}
}

func (ct *CPUThrottle) tick() {
	curVals, ok := readProcStat(ct.statPath)
	if !ok {
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	now := time.Now()
	if !ct.hasPrev {
		ct.hasPrev = true
		ct.prevVals = curVals
		if usageUsec, okStat := readCgroupStat(ct.cgroupStatPath); okStat {
			ct.hasPrevCgroup = true
			ct.prevUsageUsec = usageUsec
			ct.prevWallTime = now
		}
		return
	}

	var dTotal, dIdle uint64
	for i := 0; i < 8; i++ {
		var d uint64
		if curVals[i] > ct.prevVals[i] {
			d = curVals[i] - ct.prevVals[i]
		}
		dTotal += d
		if i == 3 || i == 4 { // idle + iowait
			dIdle += d
		}
	}
	ct.prevVals = curVals

	if dTotal == 0 {
		return
	}
	var hostRatio float64
	if dIdle < dTotal {
		hostRatio = float64(dTotal-dIdle) / float64(dTotal)
	}
	if hostRatio > 1.0 {
		hostRatio = 1.0
	}

	var cgroupAvailable bool
	var cgroupCoresUsed float64
	var cgroupQuotaRatio float64

	usageUsec, okStat := readCgroupStat(ct.cgroupStatPath)
	if okStat {
		if ct.hasPrevCgroup {
			deltaWallUsec := float64(now.Sub(ct.prevWallTime).Microseconds())
			if deltaWallUsec > 0 {
				var deltaUsageUsec float64
				if usageUsec > ct.prevUsageUsec {
					deltaUsageUsec = float64(usageUsec - ct.prevUsageUsec)
				}
				cgroupCoresUsed = deltaUsageUsec / deltaWallUsec
				cgroupAvailable = true

				if quota, period, okMax := readCgroupMax(ct.cgroupMaxPath); okMax && period > 0 && quota > 0 {
					quotaCores := quota / period
					cgroupQuotaRatio = deltaUsageUsec / (deltaWallUsec * quotaCores)
					if cgroupQuotaRatio > 1.0 {
						cgroupQuotaRatio = 1.0
					}
				}
			}
		}
		ct.hasPrevCgroup = true
		ct.prevUsageUsec = usageUsec
		ct.prevWallTime = now
	}

	effectiveRatio := hostRatio
	if cgroupQuotaRatio > effectiveRatio {
		effectiveRatio = cgroupQuotaRatio
	}
	containerShareCheck := cgroupAvailable && cgroupCoresUsed < 0.05

	ct.stepLocked(effectiveRatio, containerShareCheck)
}

func (ct *CPUThrottle) stepLocked(ratio float64, containerShareCheck bool) {
	ct.cpuRatio = ratio
	if !ct.supported {
		ct.active = false
		ct.rateBytes = 0
		return
	}
	prevActive := ct.active
	prevRateMbps := ct.rateMbps
	prevRateBytes := ct.rateBytes

	highRatio := float64(ct.cfg.CPUThrottleHighPct) / 100.0
	lowRatio := float64(ct.cfg.CPUThrottleLowPct) / 100.0
	maxRate := float64(ct.cfg.CPUThrottleRateMbps)
	minRate := float64(ct.cfg.CPUThrottleMinMbps)

	if !ct.active {
		if ratio >= highRatio {
			if containerShareCheck {
				ct.highStreak = 0
			} else {
				ct.highStreak++
				if ct.highStreak >= engageConsecutiveTicks {
					ct.active = true
					if ct.throughputFn != nil {
						measuredMbps := ct.throughputFn()
						if measuredMbps > 0 {
							ct.rateMbps = math.Min(maxRate, math.Max(minRate, measuredMbps*decreaseFactor))
						} else {
							ct.rateMbps = maxRate
						}
					} else {
						ct.rateMbps = maxRate
					}
					ct.highStreak = 0
					ct.lowStreak = 0
					ct.releaseStreak = 0
					ct.log.Info("CPU throttle engaged",
						"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
						"rate_mbps", ct.rateMbps,
					)
				}
			}
		} else {
			ct.highStreak = 0
		}
	} else {
		switch {
		case ratio >= highRatio:
			ct.lowStreak = 0
			ct.releaseStreak = 0
			if containerShareCheck {
				ct.highStreak = 0
			} else {
				ct.highStreak++
				if ct.highStreak >= adjustIntervalTicks {
					ct.highStreak = 0
					next := math.Max(minRate, ct.rateMbps*decreaseFactor)
					if next != ct.rateMbps {
						ct.rateMbps = next
						ct.log.Info("CPU throttle decreased",
							"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
							"rate_mbps", fmt.Sprintf("%.2f", ct.rateMbps),
						)
					}
				}
			}
		case ratio <= lowRatio:
			ct.highStreak = 0
			ct.lowStreak++

			measuredMbps := float64(-1)
			if ct.throughputFn != nil {
				measuredMbps = ct.throughputFn()
			}

			saturated := measuredMbps >= 0 && measuredMbps >= ct.rateMbps*0.80
			if saturated {
				ct.releaseStreak = 0
			} else {
				if measuredMbps >= 0 || ct.rateMbps >= maxRate {
					ct.releaseStreak++
				}
			}

			if ct.releaseStreak >= releaseConsecutiveTicks {
				ct.active = false
				ct.rateMbps = 0
				ct.lowStreak = 0
				ct.releaseStreak = 0
				ct.log.Info("CPU throttle released",
					"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
				)
			} else if ct.rateMbps < maxRate && ct.lowStreak >= adjustIntervalTicks {
				ct.lowStreak = 0
				stepUp := math.Max(5.0, maxRate*0.10)
				ct.rateMbps = math.Min(maxRate, ct.rateMbps+stepUp)
				ct.log.Info("CPU throttle increased",
					"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
					"rate_mbps", fmt.Sprintf("%.2f", ct.rateMbps),
				)
			}
		default:
			// Hysteresis deadband: hold current rate steady.
			ct.highStreak = 0
			ct.lowStreak = 0
			ct.releaseStreak = 0
		}
	}

	var targetBytes int64
	if ct.active {
		targetBytes = int64(math.Round(ct.rateMbps * BytesPerMbps))
	}
	if targetBytes != ct.lastWritten {
		if err := ct.writeRateFile(targetBytes); err != nil {
			ct.log.Warn("CPU throttle: failed to update control file", "path", ct.controlFile, "err", err)
			ct.active = prevActive
			ct.rateMbps = prevRateMbps
			ct.rateBytes = prevRateBytes
			return
		}
		ct.lastWritten = targetBytes
	}
	ct.rateBytes = targetBytes
}

func (ct *CPUThrottle) writeRateFile(bytesPerSec int64) error {
	if ct.controlFile == "" {
		return nil
	}
	dir := filepath.Dir(ct.controlFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rw-ratelimit-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(strconv.FormatInt(bytesPerSec, 10) + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, ct.controlFile); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// readProcStat reads the aggregate "cpu " line from /proc/stat and returns
// the first 8 numeric jiffies columns (user, nice, system, idle, iowait, irq,
// softirq, steal) as an array.
func readProcStat(path string) ([8]uint64, bool) {
	var vals [8]uint64
	b, err := os.ReadFile(path)
	if err != nil {
		return vals, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// Need at least: "cpu", user, nice, system, idle
		if len(fields) < 5 {
			return vals, false
		}
		limit := min(9, len(fields)) // up to 8 numeric columns (fields[1..8])
		for i := 1; i < limit; i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return vals, false
			}
			vals[i-1] = v
		}
		return vals, true
	}
	return vals, false
}

func readCgroupStat(path string) (uint64, bool) {
	if path == "" {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "usage_usec" {
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func readCgroupMax(path string) (quota, period float64, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 || fields[0] == "max" {
		return 0, 0, false
	}
	q, err1 := strconv.ParseFloat(fields[0], 64)
	p, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0, 0, false
	}
	return q, p, true
}

// HasRateLimitSupport checks whether the Xray core binary at binPath contains
// the rate-limiting dispatcher patch by inspecting its binary content for
// "XRAY_RATELIMIT_FILE".
func HasRateLimitSupport(binPath string) bool {
	if binPath == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		resolved = binPath
	}
	f, err := os.Open(resolved)
	if err != nil {
		return false
	}
	defer f.Close()

	needle := []byte("XRAY_RATELIMIT_FILE")
	buf := make([]byte, 64*1024)
	overlap := len(needle) - 1
	var prev []byte

	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if len(prev) > 0 {
				combined := append(prev, chunk...)
				if bytes.Contains(combined, needle) {
					return true
				}
			} else if bytes.Contains(chunk, needle) {
				return true
			}
			if n >= overlap {
				prev = append(prev[:0], chunk[n-overlap:]...)
			} else {
				prev = append(prev, chunk...)
			}
		}
		if err != nil {
			break
		}
	}
	return false
}
