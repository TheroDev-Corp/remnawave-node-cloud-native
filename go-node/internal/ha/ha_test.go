package ha

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
)

// fakeK8s is an in-memory API server: Leases with resourceVersion
// conflicts, and pods with labels.
type fakeK8s struct {
	mu     sync.Mutex
	lease  *Lease
	rv     int
	labels map[string]map[string]string
	pods   map[string]PodInfo
}

func newFakeK8s() *fakeK8s {
	return &fakeK8s{labels: map[string]map[string]string{}, pods: map[string]PodInfo{}}
}

func (f *fakeK8s) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.Contains(r.URL.Path, "/leases"):
		switch r.Method {
		case http.MethodGet:
			if f.lease == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(f.lease)
		case http.MethodPost:
			if f.lease != nil {
				w.WriteHeader(409)
				return
			}
			var l Lease
			_ = json.Unmarshal(body, &l)
			f.rv++
			l.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			f.lease = &l
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(l)
		case http.MethodPut:
			var l Lease
			_ = json.Unmarshal(body, &l)
			if f.lease == nil || l.Metadata.ResourceVersion != f.lease.Metadata.ResourceVersion {
				w.WriteHeader(409)
				return
			}
			f.rv++
			l.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			f.lease = &l
			_ = json.NewEncoder(w).Encode(l)
		}
	case strings.Contains(r.URL.Path, "/pods/"):
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodGet:
			p := f.pods[name]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"creationTimestamp": p.CreationTimestamp, "labels": f.labels[name]},
				"status":   map[string]any{"containerStatuses": []any{map[string]any{"name": "remnanode", "restartCount": p.RestartCount}}},
			})
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/strategic-merge-patch+json" {
				w.WriteHeader(415)
				return
			}
			var patch struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			_ = json.Unmarshal(body, &patch)
			if f.labels[name] == nil {
				f.labels[name] = map[string]string{}
			}
			for k, v := range patch.Metadata.Labels {
				f.labels[name][k] = v
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		}
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeK8s) holder() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease == nil || f.lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *f.lease.Spec.HolderIdentity
}

