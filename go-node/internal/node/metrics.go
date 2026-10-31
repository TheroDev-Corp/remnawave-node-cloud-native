package node

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metrics: an opt-in Prometheus endpoint (METRICS_PORT, plain HTTP, its
// own port). It is Go-only and not on NODE_PORT, so the panel sees nothing
// new. Everything comes from xray's gRPC API and the node's own state;
// there are no per-user labels, only counts and sums, so cardinality grows
// with inbounds, outbounds and balancers, not with users.

const metricsScrapeTimeout = 10 * time.Second

// MetricsHandler serves GET /metrics in the Prometheus text format 0.0.4.
func (n *Node) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), metricsScrapeTimeout)
		defer cancel()
		body := n.CollectMetrics(ctx)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
	return mux
}

// CollectMetrics renders one scrape. Scrapes are serialized; each reads
// xray without reset, so the panel's counters are untouched.
func (n *Node) CollectMetrics(ctx context.Context) []byte {
	n.metricsMu.Lock()
	defer n.metricsMu.Unlock()
	w := &promWriter{}
	collectors := map[string]bool{}

	// Node and HA.
	xver := ""
	if v := n.XrayVersion(); v != nil {
		xver = *v
	}
	w.family("remnanode_info", "gauge", "Node build: node version and xray-core version.")
	w.sample("remnanode_info", 1, "version", Version, "xray_version", xver)
	w.family("remnanode_ha_leader", "gauge", "1 if this pod holds the leader lease (gets panel and user traffic), 0 on a standby.")
	w.sample("remnanode_ha_leader", b2f(n.HA.IsLeader()))
	w.family("remnanode_ha_draining", "gauge", "1 while a released leader drains before handing off.")
	w.sample("remnanode_ha_draining", b2f(n.HA.IsDraining()))

	// xray process.
	st := n.Proc.Status()
	starts, crashes := n.Proc.Counts()
	w.family("remnanode_xray_process_up", "gauge", "1 if the xray child process is running.")
	w.sample("remnanode_xray_process_up", b2f(st.Up))
	w.family("remnanode_xray_starts_total", "counter", "xray launches by this node process (panel pushes, restarts, failovers).")
	w.sample("remnanode_xray_starts_total", float64(starts))
	w.family("remnanode_xray_crashes_total", "counter", "xray exits the node did not ask for (crash, OOM kill, external signal).")
	w.sample("remnanode_xray_crashes_total", float64(crashes))

	sys, err := n.XAPI.SysStats(ctx)
	up := err == nil && st.Up
	w.family("remnanode_xray_up", "gauge", "1 if xray answers on its gRPC API.")
	w.sample("remnanode_xray_up", b2f(up))
	w.family("remnanode_xray_online", "gauge", "1 if the node considers xray started and ready (what the panel's healthcheck reports).")
	w.sample("remnanode_xray_online", b2f(n.IsOnline()))
	if up {
		w.family("remnanode_xray_uptime_seconds", "gauge", "Seconds since xray started.")
		w.sample("remnanode_xray_uptime_seconds", float64(sys.Uptime))
		w.family("remnanode_xray_goroutines", "gauge", "Goroutines in xray.")
		w.sample("remnanode_xray_goroutines", float64(sys.NumGoroutine))
		w.family("remnanode_xray_memory_alloc_bytes", "gauge", "Heap bytes allocated by xray (Go runtime Alloc).")
		w.sample("remnanode_xray_memory_alloc_bytes", float64(sys.Alloc))
		w.family("remnanode_xray_memory_sys_bytes", "gauge", "Bytes xray obtained from the OS (Go runtime Sys).")
		w.sample("remnanode_xray_memory_sys_bytes", float64(sys.Sys))
		w.family("remnanode_xray_gc_total", "counter", "Completed GC cycles in xray.")
		w.sample("remnanode_xray_gc_total", float64(sys.NumGC))
	}

	// Users the node manages (per inbound) and users online.
	cfg := n.State.XrayConfig()
	w.family("remnanode_inbound_users", "gauge", "Users configured on each inbound, as the node tracks them.")
	users := n.State.InboundUsers()
	for _, tag := range sortedKeys(users) {
		w.sample("remnanode_inbound_users", float64(users[tag]), "tag", tag)
	}
	if up {
		if online, err := n.XAPI.OnlineUsers(ctx); err == nil {
			collectors["online"] = true
			w.family("remnanode_xray_online_users", "gauge", "Users with at least one live connection (xray online map).")
			w.sample("remnanode_xray_online_users", float64(online))
		} else {
			collectors["online"] = false
		}
	}

	// Traffic: read without reset (the panel resets on its own polls).
	if up && n.traffic != nil {
		ok := true
		for _, p := range []string{"inbound>>>", "outbound>>>", "user>>>"} {
			if _, err := n.traffic.query(ctx, n.XAPI, p, false); err != nil {
				ok = false
			}
		}
		collectors["traffic"] = ok
	}
	if n.traffic != nil {
		n.writeTraffic(w, n.traffic.totals())
	}

	// Observatory and balancers (leastLoad / leastPing health).
	if up && cfg != nil {
		if cfg["observatory"] != nil || cfg["burstObservatory"] != nil {
			collectors["observatory"] = n.writeObservatory(ctx, w)
		}
		if bals := balancersOf(cfg); len(bals) > 0 {
			collectors["balancers"] = n.writeBalancers(ctx, w, bals, outboundTagsOf(cfg))
		}
	}

	w.family("remnanode_metrics_collector_success", "gauge", "1 if the collector read its data from xray in this scrape.")
	for _, c := range sortedKeys(collectors) {
		w.sample("remnanode_metrics_collector_success", b2f(collectors[c]), "collector", c)
	}
	return w.Bytes()
}

