package node

import (
	"bufio"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SystemInfo is getSystemInfo() (node:os values, as libuv reports them).
type SystemInfo struct {
	Arch              string   `json:"arch"`
	CPUs              int      `json:"cpus"`
	CPUModel          string   `json:"cpuModel"`
	MemoryTotal       int64    `json:"memoryTotal"`
	Hostname          string   `json:"hostname"`
	Platform          string   `json:"platform"`
	Release           string   `json:"release"`
	Type              string   `json:"type"`
	Version           string   `json:"version"`
	NetworkInterfaces []string `json:"networkInterfaces"`
}

// InterfaceRate is one interface from the network-stats sampler.
type InterfaceRate struct {
	Interface     string  `json:"interface"`
	RxBytesPerSec float64 `json:"rxBytesPerSec"`
	TxBytesPerSec float64 `json:"txBytesPerSec"`
	RxTotal       int64   `json:"rxTotal"`
	TxTotal       int64   `json:"txTotal"`
}

// SystemStats is getSystemStats() plus the interface rates.
type SystemStats struct {
	MemoryFree int64          `json:"memoryFree"`
	MemoryUsed int64          `json:"memoryUsed"`
	Uptime     float64        `json:"uptime"`
	LoadAvg    []float64      `json:"loadAvg"`
	Interface  *InterfaceRate `json:"interface"`
}

func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}

func nodeType() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "darwin":
		return "Darwin"
	}
	return runtime.GOOS
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// cpuInfo counts CPUs like uv_cpu_info (cpuN lines in /proc/stat) and takes
// the first "model name".
func cpuInfo() (int, string) {
	n := 0
	for _, l := range strings.Split(readFile("/proc/stat"), "\n") {
		if strings.HasPrefix(l, "cpu") && len(l) > 3 && l[3] >= '0' && l[3] <= '9' {
			n++
		}
	}
	if n == 0 {
		n = runtime.NumCPU()
	}
	model := "unknown"
	for _, l := range strings.Split(readFile("/proc/cpuinfo"), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
			model = strings.TrimSpace(v)
			break
		}
	}
	return n, model
}

// memInfo returns MemTotal and MemAvailable in bytes (libuv's totalmem and
// freemem on Linux).
func memInfo() (total, avail int64) {
	sc := bufio.NewScanner(strings.NewReader(readFile("/proc/meminfo")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return total, avail
}

func interfaceNames() []string {
	ifs, _ := net.Interfaces()
	out := []string{}
	for _, i := range ifs {
		if addrs, err := i.Addrs(); err == nil && len(addrs) > 0 {
			out = append(out, i.Name)
		}
	}
	return out
}

var (
	infoOnce sync.Once
	info     SystemInfo
)

func GetSystemInfo() SystemInfo {
	infoOnce.Do(func() {
		cpus, model := cpuInfo()
		total, _ := memInfo()
		host, _ := os.Hostname()
		rel, ver := uname()
		info = SystemInfo{
			Arch: nodeArch(), CPUs: cpus, CPUModel: model, MemoryTotal: total, Hostname: host,
			Platform: runtime.GOOS, Release: rel, Type: nodeType(), Version: ver,
		}
	})
	i := info
	i.NetworkInterfaces = interfaceNames()
	return i
}

func GetSystemStats() SystemStats {
	total, avail := memInfo()
	up := 0.0
	if f := strings.Fields(readFile("/proc/uptime")); len(f) > 0 {
		up, _ = strconv.ParseFloat(f[0], 64)
	}
	load := []float64{0, 0, 0}
	if f := strings.Fields(readFile("/proc/loadavg")); len(f) >= 3 {
		for i := 0; i < 3; i++ {
			load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	return SystemStats{MemoryFree: avail, MemoryUsed: total - avail, Uptime: up, LoadAvg: load}
}

// NetStats is network-stats.service.ts: sample /proc/net/dev every second
// and keep per-interface rates; the default interface comes from
// /proc/net/route (lowest-metric default route).
type NetStats struct {
	mu      sync.Mutex
	prev    map[string]ifCounters
	rates   map[string]InterfaceRate
	defIf   string
	enabled bool
}

type ifCounters struct {
	rx, tx int64
	at     time.Time
}

func StartNetStats(stop <-chan struct{}) *NetStats {
	ns := &NetStats{rates: map[string]InterfaceRate{}}
	if _, err := os.Stat("/proc/net/dev"); err != nil {
		return ns
	}
	ns.enabled = true
	ns.defIf = defaultInterface()
	ns.prev = readNetDev()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ns.tick()
			}
		}
	}()
	return ns
}

func (ns *NetStats) tick() {
	now := readNetDev()
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for name, cur := range now {
		p, ok := ns.prev[name]
		if !ok {
			continue
		}
		el := cur.at.Sub(p.at).Seconds()
		if el <= 0 {
			continue
		}
		ns.rates[name] = InterfaceRate{
			Interface:     name,
			RxBytesPerSec: max(0, float64(cur.rx-p.rx)/el),
			TxBytesPerSec: max(0, float64(cur.tx-p.tx)/el),
			RxTotal:       cur.rx, TxTotal: cur.tx,
		}
	}
	ns.prev = now
}

// Default is GetInterfaceStatsQuery: the default interface's rates or nil.
func (ns *NetStats) Default() *InterfaceRate {
	if ns == nil {
		return nil
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.defIf == "" {
		return nil
	}
	r, ok := ns.rates[ns.defIf]
	if !ok {
		return nil
	}
	return &r
}

func readNetDev() map[string]ifCounters {
	out := map[string]ifCounters{}
	now := time.Now()
	lines := strings.Split(readFile("/proc/net/dev"), "\n")
	if len(lines) < 2 {
		return out
	}
	for _, l := range lines[2:] {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "" || len(f) < 16 {
			continue
		}
		rx, e1 := strconv.ParseInt(f[0], 10, 64)
		tx, e2 := strconv.ParseInt(f[8], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		out[name] = ifCounters{rx, tx, now}
	}
	return out
}

func defaultInterface() string {
	lines := strings.Split(readFile("/proc/net/route"), "\n")
	best, bestMetric := "", -1
	for _, l := range lines[min(1, len(lines)):] {
		f := strings.Fields(l)
		if len(f) < 11 || f[1] != "00000000" {
			continue
		}
		m, _ := strconv.Atoi(f[6])
		if best == "" || m < bestMetric {
			best, bestMetric = f[0], m
		}
	}
	return best
}
