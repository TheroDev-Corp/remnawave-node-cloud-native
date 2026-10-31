package node

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// TestMetricsEndToEnd runs the node with METRICS_PORT against a real xray:
// a config with a burstObservatory, a leastLoad and a leastPing balancer
// over two live outbounds and a dead one, real traffic, panel resets in
// between scrapes, and an xray crash.
func TestMetricsEndToEnd(t *testing.T) {
	bin := os.Getenv("XRAY_BINARY")
	if bin == "" {
		bin = *xrayFlag
	}
	if bin == "" {
		t.Skip("set XRAY_BINARY to run the metrics end-to-end test against a real xray")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pki := testpki.New()
	mat, _, err := secretkey.Load(pki.SecretKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{XrayBinary: bin, InternalSocketPath: "rwint-metrics", InternalRestToken: "tok", MetricsPort: 1}
	apiListen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	xapi, _ := xray.Dial(apiListen)
	proc := xray.NewProcess(bin, "", log)
	n := New(cfg, log, ha.Standalone{ID: "metrics-pod"}, proc, xapi, apiListen, nil)
	n.CoreLink = t.TempDir() + "/rw-core"
	n.RefreshXrayVersion(t.Context())
	t.Cleanup(func() { n.StopXray(false, false) })

	rt := api.NewRouter(log, api.NewJWTVerifier(mat.JWTKey), cfg.InternalRestToken)
	n.Register(rt)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := api.NewServer("", rt, api.TLSConfig(mat, false), log)
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	p := &panel{t: t, pki: pki, base: "https://" + ln.Addr().String(),
		client: &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{TLSClientConfig: pki.ClientTLS(mat.SNI)}}}

	ms := httptest.NewServer(n.MetricsHandler())
	t.Cleanup(ms.Close)
	var lastBody []byte
	scrape := func() map[string]float64 {
		t.Helper()
		resp, err := http.Get(ms.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		lastBody = body
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
			t.Fatalf("scrape: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return checkExposition(t, body)
	}
	if resp, _ := http.Post(ms.URL+"/metrics", "text/plain", nil); resp == nil || resp.StatusCode != 405 {
		t.Errorf("POST /metrics must be refused")
	}

	m := scrape()
	if m["remnanode_xray_up"] != 0 || m["remnanode_xray_starts_total"] != 0 || m["remnanode_ha_leader"] != 1 {
		t.Fatalf("before start: up=%v starts=%v leader=%v", m["remnanode_xray_up"], m["remnanode_xray_starts_total"], m["remnanode_ha_leader"])
	}
	if m[`remnanode_info{version="`+Version+`",xray_version="`+deref(n.XrayVersion())+`"}`] != 1 {
		t.Errorf("remnanode_info missing: %v", m)
	}

	// The panel's config plus three probed outbounds behind two balancers.
	const payload = 256 << 10
	target := startPayloadServer(t, payload)
	allowLoopback := map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []any{"127.0.0.1/32"}}}}
	users := []testUser{newUser(1), newUser(2)}
	xc, hashes := xrayConfigFor(t, users, b64key(16))
	xc["outbounds"] = append(xc["outbounds"].([]any),
		map[string]any{"tag": "relay-a", "protocol": "freedom", "settings": allowLoopback},
		map[string]any{"tag": "relay-b", "protocol": "freedom", "settings": allowLoopback},
		map[string]any{"tag": "relay-dead", "protocol": "socks", "settings": map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": freePort(t)}}}},
	)
	xc["burstObservatory"] = map[string]any{"subjectSelector": []any{"relay-"},
		"pingConfig": map[string]any{"destination": "http://" + target + "/", "interval": "10s", "sampling": 2, "timeout": "2s"}}
	xc["routing"] = map[string]any{"rules": []any{}, "balancers": []any{
		map[string]any{"tag": "relays-ll", "selector": []any{"relay-"}, "strategy": map[string]any{"type": "leastLoad"}, "fallbackTag": "DIRECT"},
		map[string]any{"tag": "relays-ping", "selector": []any{"relay-"}, "strategy": map[string]any{"type": "leastPing"}},
	}}
	sr := resp(p.must("POST", "/node/xray/start", "StartXrayCommand", startBody(xc, hashes, false), true, 201))
	if sr["isStarted"] != true {
		t.Fatalf("start: %v\n%s", sr, strings.Join(proc.Tail(20), "\n"))
	}
	if svc := fmt.Sprint(n.State.XrayConfig()["api"].(map[string]any)["services"]); !strings.Contains(svc, "ObservatoryService") {
		t.Fatalf("METRICS_PORT with an observatory must add ObservatoryService: %s", svc)
	}

	m = scrape()
	if m["remnanode_xray_up"] != 1 || m["remnanode_xray_process_up"] != 1 || m["remnanode_xray_online"] != 1 || m["remnanode_xray_starts_total"] != 1 {
		t.Fatalf("after start: %v", m)
	}
	for _, tag := range managedTags {
		if m[`remnanode_inbound_users{tag="`+tag+`"}`] != 2 {
			t.Errorf("inbound users %s = %v", tag, m[`remnanode_inbound_users{tag="`+tag+`"}`])
		}
	}
	if _, ok := m["remnanode_xray_online_users"]; !ok {
		t.Error("remnanode_xray_online_users missing")
	}
	if m[`remnanode_xray_balancer_info{balancer="relays-ll",strategy="leastLoad",fallback="DIRECT"}`] != 1 ||
		m[`remnanode_xray_balancer_candidates{balancer="relays-ll"}`] != 3 {
		t.Errorf("balancer info/candidates: %v", m)
	}

	// Traffic, with the panel draining the counters between scrapes.
	lan := lanIPv4()
	var through []string
	if lan != "" {
		through = []string{lan}
	}
	_, vlSocks, clientLog := startClientXray(t, bin, xc, users[0], users[1], through...)
	download := func() {
		t.Helper()
		if got, err := socksGet(vlSocks, target); err != nil || got < payload {
			t.Fatalf("download: %d %v\n%s", got, err, clientLog())
		}
		// xray counts the last buffers of a mux stream a moment later.
		time.Sleep(300 * time.Millisecond)
	}
	userDown := `remnanode_xray_user_traffic_bytes_total{direction="downlink"}`
	inDown := `remnanode_xray_inbound_traffic_bytes_total{tag="VLESS",direction="downlink"}`
	outDown := `remnanode_xray_outbound_traffic_bytes_total{tag="DIRECT",direction="downlink"}`
	download()
	m1 := scrape()
	p.must("POST", "/node/stats/get-users-stats", "GetUsersStatsCommand", map[string]any{"reset": true}, false, 201)
	p.must("POST", "/node/stats/get-combined-stats", "GetCombinedStatsCommand", map[string]any{"reset": true}, false, 201)
	m2 := scrape()
	// xray's VLESS inbound counter with mux comes out a few KiB under the
	// payload (the panel sees the same), so it gets 10 % slack.
	least := map[string]float64{userDown: payload, inDown: payload * 0.9, outDown: payload}
	for _, k := range []string{userDown, inDown, outDown} {
		if m1[k] < least[k] || m2[k] < m1[k] {
			t.Errorf("%s: %v then %v after a panel reset; want ≥ %v and not decreasing", k, m1[k], m2[k], least[k])
		}
	}
	download()
	m3 := scrape()
	for _, k := range []string{userDown, inDown, outDown} {
		if m3[k] < m2[k]+least[k] {
			t.Errorf("%s after a second download: %v, want ≥ %v", k, m3[k], m2[k]+least[k])
		}
	}
	if lan != "" && m3["remnanode_xray_online_users"] != 1 {
		t.Errorf("one user downloading from %s: online_users = %v", lan, m3["remnanode_xray_online_users"])
	}
	// Scrapes read without reset: the panel still gets the second download.
	us := resp(p.must("POST", "/node/stats/get-users-stats", "GetUsersStatsCommand", map[string]any{"reset": false}, false, 201))
	var panelDown int64
	for _, u := range us["users"].([]any) {
		d, _ := u.(map[string]any)["downlink"].(json.Number).Int64()
		panelDown += d
	}
	if panelDown < payload || panelDown >= 2*payload {
		t.Errorf("panel's users downlink after scrapes: %d, want one download (%d…%d)", panelDown, payload, 2*payload)
	}

	// Observatory: the dead relay fails its probes, the live ones answer.
	var obs map[string]float64
	deadline := time.Now().Add(20 * time.Second)
	for {
		obs = scrape()
		if obs[`remnanode_xray_observatory_alive{outbound="relay-a"}`] == 1 && obs[`remnanode_xray_observatory_alive{outbound="relay-b"}`] == 1 {
			if _, ok := obs[`remnanode_xray_observatory_alive{outbound="relay-dead"}`]; ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("observatory never reported the relays:\n%v\n%s", obs, strings.Join(proc.Tail(20), "\n"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if path := os.Getenv("METRICS_DUMP"); path != "" { // a sample scrape, for docs and dashboards
		_ = os.WriteFile(path, lastBody, 0o644)
	}
	if obs[`remnanode_xray_observatory_alive{outbound="relay-dead"}`] != 0 || obs[`remnanode_xray_observatory_window_failures{outbound="relay-dead"}`] < 1 {
		t.Errorf("relay-dead must be down with failures: %v", obs)
	}
	if d := obs[`remnanode_xray_observatory_delay_seconds{outbound="relay-a"}`]; d <= 0 || d > 2 {
		t.Errorf("relay-a delay %v s", d)
	}
	if _, ok := obs[`remnanode_xray_observatory_rtt_seconds{outbound="relay-a",stat="average"}`]; !ok {
		t.Errorf("burst RTT stats missing: %v", obs)
	}
	if obs[`remnanode_metrics_collector_success{collector="observatory"}`] != 1 || obs[`remnanode_metrics_collector_success{collector="balancers"}`] != 1 ||
		obs[`remnanode_metrics_collector_success{collector="traffic"}`] != 1 {
		t.Errorf("collectors: %v", obs)
	}
	// Balancers pick only from live relays.
	for _, bal := range []string{"relays-ll", "relays-ping"} {
		if obs[`remnanode_xray_balancer_target{balancer="`+bal+`",outbound="relay-dead"}`] != 0 {
			t.Errorf("%s picks the dead relay: %v", bal, obs)
		}
		got := obs[`remnanode_xray_balancer_target{balancer="`+bal+`",outbound="relay-a"}`] + obs[`remnanode_xray_balancer_target{balancer="`+bal+`",outbound="relay-b"}`]
		if got < 1 || obs[`remnanode_xray_balancer_targets{balancer="`+bal+`"}`] != got {
			t.Errorf("%s targets: %v", bal, obs)
		}
	}
	if obs[`remnanode_xray_balancer_targets{balancer="relays-ping"}`] != 1 {
		t.Errorf("leastPing picks exactly one: %v", obs[`remnanode_xray_balancer_targets{balancer="relays-ping"}`])
	}

	// An xray crash: counted, reported down, totals kept.
	before := scrape()
	if pr, err := os.FindProcess(proc.Status().PID); err == nil {
		_ = pr.Kill()
	}
	for deadline := time.Now().Add(5 * time.Second); proc.Status().Up && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	m = scrape()
	if m["remnanode_xray_up"] != 0 || m["remnanode_xray_process_up"] != 0 || m["remnanode_xray_crashes_total"] != 1 {
		t.Errorf("after SIGKILL: up=%v process=%v crashes=%v", m["remnanode_xray_up"], m["remnanode_xray_process_up"], m["remnanode_xray_crashes_total"])
	}
	for _, k := range []string{userDown, inDown, outDown} {
		if m[k] < before[k] {
			t.Errorf("%s dropped after the crash: %v → %v", k, before[k], m[k])
		}
	}
}

// lanIPv4 is a non-loopback IPv4 address of this host, or "".
func lanIPv4() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLinkLocalUnicast() {
				return ip.IP.String()
			}
		}
	}
	return ""
}