func (f *fakeK8s) role(pod string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.labels[pod]["role"]
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fastElector(api *httptest.Server, pod string) *Elector {
	e := NewElector(quietLog(), NewK8sAt(api.URL, "t", api.Client()), "remnanode-leader", pod, "ns")
	e.LeaseDuration, e.RenewInterval, e.DrainDuration, e.ShutdownDrain = time.Second, 100*time.Millisecond, 300*time.Millisecond, 0
	return e
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestElectionFailoverAndLabels(t *testing.T) {
	f := newFakeK8s()
	api := httptest.NewServer(f)
	defer api.Close()
	a, b := fastElector(api, "pod-a"), fastElector(api, "pod-b")
	a.Start(context.Background())
	waitFor(t, "pod-a to lead", 2*time.Second, a.IsLeader)
	b.Start(context.Background())
	time.Sleep(400 * time.Millisecond)
	if b.IsLeader() || f.holder() != "pod-a" {
		t.Fatal("a valid lease must not be stolen")
	}
	waitFor(t, "labels", 2*time.Second, func() bool { return f.role("pod-a") == "leader" && f.role("pod-b") == "standby" })

	// Graceful shutdown releases the lease: b takes over at its next cycle, no expiry wait.
	t0 := time.Now()
	a.Shutdown(context.Background())
	waitFor(t, "pod-b to lead after release", 2*time.Second, b.IsLeader)
	if d := time.Since(t0); d > 800*time.Millisecond {
		t.Errorf("takeover after a released lease took %s", d)
	}
	waitFor(t, "labels after failover", 2*time.Second, func() bool { return f.role("pod-b") == "leader" && f.role("pod-a") == "standby" })

	// A leader that dies without releasing: c acquires after the lease expires
	// and strips b's stale role=leader label.
	b.mu.Lock()
	b.shuttingDown = true // stop renewing, keep the lease (SIGKILL)
	b.mu.Unlock()
	close(b.stop)
	<-b.stopped
	c := fastElector(api, "pod-c")
	c.Start(context.Background())
	if c.IsLeader() {
		t.Fatal("c must wait for the dead leader's lease to expire")
	}
	waitFor(t, "pod-c to lead after expiry", 3*time.Second, c.IsLeader)
	waitFor(t, "stale leader label cleared", 2*time.Second, func() bool { return f.role("pod-b") == "standby" && f.role("pod-c") == "leader" })
	c.Shutdown(context.Background())
}

func TestYieldLeadership(t *testing.T) {
	f := newFakeK8s()
	api := httptest.NewServer(f)
	defer api.Close()
	old := fastElector(api, "old")
	old.Start(context.Background())
	waitFor(t, "old leads", 2*time.Second, old.IsLeader)
	defer old.Shutdown(context.Background())

	ctx := context.Background()
	if r := old.YieldLeadership(ctx, "x", time.Now().Add(-time.Hour), 0, true); r.Success || r.Reason != "candidate_not_newer" {
		t.Errorf("older candidate: %+v", r)
	}
	if r := old.YieldLeadership(ctx, "x", time.Now().Add(time.Hour), 2, true); r.Success || r.Reason != "candidate_restarted" {
		t.Errorf("restarted candidate: %+v", r)
	}
	if r := old.YieldLeadership(ctx, "x", time.Now().Add(time.Hour), 0, false); r.Success || r.Reason != "candidate_xray_not_ready" || !*r.Retryable {
		t.Errorf("cold candidate: %+v", r)
	}
	newer := fastElector(api, "new")
	if r := old.YieldLeadership(ctx, "new", time.Now().Add(time.Hour), 0, true); !r.Success {
		t.Fatalf("newer hot candidate: %+v", r)
	}
	if old.IsLeader() || !old.IsDraining() || !old.IsServingTraffic() {
		t.Error("a yielding leader drains: not leader, still serving")
	}
	if f.holder() != "" {
		t.Errorf("lease must be released, holder %q", f.holder())
	}
	newer.Start(ctx)
	defer newer.Shutdown(ctx)
	waitFor(t, "new leads", 2*time.Second, newer.IsLeader)
	waitFor(t, "drain ends", 2*time.Second, func() bool { return !old.IsDraining() && f.role("old") == "standby" })
	time.Sleep(300 * time.Millisecond)
	if old.IsLeader() {
		t.Error("the old leader must not re-acquire during the suppression window")
	}
	if r := old.YieldLeadership(ctx, "new", time.Now().Add(time.Hour), 0, true); !r.Success || r.Reason != "already_follower" {
		t.Errorf("follower yield: %+v", r)
	}
}

type gate struct{ ok atomic.Bool }

func (g *gate) CanAcquireLeadership(context.Context) bool          { return g.ok.Load() }
func (g *gate) CheckLeaderXrayHealth(context.Context) (bool, bool) { return true, false }
func (g *gate) NotifyStandbyToPromote(context.Context)             {}

func TestHealthGateDefersAcquisition(t *testing.T) {
	f := newFakeK8s()
	api := httptest.NewServer(f)
	defer api.Close()
	e := fastElector(api, "cold")
	g := &gate{}
	e.RegisterHealthGate(g)
	e.Start(context.Background())
	defer e.Shutdown(context.Background())
	time.Sleep(300 * time.Millisecond)
	if e.IsLeader() || f.holder() != "" {
		t.Fatal("a pod whose gate says no must not create the lease")
	}
	g.ok.Store(true)
	waitFor(t, "lead once ready", 2*time.Second, e.IsLeader)
}

func TestRestartedContainerReleasesItsOldLease(t *testing.T) {
	f := newFakeK8s()
	api := httptest.NewServer(f)
	defer api.Close()
	// The previous container of pod-a held the lease and was SIGKILLed.
	id := "pod-a"
	f.lease = &Lease{Metadata: LeaseMetadata{Name: "remnanode-leader", Namespace: "ns", ResourceVersion: "1"},
		Spec: LeaseSpec{HolderIdentity: &id, LeaseDurationSeconds: 10, RenewTime: k8sMicroTime(time.Now())}}
	f.rv = 1
	a := fastElector(api, "pod-a")
	a.RegisterHealthGate(&gate{}) // no xray yet, a peer is warm
	a.Start(context.Background())
	defer a.Shutdown(context.Background())
	waitFor(t, "stale own lease released", 2*time.Second, func() bool { return f.holder() == "" })
	if a.IsLeader() {
		t.Error("a cold restarted container must not lead while a peer is warm")
	}
	b := fastElector(api, "pod-b")
	b.RegisterHealthGate(&gate{})
	b.gate.(*gate).ok.Store(true)
	b.Start(context.Background())
	defer b.Shutdown(context.Background())
	waitFor(t, "warm standby leads", 2*time.Second, b.IsLeader)
}

func TestPeerAuth(t *testing.T) {
	a := NewPeerAuth("secret-key-material", false)
	mk := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		a.Sign(r, []byte(body))
		return r
	}
	r := mk("POST", "/internal/peer-sync/apply", `{"type":"addUser"}`)
	if _, ok := a.Verify(r); !ok {
		t.Fatal("valid signature rejected")
	}
	// Replay of the exact same request.
	r2 := httptest.NewRequest("POST", "/internal/peer-sync/apply", strings.NewReader(`{"type":"addUser"}`))
	r2.Header = r.Header.Clone()
	if _, ok := a.Verify(r2); ok {
		t.Error("replayed request accepted")
	}
	// Tampered body / path / key.
	r3 := mk("POST", "/internal/peer-sync/apply", `{"type":"addUser"}`)
	r3.Body = io.NopCloser(strings.NewReader(`{"type":"removeUsers"}`))
	if _, ok := a.Verify(r3); ok {
		t.Error("tampered body accepted")
	}
	r4 := mk("POST", "/internal/leader/promote", `{}`)
	r4.URL.Path = "/internal/leader/yield"
	if _, ok := a.Verify(r4); ok {
		t.Error("signature for another path accepted")
	}
	other := NewPeerAuth("another-secret", false)
	r5 := httptest.NewRequest("GET", "/internal/peer-sync/snapshot", nil)
	other.Sign(r5, nil)
	if _, ok := a.Verify(r5); ok {
		t.Error("signature under another SECRET_KEY accepted")
	}
	// Stale timestamp.
	a.now = func() time.Time { return time.Now().Add(-time.Minute) }
	r6 := mk("GET", "/internal/peer-sync/snapshot", "")
	a.now = time.Now
	if _, ok := a.Verify(r6); ok {
		t.Error("request outside the time window accepted")
	}
	// The secret itself never travels; the TS header is refused unless legacy mode is on.
	if strings.Contains(fmt.Sprint(r.Header), "secret-key-material") {
		t.Error("SECRET_KEY leaked into a header")
	}
	r7 := httptest.NewRequest("GET", "/internal/peer-sync/snapshot", nil)
	r7.Header.Set("X-Secret-Key", "secret-key-material")
	if _, ok := a.Verify(r7); ok {
		t.Error("legacy X-Secret-Key accepted without PEER_ACCEPT_LEGACY_SECRET_HEADER")
	}
	r8 := httptest.NewRequest("GET", "/internal/peer-sync/snapshot", nil)
	r8.Header.Set("X-Secret-Key", "secret-key-material")
	if _, ok := NewPeerAuth("secret-key-material", true).Verify(r8); !ok {
		t.Error("legacy mode must accept TS peers")
	}
}

func peerTLS(t *testing.T) (*PeerTLS, string) {
	pki := testpki.New()
	mat, _, err := secretkey.Load(pki.SecretKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &PeerTLS{Cert: mat.NodeCert, CA: mat.CAPool}, pki.SecretKey()
}

func TestMuxListenerServesProbesAndTLS(t *testing.T) {
	pt, _ := peerTLS(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tls=%v", r.TLS != nil)
	})}
	go func() { _ = srv.Serve(pt.MuxListener(ln)) }()
	defer srv.Close()
	resp, err := http.Get("http://" + ln.Addr().String() + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "tls=false" {
		t.Errorf("plain probe: %s", b)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: pt.client()}}
	resp, err = c.Get("https://" + ln.Addr().String() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "tls=true" {
		t.Errorf("peer TLS: %s", b)
	}
	// A server with a certificate from another CA is refused by peers.
	other, _ := peerTLS(t)
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	srv2 := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = srv2.Serve(tls.NewListener(ln2, other.server())) }()
	defer srv2.Close()
	if _, err := c.Get("https://" + ln2.Addr().String() + "/x"); err == nil {
		t.Error("peer accepted a certificate from a foreign CA")
	}
}

