package ha

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// HealthGate is ILeaderHealthGate: peer sync tells the elector whether this
// pod may take the lease, and whether a leader whose xray died should step
// down for a healthy standby.
type HealthGate interface {
	CanAcquireLeadership(ctx context.Context) bool
	CheckLeaderXrayHealth(ctx context.Context) (healthy, hasHealthyPeer bool)
	NotifyStandbyToPromote(ctx context.Context)
}

// Elector is LeaderElectionService: a coordination.k8s.io Lease renewed
// every 2 s (10 s duration), the pod labelled role=leader|standby so the
// Service only sends traffic to the leader, graceful hand-off to a newer pod
// during a rollout with a 10 s draining overlap.
type Elector struct {
	log       *slog.Logger
	k8s       *K8s
	leaseName string
	podName   string
	namespace string

	LeaseDuration   time.Duration
	RenewInterval   time.Duration
	DrainDuration   time.Duration
	ShutdownDrain   time.Duration
	OnPromoted      func() // LeaderPromotedEvent
	OnDemoted       func() // LeaderDemotedEvent
	now             func() time.Time
	labelsSupported bool

	mu              sync.Mutex
	gate            HealthGate
	leader          bool
	everLeader      bool
	draining        bool
	drainTimer      *time.Timer
	suppressUntil   time.Time
	yielded         map[string]bool
	appliedRole     string
	podCreation     time.Time
	restartCount    int
	shuttingDown    bool
	loopMu          sync.Mutex // isLoopRunning
	stop            chan struct{}
	stopped         chan struct{}
	resourceVersion string
}

func NewElector(log *slog.Logger, k8s *K8s, leaseName, podName, namespace string) *Elector {
	return &Elector{
		log: log, k8s: k8s, leaseName: leaseName, podName: podName, namespace: namespace,
		LeaseDuration: 10 * time.Second, RenewInterval: 2 * time.Second,
		DrainDuration: 10 * time.Second, ShutdownDrain: 5 * time.Second,
		now: time.Now, yielded: map[string]bool{}, podCreation: time.Now(),
		labelsSupported: true,
	}
}

// Namespace is POD_NAMESPACE, else the service-account namespace file, else "remnanode".
func Namespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile(saDir + "/namespace"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "remnanode"
}

func (e *Elector) RegisterHealthGate(g HealthGate) { e.mu.Lock(); e.gate = g; e.mu.Unlock() }

func (e *Elector) IsLeader() bool   { e.mu.Lock(); defer e.mu.Unlock(); return e.leader }
func (e *Elector) IsDraining() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.draining }
func (e *Elector) IsServingTraffic() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leader || e.draining
}
func (e *Elector) Identity() string     { return e.podName }
func (e *Elector) HasBeenLeader() bool  { e.mu.Lock(); defer e.mu.Unlock(); return e.everLeader }
func (e *Elector) StartedAt() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.podCreation }
func (e *Elector) RestartCount() int    { e.mu.Lock(); defer e.mu.Unlock(); return e.restartCount }

// Start reads the pod metadata, labels the pod standby, runs the first
// election cycle and then one every RenewInterval.
func (e *Elector) Start(ctx context.Context) {
	e.log.Info(fmt.Sprintf("Starting K8s Leader Election (pod: %s, namespace: %s, lease: %s)...", e.podName, e.namespace, e.leaseName))
	e.initPodMetadata(ctx)
	e.stop, e.stopped = make(chan struct{}), make(chan struct{})
	e.loop(ctx)
	go func() {
		defer close(e.stopped)
		t := time.NewTicker(e.RenewInterval)
		defer t.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-t.C:
				go e.loop(context.Background())
			}
		}
	}()
}

