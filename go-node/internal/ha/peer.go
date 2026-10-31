package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Local is what peer sync needs from the node.
type Local interface {
	IsXrayReady() bool
	IsStarting() bool
	LastStart() json.RawMessage
	SetLastStart(json.RawMessage)
	XrayConfig() map[string]any
	SetXrayConfig(map[string]any)
	PluginSnapshot() json.RawMessage
	// Journal is the user operations applied since the last full start.
	Journal() []Action
	// StartFrom is startXray(body, '127.0.0.1').
	StartFrom(ctx context.Context, body json.RawMessage)
	// Apply runs a replicated action locally without replicating it again.
	Apply(ctx context.Context, action string, body json.RawMessage) error
}

// Action is TPeerReplicationAction on the wire.
type Action struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// Snapshot is IPeerSnapshotResponse, plus the active plugin (the TS fork
// never handed it to a catching-up standby).
type Snapshot struct {
	XrayConfig       map[string]any  `json:"xrayConfig"`
	StartXrayRequest json.RawMessage `json:"startXrayRequest"`
	XrayUp           bool            `json:"xrayUp"`
	Plugin           json.RawMessage `json:"plugin,omitempty"`
	// Journal: user operations since the start request (Go peers only).
	Journal []Action `json:"journal,omitempty"`
}

// PeerSync is PeerSyncService: the leader replicates state changes to the
// other pods behind the headless Service; a standby catches up from a
// snapshot, keeps a hot xray, and asks an older leader to hand over during a
// rollout.
type PeerSync struct {
	log      *slog.Logger
	local    Local
	el       *Elector
	headless string
	ns       string
	port     int
	client   *peerClient
	auth     *PeerAuth

	// Resolve and LocalIPs are swappable for tests.
	Resolve  func(ctx context.Context, host string) ([]string, error)
	LocalIPs func() map[string]bool
	scheme   string

	queue         chan Action
	stop          chan struct{}
	shuttingDown  atomic.Bool
	catchingUp    atomic.Bool
	recovering    atomic.Bool
	requestedYeld atomic.Bool
	yieldMu       sync.Mutex
	yieldTimer    *time.Timer
}

func NewPeerSync(log *slog.Logger, local Local, el *Elector, headless, namespace string, port int, auth *PeerAuth, tlsCfg *PeerTLS) *PeerSync {
	p := &PeerSync{
		log: log, local: local, el: el, headless: headless, ns: namespace, port: port,
		auth: auth, client: newPeerClient(auth, tlsCfg), scheme: "https",
		queue: make(chan Action, 1024), stop: make(chan struct{}),
		Resolve: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
		LocalIPs: localIPs,
	}
	if el != nil {
		el.RegisterHealthGate(p)
		el.OnPromoted = func() { p.OnPromotedToLeader(context.Background()) }
	}
	return p
}

// SetPlainHTTP makes peer calls over http:// (tests only).
func (p *PeerSync) SetPlainHTTP() { p.scheme = "http"; p.client.http = &http.Client{} }

func localIPs() map[string]bool {
	out := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

func (p *PeerSync) isLeader() bool { return p.el == nil || p.el.IsLeader() }

// Start runs the ordered replication sender and, on followers, the catch-up loop.
func (p *PeerSync) Start(ctx context.Context) {
	go p.sender()
	if !p.isLeader() {
		go p.CatchUpFromLeader(ctx)
	}
	go func() {
		t := time.NewTicker(2500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.followerCheck(context.Background())
			}
		}
	}()
}

func (p *PeerSync) Shutdown() {
	if p.shuttingDown.CompareAndSwap(false, true) {
		close(p.stop)
		p.yieldMu.Lock()
		if p.yieldTimer != nil {
			p.yieldTimer.Stop()
		}
		p.yieldMu.Unlock()
	}
}

func (p *PeerSync) url(ip, path string) string {
	return p.scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(p.port)) + path
}

func (p *PeerSync) hostname() string {
	if strings.Contains(p.headless, ".") {
		return p.headless
	}
	return p.headless + "." + p.ns + ".svc.cluster.local"
}