// fakeLocal records what peer sync does to a node.
type fakeLocal struct {
	mu      sync.Mutex
	ready   bool
	last    json.RawMessage
	plugin  json.RawMessage
	applied []string
	starts  int
	journal []Action
}

func (l *fakeLocal) IsXrayReady() bool              { l.mu.Lock(); defer l.mu.Unlock(); return l.ready }
func (l *fakeLocal) IsStarting() bool               { return false }
func (l *fakeLocal) LastStart() json.RawMessage     { l.mu.Lock(); defer l.mu.Unlock(); return l.last }
func (l *fakeLocal) SetLastStart(b json.RawMessage) { l.mu.Lock(); l.last = b; l.mu.Unlock() }
func (l *fakeLocal) XrayConfig() map[string]any     { return nil }
func (l *fakeLocal) SetXrayConfig(map[string]any)   {}
func (l *fakeLocal) Journal() []Action {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.journal
}
func (l *fakeLocal) PluginSnapshot() json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.plugin
}
func (l *fakeLocal) StartFrom(context.Context, json.RawMessage) {
	l.mu.Lock()
	l.starts++
	l.ready = true
	l.mu.Unlock()
}
func (l *fakeLocal) Apply(_ context.Context, action string, body json.RawMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applied = append(l.applied, action+":"+string(body))
	if action == "pluginSync" {
		l.plugin = body
	}
	return nil
}
func (l *fakeLocal) snapshot() ([]string, int, json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.applied...), l.starts, l.plugin
}

