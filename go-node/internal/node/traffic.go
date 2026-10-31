package node

import (
	"context"
	"strings"
	"sync"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// trafficKey is one exported traffic series: kind is inbound, outbound or
// user; tag is the inbound/outbound tag ("" for users, which are summed so
// the metrics carry no per-user labels); dir is uplink or downlink.
type trafficKey struct {
	kind, tag, dir string
}

func parseTrafficStat(name string) (trafficKey, bool) {
	p := strings.Split(name, ">>>")
	if len(p) != 4 || p[2] != "traffic" || (p[3] != "uplink" && p[3] != "downlink") {
		return trafficKey{}, false
	}
	switch p[0] {
	case "inbound", "outbound":
		return trafficKey{p[0], p[1], p[3]}, true
	case "user":
		return trafficKey{"user", "", p[3]}, true
	}
	return trafficKey{}, false
}

// trafficTracker turns xray's counters, which the panel zeroes with
// reset:true on every poll, into monotonic totals for Prometheus without
// touching what the panel gets.
//
// Every QueryStats goes through it. A reset read hands the drained values
// to acc; a plain read only remembers the live value (last). The total of a
// series is acc + Σ last, so it never goes down: when a counter shrinks or
// disappears (xray restarted, user removed) its last value moves to acc
// first. Traffic between the last read and an xray restart is lost, as it
// is for the panel. mu spans the gRPC call, so a scrape and a panel reset
// can't interleave between reading and accounting.
type trafficTracker struct {
	mu   sync.Mutex
	acc  map[trafficKey]int64
	last map[string]int64 // raw counter name → last value read without reset
}

func newTrafficTracker() *trafficTracker {
	return &trafficTracker{acc: map[trafficKey]int64{}, last: map[string]int64{}}
}

// query is api.QueryStats with the results accounted for.
func (t *trafficTracker) query(ctx context.Context, api *xray.API, pattern string, reset bool) ([]xray.Stat, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stats, err := api.QueryStats(ctx, pattern, reset)
	if err != nil {
		return nil, err
	}
	t.observe(pattern, stats, reset)
	return stats, nil
}

// observe accounts one QueryStats answer for pattern (substring match, as
// xray does it). Callers hold mu.
func (t *trafficTracker) observe(pattern string, stats []xray.Stat, reset bool) {
	seen := make(map[string]struct{}, len(stats))
	for _, s := range stats {
		k, ok := parseTrafficStat(s.Name)
		if !ok {
			continue
		}
		seen[s.Name] = struct{}{}
		if prev := t.last[s.Name]; s.Value < prev {
			// The counter restarted since we last read it.
			t.acc[k] += prev
			delete(t.last, s.Name)
		}
		switch {
		case reset:
			t.acc[k] += s.Value
			delete(t.last, s.Name)
		case s.Value == 0:
			delete(t.last, s.Name)
		default:
			t.last[s.Name] = s.Value
		}
	}
	for name, prev := range t.last {
		if _, ok := seen[name]; ok || !strings.Contains(name, pattern) {
			continue
		}
		k, _ := parseTrafficStat(name)
		t.acc[k] += prev
		delete(t.last, name)
	}
}

// totals is the monotonic value of every series seen so far.
func (t *trafficTracker) totals() map[trafficKey]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[trafficKey]int64, len(t.acc))
	for k, v := range t.acc {
		out[k] += v
	}
	for name, v := range t.last {
		k, _ := parseTrafficStat(name)
		out[k] += v
	}
	return out
}

// queryTraffic is XAPI.QueryTraffic for the stats routes; with metrics on,
// it goes through the tracker so reset:true doesn't break the totals.
func (n *Node) queryTraffic(ctx context.Context, pattern string, reset bool) ([]xray.Traffic, error) {
	if n.traffic == nil {
		return n.XAPI.QueryTraffic(ctx, pattern, reset)
	}
	stats, err := n.traffic.query(ctx, n.XAPI, pattern, reset)
	if err != nil {
		return nil, err
	}
	return xray.AggregateTraffic(stats), nil
}