// DiscoverPeers resolves the headless Service and drops this pod's own IPs.
func (p *PeerSync) DiscoverPeers(ctx context.Context) []string {
	if p.headless == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := p.Resolve(ctx, p.hostname())
	if err != nil {
		if addrs, err = p.Resolve(ctx, p.headless); err != nil {
			p.log.Warn(fmt.Sprintf("[PEER-SYNC] Failed to resolve peer addresses for %q: %v", p.hostname(), err))
			return nil
		}
	}
	own := p.LocalIPs()
	var out []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil && !own[a] {
			out = append(out, a)
		}
	}
	return out
}

// Replicate implements node.Replicator: queue the action for the ordered sender.
func (p *PeerSync) Replicate(action string, body any) {
	if p.headless == "" {
		return
	}
	raw, ok := body.(json.RawMessage)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	select {
	case p.queue <- Action{Type: action, Body: append(json.RawMessage(nil), raw...)}:
	default:
		p.log.Warn(fmt.Sprintf("[PEER-SYNC] Replication queue full, dropping %q (followers will catch up from a snapshot)", action))
	}
}

// sender delivers actions one at a time, in order, to every peer.
func (p *PeerSync) sender() {
	for {
		select {
		case <-p.stop:
			return
		case a := <-p.queue:
			raw, _ := json.Marshal(a)
			peers := p.DiscoverPeers(context.Background())
			if len(peers) == 0 {
				p.log.Debug(fmt.Sprintf("[PEER-SYNC] No peer replicas discovered to replicate %q (ensure publishNotReadyAddresses is true on headless service).", a.Type))
			}
			for _, ip := range peers {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.url(ip, "/internal/peer-sync/apply"), nil)
				resp, err := p.client.do(req, raw)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode/100 == 2 {
						p.log.Info(fmt.Sprintf("[PEER-SYNC] Replicated action %q to peer %s", a.Type, ip))
					} else {
						err = fmt.Errorf("HTTP %d", resp.StatusCode)
					}
				}
				if err != nil {
					p.log.Warn(fmt.Sprintf("[PEER-SYNC] Failed to replicate %q to peer %s: %v", a.Type, ip, err))
				}
				cancel()
			}
		}
	}
}

// ApplyReplicated is applyReplicatedAction on a follower.
func (p *PeerSync) ApplyReplicated(ctx context.Context, a Action) error {
	p.log.Info(fmt.Sprintf("[PEER-SYNC] Applying replicated action: %q", a.Type))
	switch a.Type {
	case "startXray":
		p.local.SetLastStart(a.Body)
		if p.local.IsStarting() {
			p.log.Debug("[PEER-SYNC] Local Xray startup is already in progress; deferring handoff until startup completes.")
			return nil
		}
		p.local.StartFrom(ctx, a.Body)
		if p.local.IsXrayReady() {
			p.onLocalXrayReady(ctx)
		}
	case "addUser", "addUsers":
		if !p.local.IsXrayReady() {
			p.log.Warn(fmt.Sprintf("[PEER-SYNC] Received %s but Xray is not running yet. Triggering catch-up...", a.Type))
			go p.CatchUpFromLeader(context.Background())
			return nil
		}
		return p.local.Apply(ctx, a.Type, a.Body)
	case "removeUser", "removeUsers":
		if p.local.IsXrayReady() {
			return p.local.Apply(ctx, a.Type, a.Body)
		}
	case "suspendUser", "restoreUser", "pluginSync":
		return p.local.Apply(ctx, a.Type, a.Body)
	default:
		p.log.Warn("Unknown peer sync action: " + a.Type)
	}
	return nil
}

func (p *PeerSync) ExportSnapshot() Snapshot {
	return Snapshot{XrayConfig: p.local.XrayConfig(), StartXrayRequest: p.local.LastStart(), XrayUp: p.local.IsXrayReady(),
		Plugin: p.local.PluginSnapshot(), Journal: p.local.Journal()}
}

type peerSnap struct {
	ip   string
	snap Snapshot
}

func (p *PeerSync) fetchSnapshot(ctx context.Context, ip string, timeout time.Duration) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.url(ip, "/internal/peer-sync/snapshot"), nil)
	resp, err := p.client.do(req, nil)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Snapshot{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var s Snapshot
	err = json.NewDecoder(resp.Body).Decode(&s)
	return s, err
}