// TestPeerSyncOverTLS runs a leader and a follower PeerSync against each
// other on real sockets: catch-up from a snapshot (start request and
// plugin), then ordered replication.
func TestPeerSyncOverTLS(t *testing.T) {
	pt, secret := peerTLS(t)
	auth := NewPeerAuth(secret, false)
	mkNode := func(local *fakeLocal) (*PeerSync, string, func()) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		ps := NewPeerSync(quietLog(), local, nil, "peers.test", "ns", 0, auth, pt)
		mux := http.NewServeMux()
		ps.Register(mux)
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(pt.MuxListener(ln)) }()
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		return ps, port, func() { srv.Close(); ps.Shutdown() }
	}
	leaderLocal := &fakeLocal{ready: true, last: json.RawMessage(`{"xrayConfig":{"a":1}}`), plugin: json.RawMessage(`{"plugin":{"name":"tb"}}`),
		journal: []Action{{Type: "addUser", Body: json.RawMessage(`{"u":3}`)}, {Type: "removeUser", Body: json.RawMessage(`{"u":1}`)}}}
	followerLocal := &fakeLocal{}
	leader, leaderPort, stopL := mkNode(leaderLocal)
	defer stopL()
	follower, followerPort, stopF := mkNode(followerLocal)
	defer stopF()
	// Both resolve the headless name to 127.0.0.1 and treat it as the peer;
	// each talks to the other's port.
	for _, c := range []struct {
		ps   *PeerSync
		port string
	}{{leader, followerPort}, {follower, leaderPort}} {
		c.ps.Resolve = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
		c.ps.LocalIPs = func() map[string]bool { return map[string]bool{} }
		c.ps.port, _ = strconv.Atoi(c.port)
	}

	follower.CatchUpFromLeader(context.Background())
	applied, starts, plugin := followerLocal.snapshot()
	if starts != 1 || string(followerLocal.LastStart()) != `{"xrayConfig":{"a":1}}` {
		t.Fatalf("catch-up must start xray from the leader's request: starts=%d last=%s", starts, followerLocal.LastStart())
	}
	if string(plugin) != `{"plugin":{"name":"tb"}}` || len(applied) != 3 {
		t.Fatalf("catch-up applies the plugin, then the journal: %v", applied)
	}
	if applied[1] != `addUser:{"u":3}` || applied[2] != `removeUser:{"u":1}` {
		t.Errorf("users changed since the last start must be replayed in order (fork fix): %v", applied)
	}

	go leader.sender()
	for i := 0; i < 20; i++ {
		leader.Replicate("addUser", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
		leader.Replicate("removeUser", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
	}
	leader.Replicate("pluginSync", json.RawMessage(`{"plugin":null}`))
	waitFor(t, "41 replicated actions", 5*time.Second, func() bool { a, _, _ := followerLocal.snapshot(); return len(a) == 44 })
	applied, _, _ = followerLocal.snapshot()
	for i := 0; i < 20; i++ {
		if applied[3+2*i] != fmt.Sprintf(`addUser:{"n":%d}`, i) || applied[4+2*i] != fmt.Sprintf(`removeUser:{"n":%d}`, i) {
			t.Fatalf("replication out of order at %d: %v", i, applied[3+2*i:5+2*i])
		}
	}
	if applied[43] != `pluginSync:{"plugin":null}` {
		t.Errorf("plugin sync must be replicated (fork fix): %v", applied[43])
	}

	// An unsigned request is refused.
	req, _ := http.NewRequest("GET", "https://127.0.0.1:"+leaderPort+"/internal/peer-sync/snapshot", nil)
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: pt.client()}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("unsigned snapshot request: %d", resp.StatusCode)
	}
}
