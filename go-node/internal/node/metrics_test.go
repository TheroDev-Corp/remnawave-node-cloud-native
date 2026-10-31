package node

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

func st(name string, v int64) xray.Stat { return xray.Stat{Name: name, Value: v} }

const (
	inUp   = "inbound>>>VLESS>>>traffic>>>uplink"
	inDown = "inbound>>>VLESS>>>traffic>>>downlink"
	u1Down = "user>>>1>>>traffic>>>downlink"
	u2Down = "user>>>2>>>traffic>>>downlink"
)

func total(t *trafficTracker, k trafficKey) int64 { return t.totals()[k] }

// The panel drains counters with reset:true between scrapes; the exported
// totals must keep every byte and never go down.
func TestTrafficTrackerPanelResets(t *testing.T) {
	tr := newTrafficTracker()
	kDown := trafficKey{"inbound", "VLESS", "downlink"}
	kUp := trafficKey{"inbound", "VLESS", "uplink"}

	tr.observe("inbound>>>", []xray.Stat{st(inDown, 100), st(inUp, 10)}, false) // scrape
	if got := total(tr, kDown); got != 100 {
		t.Fatalf("after scrape: %d", got)
	}
	tr.observe("inbound>>>", []xray.Stat{st(inDown, 150), st(inUp, 10)}, true) // panel reset
	if got := total(tr, kDown); got != 150 {
		t.Fatalf("after panel reset: %d", got)
	}
	tr.observe("inbound>>>", []xray.Stat{st(inDown, 30), st(inUp, 0)}, false) // scrape
	if got := total(tr, kDown); got != 180 {
		t.Fatalf("scrape after reset: %d", got)
	}
	// Per-tag reset (get-inbound-stats) behaves the same.
	tr.observe("inbound>>>VLESS>>>", []xray.Stat{st(inDown, 40), st(inUp, 5)}, true)
	if got, up := total(tr, kDown), total(tr, kUp); got != 190 || up != 15 {
		t.Fatalf("per-tag reset: down %d up %d", got, up)
	}
	if len(tr.last) != 0 {
		t.Errorf("reset counters must not stay in last: %v", tr.last)
	}
}

// xray restarts (counters start from zero) and removed users (counters
// vanish) move the last value into the total instead of dropping it.
func TestTrafficTrackerRestartAndVanish(t *testing.T) {
	tr := newTrafficTracker()
	kUsers := trafficKey{"user", "", "downlink"}
	tr.observe("user>>>", []xray.Stat{st(u1Down, 500), st(u2Down, 300)}, false)
	if got := total(tr, kUsers); got != 800 {
		t.Fatalf("users summed: %d", got)
	}
	// xray restarted: user 1 starts again from 20, user 2 not seen yet.
	tr.observe("user>>>", []xray.Stat{st(u1Down, 20)}, false)
	if got := total(tr, kUsers); got != 820 {
		t.Fatalf("after restart: %d, want 820", got)
	}
	// A reset read that finds the counter smaller (another restart) keeps
	// the 25 seen before it plus the 5 drained now.
	tr.observe("user>>>", []xray.Stat{st(u1Down, 25)}, false)
	tr.observe("user>>>", []xray.Stat{st(u1Down, 5)}, true)
	if got := total(tr, kUsers); got != 830 {
		t.Fatalf("reset after restart: %d, want 830", got)
	}
	// A per-tag query must not fold counters it didn't ask about.
	tr.observe("inbound>>>", []xray.Stat{st(inDown, 7)}, false)
	tr.observe("inbound>>>OTHER>>>", nil, false)
	if got := total(tr, trafficKey{"inbound", "VLESS", "downlink"}); got != 7 || tr.last[inDown] != 7 {
		t.Fatalf("unrelated pattern touched VLESS: total %d last %v", got, tr.last)
	}
}

func TestTrafficTrackerMonotonicRandomized(t *testing.T) {
	tr := newTrafficTracker()
	k := trafficKey{"outbound", "DIRECT", "uplink"}
	name := "outbound>>>DIRECT>>>traffic>>>uplink"
	var live, sent, prev int64
	for i := 0; i < 2000; i++ {
		step := int64(i*7919%1000 + 1)
		live += step
		sent += step
		switch i % 5 {
		case 0, 2:
			tr.observe("outbound>>>", []xray.Stat{st(name, live)}, false)
		case 1:
			tr.observe("outbound>>>", []xray.Stat{st(name, live)}, true)
			live = 0
		case 3:
			tr.observe("outbound>>>DIRECT>>>", []xray.Stat{st(name, live)}, true)
			live = 0
		}
		got := total(tr, k)
		if got < prev {
			t.Fatalf("step %d: total went down %d → %d", i, prev, got)
		}
		prev = got
	}
	tr.observe("outbound>>>", []xray.Stat{st(name, live)}, true)
	if got := total(tr, k); got != sent {
		t.Fatalf("total %d, sent %d: bytes lost or double-counted", got, sent)
	}
}