func (p *PeerSync) fetchSnapshots(ctx context.Context) []peerSnap {
	var out []peerSnap
	for _, ip := range p.DiscoverPeers(ctx) {
		if s, err := p.fetchSnapshot(ctx, ip, 3*time.Second); err == nil {
			out = append(out, peerSnap{ip, s})
		} else {
			p.log.Debug(fmt.Sprintf("Could not fetch snapshot from peer %s: %v", ip, err))
		}
	}
	return out
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// CatchUpFromLeader takes the first peer snapshot that has a start request
// (and plugin), starts xray from it, then tries for leadership/hand-off.
func (p *PeerSync) CatchUpFromLeader(ctx context.Context) {
	if p.shuttingDown.Load() || !p.catchingUp.CompareAndSwap(false, true) {
		return
	}
	defer p.catchingUp.Store(false)
	peers := p.DiscoverPeers(ctx)
	if len(peers) == 0 {
		return
	}
	p.log.Info(fmt.Sprintf("[PEER-SYNC] Attempting catch-up sync from peers: %s...", strings.Join(peers, ", ")))
	for _, ip := range peers {
		s, err := p.fetchSnapshot(ctx, ip, 5*time.Second)
		if err != nil {
			p.log.Debug(fmt.Sprintf("Could not catch up from peer %s: %v", ip, err))
			continue
		}
		if len(s.Plugin) > 0 {
			if err := p.local.Apply(ctx, "pluginSync", s.Plugin); err != nil {
				p.log.Warn("[PEER-SYNC] Plugin from snapshot not applied: " + err.Error())
			}
		}
		if !isNull(s.StartXrayRequest) {
			p.log.Info(fmt.Sprintf("[PEER-SYNC] Successfully retrieved snapshot from %s. Starting local Xray on follower...", ip))
			p.local.SetLastStart(s.StartXrayRequest)
			if !p.local.IsStarting() {
				p.local.StartFrom(ctx, s.StartXrayRequest)
			}
			if p.local.IsXrayReady() {
				// Replay the users added/removed since that start, in order.
				for _, a := range s.Journal {
					if err := p.local.Apply(ctx, a.Type, a.Body); err != nil {
						p.log.Warn(fmt.Sprintf("[PEER-SYNC] Journal %q not applied: %v", a.Type, err))
					}
				}
				if len(s.Journal) > 0 {
					p.log.Info(fmt.Sprintf("[PEER-SYNC] Replayed %d user operations from %s's journal", len(s.Journal), ip))
				}
				p.onLocalXrayReady(ctx)
			}
			return
		}
		if len(s.XrayConfig) > 0 {
			p.log.Info(fmt.Sprintf("[PEER-SYNC] Successfully retrieved config snapshot from %s", ip))
			p.local.SetXrayConfig(s.XrayConfig)
			return
		}
	}
}

// CanAcquireLeadership: a pod with a warm peer must be warm itself first;
// on a genuine cold start (no peer has config) it may take the lease so the
// panel can connect.
func (p *PeerSync) CanAcquireLeadership(ctx context.Context) bool {
	if p.local.IsXrayReady() {
		return true
	}
	if p.local.IsStarting() || p.catchingUp.Load() {
		return false
	}
	if last := p.local.LastStart(); !isNull(last) {
		go p.ensureLocalXray(context.Background(), last)
		return false
	}
	if p.headless == "" {
		return true
	}
	for _, s := range p.fetchSnapshots(ctx) {
		if s.snap.XrayUp || !isNull(s.snap.StartXrayRequest) {
			go p.CatchUpFromLeader(context.Background())
			return false
		}
	}
	return true
}

// CheckLeaderXrayHealth detects an xray that died under a live node process.
func (p *PeerSync) CheckLeaderXrayHealth(ctx context.Context) (bool, bool) {
	last := p.local.LastStart()
	if isNull(last) || p.local.IsStarting() || p.recovering.Load() {
		return true, false
	}
	if p.local.IsXrayReady() {
		return true, false
	}
	p.log.Warn("[PEER-SYNC] 🚨 Local Xray process on Leader is DOWN (likely OOMKilled or crashed)! Checking standby peers...")
	healthyPeer := false
	for _, s := range p.fetchSnapshots(ctx) {
		if s.snap.XrayUp {
			healthyPeer = true
		}
	}
	go p.ensureLocalXray(context.Background(), last)
	return false, healthyPeer
}

func (p *PeerSync) NotifyStandbyToPromote(ctx context.Context) {
	for _, ip := range p.DiscoverPeers(ctx) {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodPost, p.url(ip, "/internal/leader/promote"), nil)
		resp, err := p.client.do(req, []byte("{}"))
		if err == nil {
			resp.Body.Close()
			p.log.Info(fmt.Sprintf("[PEER-SYNC] Notified standby peer %s to promote immediately.", ip))
		} else {
			p.log.Warn(fmt.Sprintf("[PEER-SYNC] Failed to notify standby peer %s to promote: %v", ip, err))
		}
		cancel()
	}
}