func (e *Elector) initPodMetadata(ctx context.Context) {
	st, pod, err := e.k8s.GetPod(ctx, e.namespace, e.podName)
	if err != nil {
		e.log.Warn("Failed to fetch initial pod metadata: " + err.Error())
	} else if st == 200 && pod != nil {
		e.mu.Lock()
		e.restartCount = pod.RestartCount
		if t, err := time.Parse(time.RFC3339, pod.CreationTimestamp); err == nil {
			e.podCreation = t
		}
		e.mu.Unlock()
		e.log.Info(fmt.Sprintf("Pod metadata initialized: creationTime=%s, restartCount=%d, currentRoleLabel=%s",
			e.podCreation.UTC().Format(time.RFC3339), pod.RestartCount, orNone(pod.Labels["role"])))
	}
	// Clear a stale role=leader left by a previous (OOM-killed) container.
	e.UpdatePodRoleLabel(ctx, "standby")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Shutdown releases the lease if held and drains for ShutdownDrain, like
// onApplicationShutdown.
func (e *Elector) Shutdown(ctx context.Context) {
	e.mu.Lock()
	e.shuttingDown = true
	if e.drainTimer != nil {
		e.drainTimer.Stop()
	}
	wasLeader := e.leader
	e.mu.Unlock()
	if e.stop != nil {
		close(e.stop)
		<-e.stopped
	}
	// Wait out a cycle already in flight, or it could renew after the release.
	e.loopMu.Lock()
	defer e.loopMu.Unlock()
	if wasLeader {
		e.log.Info(fmt.Sprintf("Releasing lease %q due to shutdown...", e.leaseName))
		e.releaseLease(ctx)
		e.setLeader(ctx, false)
		e.log.Info(fmt.Sprintf("Gracefully draining connections for %s before exit...", e.ShutdownDrain))
		time.Sleep(e.ShutdownDrain)
	}
}

func (e *Elector) loop(ctx context.Context) {
	if !e.loopMu.TryLock() {
		return
	}
	defer e.loopMu.Unlock()
	e.mu.Lock()
	if e.shuttingDown {
		e.mu.Unlock()
		return
	}
	skip := e.draining || (!e.leader && e.now().Before(e.suppressUntil))
	gate := e.gate
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		down := e.shuttingDown
		want := "standby"
		if e.leader || e.draining {
			want = "leader"
		}
		applied := e.appliedRole
		e.mu.Unlock()
		if !down && applied != want {
			e.UpdatePodRoleLabel(ctx, want)
		}
	}()
	if skip {
		return
	}

	st, lease, err := e.k8s.GetLease(ctx, e.namespace, e.leaseName)
	if err != nil {
		e.log.Warn("Leader election cycle failed: " + err.Error())
		return
	}
	switch {
	case st == 404:
		if gate != nil && !gate.CanAcquireLeadership(ctx) {
			e.log.Debug(fmt.Sprintf("Deferring initial lease creation on %q until local Xray is ready.", e.podName))
			return
		}
		e.tryCreate(ctx)
	case st == 200 && lease != nil:
		e.mu.Lock()
		e.resourceVersion = lease.Metadata.ResourceVersion
		e.mu.Unlock()
		holder := ""
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}
		dur := time.Duration(lease.Spec.LeaseDurationSeconds) * time.Second
		if dur == 0 {
			dur = e.LeaseDuration
		}
		expired := true
		if t, err := parseK8sTime(lease.Spec.RenewTime); err == nil && lease.Spec.RenewTime != "" {
			expired = e.now().After(t.Add(dur))
		}
		switch {
		case holder == e.podName && !e.IsLeader() && gate != nil && !gate.CanAcquireLeadership(ctx):
			// Our previous container held the lease and died (SIGKILL/OOM);
			// this fresh process has no xray yet while a peer is warm. Release
			// so the warm standby takes over now instead of a cold leader.
			e.log.Warn(fmt.Sprintf("Lease %q is still held by this pod from a previous container, but local Xray is not ready while a peer is warm. Releasing it...", e.leaseName))
			e.mu.Lock()
			e.suppressUntil = e.now().Add(15 * time.Second)
			e.mu.Unlock()
			e.releaseLease(ctx)
			return
		case holder == e.podName:
			if gate != nil {
				if healthy, peer := gate.CheckLeaderXrayHealth(ctx); !healthy && peer {
					e.log.Warn(fmt.Sprintf("⚠️ Local Xray process on leader %q is down (OOM/crash) while standby peer is healthy! Stepping down immediately...", e.podName))
					e.mu.Lock()
					e.suppressUntil = e.now().Add(15 * time.Second)
					e.mu.Unlock()
					e.releaseLease(ctx)
					e.setLeader(ctx, false)
					gate.NotifyStandbyToPromote(ctx)
					return
				}
			}
			e.renew(ctx, lease)
			if !e.IsLeader() {
				e.setLeader(ctx, true)
			}
		case holder == "" || expired:
			if gate != nil && !gate.CanAcquireLeadership(ctx) {
				e.log.Debug(fmt.Sprintf("Lease %q is vacant/expired, but %q Xray is not ready yet. Waiting for Xray startup before acquiring...", e.leaseName, e.podName))
				return
			}
			e.log.Info(fmt.Sprintf("Lease %q expired (previous holder: %s), attempting acquisition...", e.leaseName, holder))
			e.acquire(ctx, lease, holder)
		default:
			if e.IsLeader() {
				e.log.Warn(fmt.Sprintf("Lost lease %q to %s. Demoting to follower.", e.leaseName, holder))
				e.setLeader(ctx, false)
			}
		}
	}
}

