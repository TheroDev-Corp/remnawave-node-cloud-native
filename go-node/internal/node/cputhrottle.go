package node

import (
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
	bytesPerMbps            = 125_000.0 // 1 Mbps = 1,000,000 bits/s = 125,000 bytes/s
)

var activeCPUThrottle atomic.Pointer[CPUThrottle]

// CPUThrottle samples host CPU usage from /proc/stat and applies an adaptive
// AIMD speed limit to Xray via an atomically updated control file.
type CPUThrottle struct {
	cfg         *config.Config
	log         *slog.Logger
	statPath    string
	controlFile string

	mu            sync.Mutex
	hasPrev       bool
	prevTotal     uint64
	prevIdle      uint64
	cpuRatio      float64
	active        bool
	rateMbps      float64
	rateBytes     int64
	lastWritten   int64
	highStreak    int
	lowStreak     int
	releaseStreak int

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
	if total, idle, ok := readProcStat(ct.statPath); ok {
		ct.hasPrev = true
		ct.prevTotal = total
		ct.prevIdle = idle
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
		cfg:         cfg,
		log:         log,
		statPath:    statPath,
		controlFile: controlFile,
		lastWritten: -1,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
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
	total, idle, ok := readProcStat(ct.statPath)
	if !ok {
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if !ct.hasPrev {
		ct.hasPrev = true
		ct.prevTotal = total
		ct.prevIdle = idle
		return
	}
	if total <= ct.prevTotal || idle < ct.prevIdle {
		ct.prevTotal = total
		ct.prevIdle = idle
		return
	}
	dTotal := total - ct.prevTotal
	dIdle := idle - ct.prevIdle
	ct.prevTotal = total
	ct.prevIdle = idle
	if dTotal == 0 {
		return
	}
	ratio := float64(dTotal-dIdle) / float64(dTotal)
	if ratio < 0 {
		ratio = 0
	} else if ratio > 1 {
		ratio = 1
	}
	ct.stepLocked(ratio)
}

func (ct *CPUThrottle) stepLocked(ratio float64) {
	ct.cpuRatio = ratio
	highRatio := float64(ct.cfg.CPUThrottleHighPct) / 100.0
	lowRatio := float64(ct.cfg.CPUThrottleLowPct) / 100.0
	maxRate := float64(ct.cfg.CPUThrottleRateMbps)
	minRate := float64(ct.cfg.CPUThrottleMinMbps)

	if !ct.active {
		if ratio >= highRatio {
			ct.highStreak++
			if ct.highStreak >= engageConsecutiveTicks {
				ct.active = true
				ct.rateMbps = maxRate
				ct.highStreak = 0
				ct.lowStreak = 0
				ct.releaseStreak = 0
				ct.log.Info("CPU throttle engaged",
					"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
					"rate_mbps", ct.rateMbps,
				)
			}
		} else {
			ct.highStreak = 0
		}
	} else {
		switch {
		case ratio >= highRatio:
			ct.lowStreak = 0
			ct.releaseStreak = 0
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
		case ratio <= lowRatio:
			ct.highStreak = 0
			ct.lowStreak++
			if ct.rateMbps >= maxRate {
				ct.releaseStreak++
				if ct.releaseStreak >= releaseConsecutiveTicks {
					ct.active = false
					ct.rateMbps = 0
					ct.lowStreak = 0
					ct.releaseStreak = 0
					ct.log.Info("CPU throttle released",
						"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
					)
				}
			} else if ct.lowStreak >= adjustIntervalTicks {
				ct.lowStreak = 0
				stepUp := math.Max(5.0, maxRate*0.10)
				ct.rateMbps = math.Min(maxRate, ct.rateMbps+stepUp)
				ct.log.Info("CPU throttle increased",
					"cpu_pct", fmt.Sprintf("%.1f", ratio*100),
					"rate_mbps", fmt.Sprintf("%.2f", ct.rateMbps),
				)
				if ct.rateMbps >= maxRate {
					ct.releaseStreak = 1
				}
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
		targetBytes = int64(math.Round(ct.rateMbps * bytesPerMbps))
	}
	ct.rateBytes = targetBytes
	if targetBytes != ct.lastWritten {
		if err := ct.writeRateFile(targetBytes); err != nil {
			ct.log.Warn("CPU throttle: failed to update control file", "path", ct.controlFile, "err", err)
		} else {
			ct.lastWritten = targetBytes
		}
	}
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
// total jiffies (user+nice+system+idle+iowait+irq+softirq+steal) and idle
// jiffies (idle+iowait).
func readProcStat(path string) (total, idle uint64, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// Need at least: "cpu", user, nice, system, idle
		if len(fields) < 5 {
			return 0, 0, false
		}
		limit := min(9, len(fields)) // up to 8 numeric columns (fields[1..8])
		var vals [8]uint64
		for i := 1; i < limit; i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals[i-1] = v
			total += v
		}
		idle = vals[3] + vals[4] // idle + iowait
		return total, idle, true
	}
	return 0, 0, false
}