func (p *PeerSync) ensureLocalXray(ctx context.Context, body json.RawMessage) {
	if p.local.IsStarting() || p.shuttingDown.Load() || !p.recovering.CompareAndSwap(false, true) {
		return
	}
	defer p.recovering.Store(false)
	p.log.Info("[PEER-SYNC] Starting/recovering local Xray from stored config...")
	p.local.StartFrom(ctx, body)
	if p.local.IsXrayReady() && !p.shuttingDown.Load() && !p.isLeader() && p.el != nil {
		p.el.TriggerElectionNow(ctx)
	}
}

func (p *PeerSync) onLocalXrayReady(ctx context.Context) {
	if p.shuttingDown.Load() || p.isLeader() || p.el == nil {
		return
	}
	p.el.TriggerElectionNow(ctx)
	if !p.el.IsLeader() {
		go p.RequestLeadershipHandoff(context.Background(), 1)
	}
}

func (p *PeerSync) followerCheck(ctx context.Context) {
	if p.shuttingDown.Load() || p.isLeader() || p.catchingUp.Load() || p.local.IsStarting() || p.recovering.Load() {
		return
	}
	if p.local.IsXrayReady() {
		return
	}
	if last := p.local.LastStart(); !isNull(last) {
		p.log.Warn("[PEER-SYNC] Standby Xray process is not running. Restarting from cached config...")
		p.ensureLocalXray(ctx, last)
	} else {
		p.CatchUpFromLeader(ctx)
	}
}

// OnPromotedToLeader makes sure a new leader serves with a running xray.
func (p *PeerSync) OnPromotedToLeader(ctx context.Context) {
	p.log.Info("[PEER-SYNC] Pod promoted to LEADER. Ensuring local Xray state...")
	p.requestedYeld.Store(true)
	p.yieldMu.Lock()
	if p.yieldTimer != nil {
		p.yieldTimer.Stop()
		p.yieldTimer = nil
	}
	p.yieldMu.Unlock()
	if p.local.IsXrayReady() {
		return
	}
	if last := p.local.LastStart(); !isNull(last) {
		p.log.Info("[PEER-SYNC] Starting Xray on newly promoted leader using stored configuration...")
		p.local.StartFrom(ctx, last)
	} else {
		p.log.Warn("[PEER-SYNC] Promoted to leader without cached Xray config. Attempting peer catch-up or waiting for panel...")
		p.CatchUpFromLeader(ctx)
	}
}

type yieldRequest struct {
	CandidatePod          string `json:"candidatePod"`
	CandidateBootTime     *int64 `json:"candidateBootTime"`
	CandidateRestartCount int    `json:"candidateRestartCount"`
	CandidateXrayReady    *bool  `json:"candidateXrayReady"`
}