func (e *Elector) tryCreate(ctx context.Context) {
	now := k8sMicroTime(e.now())
	id := e.podName
	l := &Lease{APIVersion: "coordination.k8s.io/v1", Kind: "Lease",
		Metadata: LeaseMetadata{Name: e.leaseName, Namespace: e.namespace},
		Spec: LeaseSpec{HolderIdentity: &id, LeaseDurationSeconds: int(e.LeaseDuration / time.Second),
			AcquireTime: now, RenewTime: now, LeaseTransitions: 1}}
	st, got, err := e.k8s.CreateLease(ctx, e.namespace, l)
	if err == nil && st == 201 && got != nil {
		e.setLeader(ctx, true)
	}
}

func (e *Elector) renew(ctx context.Context, l *Lease) {
	id := e.podName
	l.Spec.HolderIdentity = &id
	l.Spec.RenewTime = k8sMicroTime(e.now())
	l.Spec.LeaseDurationSeconds = int(e.LeaseDuration / time.Second)
	_, _, _ = e.k8s.UpdateLease(ctx, e.namespace, e.leaseName, l)
}

func (e *Elector) acquire(ctx context.Context, l *Lease, previous string) {
	id := e.podName
	now := k8sMicroTime(e.now())
	l.Spec.HolderIdentity = &id
	l.Spec.AcquireTime, l.Spec.RenewTime = now, now
	l.Spec.LeaseDurationSeconds = int(e.LeaseDuration / time.Second)
	l.Spec.LeaseTransitions++
	st, got, err := e.k8s.UpdateLease(ctx, e.namespace, e.leaseName, l)
	if err != nil || st != 200 || got == nil {
		return
	}
	e.setLeader(ctx, true)
	// A holder that died without releasing (OOM/SIGKILL) keeps role=leader;
	// strip it so the Service stops routing to it.
	if previous != "" && previous != e.podName {
		go func() {
			if st, _, err := e.k8s.PatchPodLabels(context.Background(), e.namespace, previous, map[string]string{"role": "standby"}); err == nil && st == 200 {
				e.log.Info(fmt.Sprintf("🧹 Cleared stale role=\"leader\" label on previous holder pod %q -> role=\"standby\"", previous))
			}
		}()
	}
}

func (e *Elector) releaseLease(ctx context.Context) {
	st, l, err := e.k8s.GetLease(ctx, e.namespace, e.leaseName)
	if err != nil {
		e.log.Warn("Failed to release lease: " + err.Error())
		return
	}
	if st == 200 && l != nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == e.podName {
		l.Spec.HolderIdentity = nil
		_, _, _ = e.k8s.UpdateLease(ctx, e.namespace, e.leaseName, l)
	}
}

func (e *Elector) setLeader(ctx context.Context, leader bool) {
	e.mu.Lock()
	prev := e.leader
	e.leader = leader
	draining := e.draining
	if !prev && leader {
		e.everLeader = true
		e.draining = false
		if e.drainTimer != nil {
			e.drainTimer.Stop()
			e.drainTimer = nil
		}
	}
	onP, onD := e.OnPromoted, e.OnDemoted
	e.mu.Unlock()
	switch {
	case !prev && leader:
		e.log.Info(fmt.Sprintf("🏆 Pod %q promoted to LEADER.", e.podName))
		e.markRole("leader") // before the cycle's own label check, like the TS sync assignment
		go e.UpdatePodRoleLabel(context.Background(), "leader")
		if onP != nil {
			go onP()
		}
	case prev && !leader:
		e.log.Warn(fmt.Sprintf("Pod %q demoted to FOLLOWER / STANDBY.", e.podName))
		if !draining {
			e.markRole("standby")
			go e.UpdatePodRoleLabel(context.Background(), "standby")
		}
		if onD != nil {
			go onD()
		}
	}
}

func (e *Elector) markRole(role string) {
	e.mu.Lock()
	e.appliedRole = role
	e.mu.Unlock()
}