func (n *Node) writeTraffic(w *promWriter, totals map[trafficKey]int64) {
	keys := make([]trafficKey, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.tag != b.tag {
			return a.tag < b.tag
		}
		return a.dir < b.dir
	})
	fam := map[string]string{
		"inbound":  "remnanode_xray_inbound_traffic_bytes_total",
		"outbound": "remnanode_xray_outbound_traffic_bytes_total",
		"user":     "remnanode_xray_user_traffic_bytes_total",
	}
	help := map[string]string{
		"inbound":  "Bytes through each inbound since the node started (uplink = client to server).",
		"outbound": "Bytes through each outbound since the node started.",
		"user":     "Bytes of all users together since the node started (no per-user labels).",
	}
	for _, kind := range []string{"inbound", "outbound", "user"} {
		w.family(fam[kind], "counter", help[kind])
		for _, k := range keys {
			if k.kind != kind {
				continue
			}
			if kind == "user" {
				w.sample(fam[kind], float64(totals[k]), "direction", k.dir)
			} else {
				w.sample(fam[kind], float64(totals[k]), "tag", k.tag, "direction", k.dir)
			}
		}
	}
}

func (n *Node) writeObservatory(ctx context.Context, w *promWriter) bool {
	sts, err := n.XAPI.OutboundStatus(ctx)
	if err != nil {
		return false
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i].OutboundTag < sts[j].OutboundTag })
	w.family("remnanode_xray_observatory_alive", "gauge", "1 if the observatory's last probe through the outbound succeeded.")
	for _, s := range sts {
		w.sample("remnanode_xray_observatory_alive", b2f(s.Alive), "outbound", s.OutboundTag)
	}
	w.family("remnanode_xray_observatory_delay_seconds", "gauge", "Probe RTT of a live outbound (burstObservatory: average over its sampling window).")
	for _, s := range sts {
		if !s.Alive {
			continue
		}
		// Delay is whole milliseconds; burst results carry the exact average.
		d := float64(s.Delay) / 1e3
		if h := s.HealthPing; h != nil && h.Average > 0 {
			d = float64(h.Average) / 1e9
		}
		w.sample("remnanode_xray_observatory_delay_seconds", d, "outbound", s.OutboundTag)
	}
	w.family("remnanode_xray_observatory_last_seen_timestamp_seconds", "gauge", "Unix time of the last successful probe.")
	for _, s := range sts {
		if s.LastSeenTime > 0 {
			w.sample("remnanode_xray_observatory_last_seen_timestamp_seconds", float64(s.LastSeenTime), "outbound", s.OutboundTag)
		}
	}
	w.family("remnanode_xray_observatory_last_try_timestamp_seconds", "gauge", "Unix time of the last probe attempt.")
	for _, s := range sts {
		if s.LastTryTime > 0 {
			w.sample("remnanode_xray_observatory_last_try_timestamp_seconds", float64(s.LastTryTime), "outbound", s.OutboundTag)
		}
	}
	// burstObservatory only: the sampling window behind leastLoad's choice.
	var burst bool
	for _, s := range sts {
		burst = burst || s.HealthPing != nil
	}
	if !burst {
		return true
	}
	w.family("remnanode_xray_observatory_window_probes", "gauge", "burstObservatory: probes in the current sampling window.")
	for _, s := range sts {
		if h := s.HealthPing; h != nil {
			w.sample("remnanode_xray_observatory_window_probes", float64(h.All), "outbound", s.OutboundTag)
		}
	}
	w.family("remnanode_xray_observatory_window_failures", "gauge", "burstObservatory: failed probes in the current sampling window.")
	for _, s := range sts {
		if h := s.HealthPing; h != nil {
			w.sample("remnanode_xray_observatory_window_failures", float64(h.Fail), "outbound", s.OutboundTag)
		}
	}
	w.family("remnanode_xray_observatory_rtt_seconds", "gauge", "burstObservatory: RTT statistics of the sampling window (stat = average, min, max, deviation).")
	for _, s := range sts {
		h := s.HealthPing
		if h == nil || h.All == h.Fail {
			continue
		}
		for _, x := range []struct {
			stat string
			ns   int64
		}{{"average", h.Average}, {"deviation", h.Deviation}, {"max", h.Max}, {"min", h.Min}} {
			w.sample("remnanode_xray_observatory_rtt_seconds", float64(x.ns)/1e9, "outbound", s.OutboundTag, "stat", x.stat)
		}
	}
	return true
}