func TestParseTrafficStat(t *testing.T) {
	for name, want := range map[string]trafficKey{
		inUp:                                     {"inbound", "VLESS", "uplink"},
		"outbound>>>DIRECT>>>traffic>>>downlink": {"outbound", "DIRECT", "downlink"},
		"user>>>42>>>traffic>>>uplink":           {"user", "", "uplink"},
	} {
		if got, ok := parseTrafficStat(name); !ok || got != want {
			t.Errorf("%s: %v %v", name, got, ok)
		}
	}
	for _, name := range []string{"user>>>1>>>online", "inbound>>>x>>>traffic", "foo>>>a>>>traffic>>>uplink", "inbound>>>a>>>traffic>>>sideways"} {
		if _, ok := parseTrafficStat(name); ok {
			t.Errorf("%s must not parse", name)
		}
	}
}

func TestWithObservatoryService(t *testing.T) {
	base := map[string]any{"inbounds": []any{}, "outbounds": []any{}}
	full := GenerateAPIConfig(base, TorrentBlockerConfig{}, "@sock", "")
	withObservatoryService(full)
	if s := full["api"].(map[string]any)["services"].([]any); len(s) != 3 {
		t.Fatalf("no observatory → services unchanged (xray would refuse to start otherwise): %v", s)
	}
	for _, key := range []string{"observatory", "burstObservatory"} {
		cfg := map[string]any{key: map[string]any{"subjectSelector": []any{"p-"}}}
		full := GenerateAPIConfig(cfg, TorrentBlockerConfig{}, "@sock", "")
		withObservatoryService(full)
		withObservatoryService(full)
		got := fmt.Sprint(full["api"].(map[string]any)["services"])
		if got != "[HandlerService StatsService RoutingService ObservatoryService]" {
			t.Errorf("%s: services %s", key, got)
		}
	}
}

func TestBalancersOf(t *testing.T) {
	cfg := map[string]any{
		"outbounds": []any{
			map[string]any{"tag": "DIRECT"}, map[string]any{"tag": "relay-a"}, map[string]any{"tag": "relay-b"}, map[string]any{"tag": "BLOCK"},
		},
		"routing": map[string]any{"balancers": []any{
			map[string]any{"tag": "ll", "selector": []any{"relay-"}, "strategy": map[string]any{"type": "leastLoad"}, "fallbackTag": "DIRECT"},
			map[string]any{"tag": "any", "selector": []any{"relay-a", "BLOCK"}},
			map[string]any{"selector": []any{"x"}}, // no tag: ignored
		}},
	}
	bals := balancersOf(cfg)
	tags := outboundTagsOf(cfg)
	if len(bals) != 2 || bals[0].tag != "any" || bals[0].strategy != "random" || bals[1].strategy != "leastLoad" || bals[1].fallback != "DIRECT" {
		t.Fatalf("%+v", bals)
	}
	if bals[0].candidates(tags) != 2 || bals[1].candidates(tags) != 2 {
		t.Errorf("candidates: %d %d", bals[0].candidates(tags), bals[1].candidates(tags))
	}
}

func TestPromWriterEscaping(t *testing.T) {
	w := &promWriter{}
	w.family("x_total", "counter", "line one\nback\\slash")
	w.sample("x_total", 12345678901234, "tag", "a\"b\\c\nd")
	w.sample("x_total", 0.5)
	w.sample("x_total", 1e21)
	want := "# HELP x_total line one\\nback\\\\slash\n# TYPE x_total counter\n" +
		"x_total{tag=\"a\\\"b\\\\c\\nd\"} 12345678901234\nx_total 0.5\nx_total 1e+21\n"
	if got := string(w.Bytes()); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// checkExposition is a strict reader of the text format: every sample
// belongs to the family declared right before it, families appear once,
// names and values are well-formed. It returns sample → value.
func checkExposition(t *testing.T, body []byte) map[string]float64 {
	t.Helper()
	nameRe := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	sampleRe := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(?:[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*",?)*\})? (\S+)$`)
	seen := map[string]bool{}
	cur := ""
	out := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for line := 1; sc.Scan(); line++ {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "# HELP "):
			f := strings.SplitN(l, " ", 4)
			if !nameRe.MatchString(f[2]) || seen[f[2]] {
				t.Fatalf("line %d: bad or repeated family %q", line, l)
			}
			seen[f[2]], cur = true, f[2]
		case strings.HasPrefix(l, "# TYPE "):
			f := strings.Fields(l)
			if len(f) != 4 || f[2] != cur || (f[3] != "gauge" && f[3] != "counter") {
				t.Fatalf("line %d: TYPE %q after HELP for %q", line, l, cur)
			}
		default:
			m := sampleRe.FindStringSubmatch(l)
			if m == nil || m[1] != cur {
				t.Fatalf("line %d: sample %q outside its family %q", line, l, cur)
			}
			v, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				t.Fatalf("line %d: value %q", line, m[3])
			}
			key := m[1] + m[2]
			if _, dup := out[key]; dup {
				t.Fatalf("line %d: duplicate sample %s", line, key)
			}
			out[key] = v
		}
	}
	return out
}