func (e *Elector) UpdatePodRoleLabel(ctx context.Context, role string) {
	e.mu.Lock()
	e.appliedRole = role
	e.mu.Unlock()
	st, body, err := e.k8s.PatchPodLabels(ctx, e.namespace, e.podName, map[string]string{"role": role})
	if err == nil && st == 200 {
		e.log.Info(fmt.Sprintf("🏷️ Successfully labeled pod %q with role=%q", e.podName, role))
		return
	}
	e.mu.Lock()
	e.appliedRole = ""
	e.mu.Unlock()
	if err != nil {
		e.log.Warn(fmt.Sprintf("Error patching pod label role=%q: %v", role, err))
	} else {
		e.log.Warn(fmt.Sprintf("Failed to patch pod label role=%q (HTTP %d): %.200s", role, st, body))
	}
}

// YieldResult is yieldLeadership's answer.
type YieldResult struct {
	Success   bool   `json:"success"`
	Reason    string `json:"reason,omitempty"`
	Retryable *bool  `json:"retryable,omitempty"`
}

func boolp(b bool) *bool { return &b }

// YieldLeadership hands the lease to a newer pod (rolling update): release,
// then keep serving for DrainDuration so both pods overlap in the Service.
func (e *Elector) YieldLeadership(ctx context.Context, candidate string, candidateBoot time.Time, candidateRestarts int, candidateReady bool) YieldResult {
	e.mu.Lock()
	if !e.leader {
		e.mu.Unlock()
		return YieldResult{Success: true, Reason: "already_follower"}
	}
	switch {
	case !candidateReady:
		e.mu.Unlock()
		e.log.Warn(fmt.Sprintf("[LEADER-ELECTION] Rejecting yield request from %q: candidate Xray is not ready yet.", candidate))
		return YieldResult{Reason: "candidate_xray_not_ready", Retryable: boolp(true)}
	case candidateRestarts > 0:
		e.mu.Unlock()
		e.log.Warn(fmt.Sprintf("[LEADER-ELECTION] Rejecting yield request from %q: candidate has restartCount=%d (container restarted after OOM/crash, not a new deployment pod).", candidate, candidateRestarts))
		return YieldResult{Reason: "candidate_restarted", Retryable: boolp(false)}
	case !candidateBoot.After(e.podCreation):
		e.mu.Unlock()
		e.log.Warn(fmt.Sprintf("[LEADER-ELECTION] Rejecting yield request from %q (candidateBootTime %d <= local %d)", candidate, candidateBoot.UnixMilli(), e.podCreation.UnixMilli()))
		return YieldResult{Reason: "candidate_not_newer", Retryable: boolp(false)}
	case e.yielded[candidate]:
		e.mu.Unlock()
		e.log.Warn(fmt.Sprintf("[LEADER-ELECTION] Rejecting yield request from %q: already yielded to this pod once (likely recovering after local Xray crash).", candidate))
		return YieldResult{Reason: "already_yielded_to_candidate", Retryable: boolp(false)}
	}
	e.yielded[candidate] = true
	e.suppressUntil = e.now().Add(20 * time.Second)
	e.mu.Unlock()

	e.log.Info(fmt.Sprintf("[LEADER-ELECTION] 🤝 Gracefully yielding leadership to newer pod %q. Releasing lease and entering %s draining overlap...", candidate, e.DrainDuration))
	e.releaseLease(ctx)
	e.mu.Lock()
	e.draining = true
	e.mu.Unlock()
	e.setLeader(ctx, false)
	e.mu.Lock()
	if e.drainTimer != nil {
		e.drainTimer.Stop()
	}
	e.drainTimer = time.AfterFunc(e.DrainDuration, func() {
		e.log.Info("[LEADER-ELECTION] Draining period completed. Stepping down to standby.")
		e.mu.Lock()
		e.draining = false
		e.mu.Unlock()
		e.UpdatePodRoleLabel(context.Background(), "standby")
	})
	e.mu.Unlock()
	return YieldResult{Success: true}
}

// TriggerElectionNow runs a cycle at once, lifting any acquisition suppression.
func (e *Elector) TriggerElectionNow(ctx context.Context) {
	e.mu.Lock()
	if e.shuttingDown {
		e.mu.Unlock()
		return
	}
	e.suppressUntil = time.Time{}
	e.mu.Unlock()
	e.loop(ctx)
}