type balancerConf struct {
	tag, strategy, fallback string
	selector                []string
}

func balancersOf(cfg map[string]any) []balancerConf {
	routing, _ := cfg["routing"].(map[string]any)
	raw, _ := routing["balancers"].([]any)
	var out []balancerConf
	for _, r := range raw {
		m, _ := r.(map[string]any)
		tag, _ := m["tag"].(string)
		if tag == "" {
			continue
		}
		b := balancerConf{tag: tag, strategy: "random"}
		if s, ok := m["strategy"].(map[string]any); ok {
			if t, _ := s["type"].(string); t != "" {
				b.strategy = t
			}
		}
		b.fallback, _ = m["fallbackTag"].(string)
		sel, _ := m["selector"].([]any)
		for _, x := range sel {
			if s, ok := x.(string); ok {
				b.selector = append(b.selector, s)
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].tag < out[j].tag })
	return out
}

func outboundTagsOf(cfg map[string]any) []string {
	raw, _ := cfg["outbounds"].([]any)
	var out []string
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			if t, _ := m["tag"].(string); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// candidates counts outbounds a balancer selects from: xray matches the
// selector entries as tag prefixes.
func (b balancerConf) candidates(tags []string) int {
	n := 0
	for _, t := range tags {
		for _, s := range b.selector {
			if strings.HasPrefix(t, s) {
				n++
				break
			}
		}
	}
	return n
}

func (n *Node) writeBalancers(ctx context.Context, w *promWriter, bals []balancerConf, outbounds []string) bool {
	w.family("remnanode_xray_balancer_info", "gauge", "One series per balancer with its strategy and fallback outbound.")
	for _, b := range bals {
		w.sample("remnanode_xray_balancer_info", 1, "balancer", b.tag, "strategy", b.strategy, "fallback", b.fallback)
	}
	w.family("remnanode_xray_balancer_candidates", "gauge", "Outbounds in the config that match the balancer's selector.")
	for _, b := range bals {
		w.sample("remnanode_xray_balancer_candidates", float64(b.candidates(outbounds)), "balancer", b.tag)
	}
	type info struct {
		targets  []string
		override string
	}
	got := map[string]info{}
	ok := true
	for _, b := range bals {
		t, o, err := n.XAPI.BalancerInfo(ctx, b.tag)
		if err != nil {
			ok = false
			continue
		}
		sort.Strings(t)
		got[b.tag] = info{t, o}
	}
	w.family("remnanode_xray_balancer_targets", "gauge", "Outbounds the strategy currently picks from (leastLoad: the selected set, leastPing: 1; 0 means requests go to the fallback).")
	for _, b := range bals {
		if i, found := got[b.tag]; found {
			w.sample("remnanode_xray_balancer_targets", float64(len(i.targets)), "balancer", b.tag)
		}
	}
	w.family("remnanode_xray_balancer_target", "gauge", "1 for each outbound the balancer currently picks from.")
	for _, b := range bals {
		for _, t := range got[b.tag].targets {
			w.sample("remnanode_xray_balancer_target", 1, "balancer", b.tag, "outbound", t)
		}
	}
	w.family("remnanode_xray_balancer_override", "gauge", "1 for the outbound a balancer is manually pinned to (OverrideBalancerTarget).")
	for _, b := range bals {
		if o := got[b.tag].override; o != "" {
			w.sample("remnanode_xray_balancer_override", 1, "balancer", b.tag, "outbound", o)
		}
	}
	return ok
}

// promWriter writes the text exposition format. Families must be written
// whole, header first.
type promWriter struct {
	buf bytes.Buffer
}

func (w *promWriter) family(name, typ, help string) {
	fmt.Fprintf(&w.buf, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
}

// sample writes one line; labels are name/value pairs.
func (w *promWriter) sample(name string, v float64, labels ...string) {
	w.buf.WriteString(name)
	if len(labels) > 0 {
		w.buf.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.buf.WriteByte(',')
			}
			w.buf.WriteString(labels[i])
			w.buf.WriteString(`="`)
			w.buf.WriteString(escapeLabel(labels[i+1]))
			w.buf.WriteByte('"')
		}
		w.buf.WriteByte('}')
	}
	w.buf.WriteByte(' ')
	w.buf.WriteString(formatFloat(v))
	w.buf.WriteByte('\n')
}

func (w *promWriter) Bytes() []byte { return w.buf.Bytes() }

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