// RequestLeadershipHandoff asks the leader to yield to this (newer, hot) pod.
func (p *PeerSync) RequestLeadershipHandoff(ctx context.Context, attempt int) {
	const maxAttempts = 3
	if p.el == nil || p.requestedYeld.Load() || p.el.IsLeader() || p.el.HasBeenLeader() || p.shuttingDown.Load() {
		return
	}
	if n := p.el.RestartCount(); n > 0 {
		p.log.Debug(fmt.Sprintf("[PEER-SYNC] Skipping leadership handoff request because container has restartCount=%d (remaining hot-standby).", n))
		p.requestedYeld.Store(true)
		return
	}
	if !p.local.IsXrayReady() {
		p.log.Debug("[PEER-SYNC] Deferring leadership handoff request because local Xray is not ready yet.")
		return
	}
	peers := p.DiscoverPeers(ctx)
	if len(peers) == 0 {
		return
	}
	p.requestedYeld.Store(true)
	p.log.Info(fmt.Sprintf("[PEER-SYNC] Requesting graceful leadership handoff from leader(s): %s (attempt %d/%d)...", strings.Join(peers, ", "), attempt, maxAttempts))
	boot := p.el.StartedAt().UnixMilli()
	ready := true
	body, _ := json.Marshal(yieldRequest{CandidatePod: p.el.Identity(), CandidateBootTime: &boot, CandidateRestartCount: p.el.RestartCount(), CandidateXrayReady: &ready})
	retry := false
	for _, ip := range peers {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodPost, p.url(ip, "/internal/leader/yield"), nil)
		resp, err := p.client.do(req, body)
		if err != nil {
			cancel()
			p.log.Debug(fmt.Sprintf("[PEER-SYNC] Failed to reach peer %s for leadership yield: %v", ip, err))
			retry = true
			continue
		}
		var res YieldResult
		_ = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != 200 && resp.StatusCode != 409 {
			retry = true
			continue
		}
		switch {
		case res.Success && res.Reason != "already_follower":
			p.log.Info(fmt.Sprintf("[PEER-SYNC] 🏆 Leader %s yielded leadership. Triggering immediate election cycle to claim lease...", ip))
			p.el.TriggerElectionNow(ctx)
			return
		case res.Reason == "already_follower":
			continue
		case (res.Retryable != nil && !*res.Retryable) || res.Reason == "candidate_not_newer" || res.Reason == "candidate_restarted" || res.Reason == "already_yielded_to_candidate":
			p.log.Info(fmt.Sprintf("[PEER-SYNC] Leader %s rejected yield permanently (%s). Remaining in hot-standby mode.", ip, res.Reason))
			return
		default:
			p.log.Warn(fmt.Sprintf("[PEER-SYNC] Leader %s did not yield yet (%s).", ip, res.Reason))
			retry = true
		}
	}
	if retry && attempt < maxAttempts {
		p.log.Warn(fmt.Sprintf("[PEER-SYNC] Scheduling leadership handoff retry %d/%d in 3s...", attempt+1, maxAttempts))
		p.yieldMu.Lock()
		if p.yieldTimer != nil {
			p.yieldTimer.Stop()
		}
		p.yieldTimer = time.AfterFunc(3*time.Second, func() {
			if !p.isLeader() && !p.shuttingDown.Load() {
				p.requestedYeld.Store(false)
				p.RequestLeadershipHandoff(context.Background(), attempt+1)
			}
		})
		p.yieldMu.Unlock()
	}
}

// Register adds the peer routes to the HEALTH_PORT mux.
func (p *PeerSync) Register(mux *http.ServeMux) {
	auth := func(h func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, ok := p.auth.Verify(r)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
				return
			}
			h(w, r, body)
		}
	}
	mux.HandleFunc("POST /internal/peer-sync/apply", auth(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var a Action
		if err := json.Unmarshal(body, &a); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"status": "error", "message": err.Error()})
			return
		}
		if err := p.ApplyReplicated(r.Context(), a); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "applied"})
	}))
	mux.HandleFunc("GET /internal/peer-sync/snapshot", auth(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, http.StatusOK, p.ExportSnapshot())
	}))
	mux.HandleFunc("POST /internal/leader/yield", auth(func(w http.ResponseWriter, r *http.Request, body []byte) {
		var y yieldRequest
		if json.Unmarshal(body, &y) != nil || y.CandidatePod == "" || y.CandidateBootTime == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "candidatePod and candidateBootTime are required"})
			return
		}
		if p.el == nil {
			writeJSON(w, http.StatusOK, YieldResult{Success: true, Reason: "already_follower"})
			return
		}
		ready := y.CandidateXrayReady == nil || *y.CandidateXrayReady
		res := p.el.YieldLeadership(r.Context(), y.CandidatePod, time.UnixMilli(*y.CandidateBootTime), y.CandidateRestartCount, ready)
		code := http.StatusOK
		if !res.Success {
			code = http.StatusConflict
		}
		writeJSON(w, code, res)
	}))
	mux.HandleFunc("POST /internal/leader/promote", auth(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if p.el != nil {
			p.el.TriggerElectionNow(r.Context())
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "isLeader": p.isLeader(), "identity": p.identity()})
	}))
}

func (p *PeerSync) identity() string {
	if p.el == nil {
		return ""
	}
	return p.el.Identity()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
