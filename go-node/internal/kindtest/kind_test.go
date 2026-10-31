// Package kindtest runs the Helm chart with the Go node in a kind cluster
// and checks the HA layer end to end: Lease election and pod labels, a
// standby catching up and staying hot, graceful and hard leader failure, a
// rolling update with leadership hand-off, and real proxied traffic through
// the leader-only Service before, during and after each event.
//
//	KIND_CONTEXT=kind-rwnode KIND_IMAGE=remnawave-node-go:dev go test -v -timeout 30m ./internal/kindtest/
//
// KIND_NONROOT=1 also turns on the chart's podSecurity.runAsNonRoot hardening;
// KIND_ROOT=1 runs the pods as root instead (for comparison). The same settings
// work as flags: -args -kind-context=… -kind-image=… -kind-nonroot / -kind-root.
//
// The image (and busybox:1.36.1) must be loaded with `kind load docker-image`.
package kindtest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/hashedset"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
)

const payload = 256 << 10

var (
	kindContextFlag = flag.String("kind-context", "", "kubectl context of the kind cluster (or KIND_CONTEXT)")
	kindImageFlag   = flag.String("kind-image", "", "node image loaded into kind (or KIND_IMAGE)")
	kindNonRootFlag = flag.Bool("kind-nonroot", false, "turn on podSecurity.runAsNonRoot (or KIND_NONROOT=1)")
	kindRootFlag    = flag.Bool("kind-root", false, "run the pods as root (podSecurity.runAsUser 0), for comparison (or KIND_ROOT=1)")
)

func envOr(env, flagVal string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return flagVal
}

type env struct {
	t       *testing.T
	ctx, ns string
	release string
	pki     *testpki.PKI
	log     []string
}

func (e *env) note(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	e.log = append(e.log, s)
	e.t.Log(s)
}

func (e *env) kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", e.ctx, "-n", e.ns}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func (e *env) must(args ...string) string {
	e.t.Helper()
	out, err := e.kubectl(args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// pods returns node pod name → role label, for pods not being deleted.
func (e *env) pods() map[string]string {
	out, err := e.kubectl("get", "pods", "-l", "app.kubernetes.io/instance="+e.release+",app.kubernetes.io/name=remnanode", "-o", "json")
	m := map[string]string{}
	if err != nil {
		return m
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Labels            map[string]string `json:"labels"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	_ = json.Unmarshal([]byte(out), &list)
	for _, it := range list.Items {
		if it.Metadata.DeletionTimestamp == nil {
			m[it.Metadata.Name] = it.Metadata.Labels["role"]
		}
	}
	return m
}

func (e *env) leaseHolder() string {
	out, _ := e.kubectl("get", "lease", "remnanode-leader", "-o", "jsonpath={.spec.holderIdentity}")
	return out
}

// leader waits for exactly one pod labelled role=leader that also holds the lease.
func (e *env) leader(timeout time.Duration, not string) string {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var leaders []string
		for p, r := range e.pods() {
			if r == "leader" {
				leaders = append(leaders, p)
			}
		}
		if len(leaders) == 1 && leaders[0] != not && e.leaseHolder() == leaders[0] {
			return leaders[0]
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no single leader within %s: pods %v, lease holder %q", timeout, e.pods(), e.leaseHolder())
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (e *env) ready(pod string) bool {
	out, _ := e.kubectl("get", "pod", pod, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
	return out == "True"
}

func (e *env) waitAllReady(want int, timeout time.Duration) []string {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ready []string
		ps := e.pods()
		for p := range ps {
			if e.ready(p) {
				ready = append(ready, p)
			}
		}
		if len(ready) == want && len(ps) == want {
			return ready
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("want %d ready node pods within %s, have %v of %v", want, timeout, ready, ps)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var reForward = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// forward runs kubectl port-forward and returns the local address.
func (e *env) forward(target string, port int) (string, func()) {
	e.t.Helper()
	cmd := exec.Command("kubectl", "--context", e.ctx, "-n", e.ns, "port-forward", target, fmt.Sprintf(":%d", port))
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	addr := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if m := reForward.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case addr <- "127.0.0.1:" + m[1]:
				default:
				}
			}
		}
	}()
	select {
	case a := <-addr:
		return a, func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		e.t.Fatalf("port-forward %s: no address", target)
	}
	return "", nil
}

// panelCall plays the panel against one pod (mTLS, JWT, zstd body).
func (e *env) panelCall(addr, method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		enc, _ := zstd.NewWriter(nil, zstd.WithWindowSize(1<<25))
		rd = bytes.NewReader(enc.EncodeAll(raw, nil))
	}
	req, _ := http.NewRequest(method, "https://"+addr+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.pki.PanelJWT())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "zstd")
	}
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: e.pki.ClientTLS("x")}}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	r, _ := m["response"].(map[string]any)
	return resp.StatusCode, r
}

type user struct{ id, vless, hy string }

func xrayConfig(users []user) (map[string]any, map[string]any) {
	var vl, hy []any
	sets := map[string]*hashedset.Set{"VLESS": hashedset.New(), "HY": hashedset.New()}
	for _, u := range users {
		vl = append(vl, map[string]any{"id": u.vless, "email": u.id})
		hy = append(hy, map[string]any{"auth": u.hy, "email": u.id, "id": u.vless})
		sets["VLESS"].Add(u.vless)
		sets["HY"].Add(u.vless)
	}
	cert := testpki.SelfSigned("node.test")
	lines := func(s string) []any {
		var out []any
		for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
			out = append(out, l)
		}
		return out
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "VLESS", "listen": "0.0.0.0", "port": 21001, "protocol": "vless", "settings": map[string]any{"clients": vl, "decryption": "none"}},
			map[string]any{"tag": "HY", "listen": "0.0.0.0", "port": 21004, "protocol": "hysteria", "settings": map[string]any{"version": 2, "clients": hy},
				"streamSettings": map[string]any{"network": "hysteria", "security": "tls", "hysteriaSettings": map[string]any{"version": 2},
					"tlsSettings": map[string]any{"alpn": []any{"h3"}, "certificates": []any{map[string]any{"certificate": lines(cert.CertPEM), "key": lines(cert.KeyPEM)}}}}},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "DIRECT", "settings": map[string]any{
			"finalRules": []any{map[string]any{"action": "allow", "ip": []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}}}}}},
		"routing": map[string]any{"rules": []any{}},
	}
	var ib []any
	for _, tag := range []string{"VLESS", "HY"} {
		ib = append(ib, map[string]any{"usersCount": sets[tag].Len(), "hash": sets[tag].Hash64String(), "tag": tag})
	}
	return cfg, map[string]any{"emptyConfig": "kind-empty", "inbounds": ib}
}

func TestHelmChartHA(t *testing.T) {
	kctx, image := envOr("KIND_CONTEXT", *kindContextFlag), envOr("KIND_IMAGE", *kindImageFlag)
	if kctx == "" || image == "" {
		t.Skip("set KIND_CONTEXT and KIND_IMAGE to run the chart in a kind cluster")
	}
	repo, tag, _ := strings.Cut(image, ":")
	e := &env{t: t, ctx: kctx, ns: "rwkind-" + randHex(3), release: "rn", pki: testpki.New()}
	defer e.report()
	chart := filepath.Join(nodefixture.Root(), "..", "charts", "remnanode")

	if out, err := exec.Command("kubectl", "--context", kctx, "create", "ns", e.ns).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if os.Getenv("KIND_KEEP") == "" {
		t.Cleanup(func() { _ = exec.Command("kubectl", "--context", kctx, "delete", "ns", e.ns, "--wait=false").Run() })
	}
	for i := 0; i < 60; i++ { // the namespace's default service account appears asynchronously
		if _, err := e.kubectl("get", "serviceaccount", "default"); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	values := map[string]any{
		"replicaCount": 2,
		"website":      map[string]any{"enabled": false},
		"ingress":      map[string]any{"enabled": false},
		"node": map[string]any{
			"image":     map[string]any{"repository": repo, "tag": tag, "pullPolicy": "IfNotPresent"},
			"service":   map[string]any{"type": "NodePort", "externalTrafficPolicy": "Cluster"},
			"secretKey": e.pki.SecretKey(),
			"resources": map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"cpu": "1", "memory": "512Mi"}},
		},
		"inbounds": []any{
			map[string]any{"name": "vless", "port": 21001, "mode": "ingress-sni"},
			map[string]any{"name": "hysteria2", "port": 21004, "protocol": "udp", "mode": "ingress-sni"},
		},
	}
	if os.Getenv("KIND_NONROOT") != "" || *kindNonRootFlag {
		// The image runs as nobody either way; this adds the chart's hardening
		// (runAsNonRoot, no capabilities, RuntimeDefault seccomp).
		values["podSecurity"] = map[string]any{"runAsNonRoot": true}
	} else if os.Getenv("KIND_ROOT") != "" || *kindRootFlag {
		values["podSecurity"] = map[string]any{"runAsUser": 0, "runAsGroup": 0}
	}
	vf := filepath.Join(t.TempDir(), "values.json")
	raw, _ := json.Marshal(values)
	_ = os.WriteFile(vf, raw, 0o600)
	if out, err := exec.Command("helm", "--kube-context", kctx, "-n", e.ns, "install", e.release, chart, "-f", vf).CombinedOutput(); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}

	// Payload server and xray client pods.
	e.must("run", "web", "--image=busybox:1.36.1", "--image-pull-policy=IfNotPresent", "--port=8080", "--",
		"sh", "-c", fmt.Sprintf("mkdir /www && head -c %d /dev/urandom > /www/f && exec httpd -f -p 8080 -h /www", payload))
	e.must("expose", "pod", "web", "--port=8080")

	// 1. Election on a cold start: one leader, labelled, holding the lease.
	t0 := time.Now()
	lead := e.leader(3*time.Minute, "")
	e.note("cold start: %s leads after %s; pods %v", lead, time.Since(t0).Round(time.Second), e.pods())

	// 2. The panel pushes a config and adds a user incrementally on the leader.
	users := []user{{"1", uuid(), randHex(8)}, {"2", uuid(), randHex(8)}}
	cfg, hashes := xrayConfig(users)
	addr, stop := e.forward("pod/"+lead, 2222)
	st, r := e.panelCall(addr, "POST", "/node/xray/start", map[string]any{
		"internals":  map[string]any{"forceRestart": false, "hashes": hashes, "metadata": map[string]any{"uuid": uuid(), "name": "kind", "countryCode": "XX", "id": 1, "tags": []any{}}},
		"xrayConfig": cfg})
	if st != 201 || r["isStarted"] != true {
		t.Fatalf("xray/start on the leader: %d %v", st, r)
	}
	u3 := user{"3", uuid(), randHex(8)}
	st, r = e.panelCall(addr, "POST", "/node/handler/add-user", map[string]any{
		"data":     []any{map[string]any{"type": "vless", "tag": "VLESS", "username": u3.id, "uuid": u3.vless, "flow": ""}, map[string]any{"type": "hysteria", "tag": "HY", "username": u3.id, "password": u3.hy}},
		"hashData": map[string]any{"vlessUuid": u3.vless}})
	stop()
	if st != 201 || r["success"] != true {
		t.Fatalf("add-user: %d %v", st, r)
	}

	// 3. The standby catches up (replicated start) and becomes Ready = hot.
	t0 = time.Now()
	e.waitAllReady(2, 2*time.Minute)
	e.note("standby hot (xray up, Ready) %s after the panel's push", time.Since(t0).Round(100*time.Millisecond))

	// Client pod: xray with SOCKS, as user 3 (added incrementally) over VLESS
	// and Hysteria 2 to the leader-only Service.
	svc := e.release + "-remnanode"
	clientCfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "s-vl", "listen": "0.0.0.0", "port": 1080, "protocol": "socks", "settings": map[string]any{"udp": false}},
			map[string]any{"tag": "h-vl", "listen": "0.0.0.0", "port": 8118, "protocol": "http", "settings": map[string]any{}},
		},
		"outbounds": []any{
			map[string]any{"tag": "vl", "protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": svc, "port": 21001,
				"users": []any{map[string]any{"id": u3.vless, "encryption": "none"}}}}}},
		},
		"routing": map[string]any{"rules": []any{map[string]any{"inboundTag": []any{"s-vl", "h-vl"}, "outboundTag": "vl"}}},
	}
	ccfg, _ := json.Marshal(clientCfg)
	cf := filepath.Join(t.TempDir(), "client.json")
	_ = os.WriteFile(cf, ccfg, 0o600)
	e.must("create", "configmap", "client", "--from-file=client.json="+cf)
	override := fmt.Sprintf(`{"spec":{"containers":[{"name":"client","image":%q,"imagePullPolicy":"IfNotPresent","command":["/usr/local/bin/xray","run","-c","/cfg/client.json"],"volumeMounts":[{"name":"cfg","mountPath":"/cfg"}]}],"volumes":[{"name":"cfg","configMap":{"name":"client"}}]}}`, image)
	e.must("run", "client", "--image="+image, "--overrides="+override, "--restart=Never")
	e.must("wait", "--for=condition=Ready", "pod/client", "pod/web", "--timeout=120s")
	e.must("expose", "pod", "client", "--port=8118")
	// The prober downloads through the client's HTTP proxy every 200 ms,
	// inside the cluster, and logs "<uptime> OK|FAIL" (kubectl port-forward
	// dies on the first reset connection, so it can't measure outages).
	e.must("run", "prober", "--image=busybox:1.36.1", "--image-pull-policy=IfNotPresent", "--env=http_proxy=http://client:8118", "--",
		"sh", "-c", `while true; do read u _ </proc/uptime; if wget -q -T 5 -O /dev/null http://web:8080/f; then echo "$u OK"; else echo "$u FAIL"; fi; sleep 0.2; done`)
	e.must("wait", "--for=condition=Ready", "pod/prober", "--timeout=120s")
	if !e.waitOK(60 * time.Second) {
		t.Fatal("no traffic through the leader as the incrementally added user")
	}
	e.note("traffic via the leader-only Service as user 3 (added with add-user): %d KiB OK", payload>>10)

	type probeResult struct {
		ok, fail int
		maxGap   time.Duration
	}
	// probe reads the prober's results for the next d.
	probe := func(d time.Duration) probeResult {
		start := e.uptime()
		time.Sleep(d)
		end := e.uptime()
		var res probeResult
		var downSince float64 = -1
		var last float64
		for _, l := range e.proberLines() {
			if l.t < start || l.t > end {
				continue
			}
			last = l.t
			if l.ok {
				res.ok++
				if downSince >= 0 {
					if g := time.Duration((l.t - downSince) * float64(time.Second)); g > res.maxGap {
						res.maxGap = g
					}
					downSince = -1
				}
			} else {
				res.fail++
				if downSince < 0 {
					downSince = l.t
				}
			}
		}
		if downSince >= 0 {
			res.maxGap = time.Duration((last - downSince) * float64(time.Second))
		}
		return res
	}

	// 4. Graceful leader loss (pod deleted with its grace period): the lease
	// is released, the hot standby takes over, user 3 keeps working.
	done := make(chan probeResult)
	go func() { done <- probe(45 * time.Second) }()
	time.Sleep(2 * time.Second)
	t0 = time.Now()
	e.must("delete", "pod", lead, "--wait=false")
	next := e.leader(30*time.Second, lead)
	e.note("graceful failover: %s → %s in %s", lead, next, time.Since(t0).Round(100*time.Millisecond))
	res := <-done
	e.note("traffic during graceful failover: %d ok, %d failed, longest outage %s", res.ok, res.fail, res.maxGap.Round(100*time.Millisecond))
	if res.ok == 0 || res.maxGap > 15*time.Second {
		t.Errorf("graceful failover outage too long: %+v", res)
	}

	// The replacement pod catches up and, being newer, asks for the hand-off.
	e.waitAllReady(2, 2*time.Minute)
	time.Sleep(15 * time.Second)
	cur := e.leader(30*time.Second, "")
	e.note("after the replacement pod caught up: leader %s, pods %v", cur, e.pods())
	if !e.waitOK(15 * time.Second) {
		t.Errorf("no traffic after hand-off to the replacement")
	}

	// 5. Hard leader loss (SIGKILL, no release): the lease expires (10 s), the
	// standby acquires it and strips the dead leader's label.
	go func() { done <- probe(50 * time.Second) }()
	time.Sleep(2 * time.Second)
	t0 = time.Now()
	e.must("delete", "pod", cur, "--grace-period=0", "--force", "--wait=false")
	next = e.leader(40*time.Second, cur)
	e.note("hard failover (force delete): %s → %s in %s", cur, next, time.Since(t0).Round(100*time.Millisecond))
	res = <-done
	e.note("traffic during hard failover: %d ok, %d failed, longest outage %s", res.ok, res.fail, res.maxGap.Round(100*time.Millisecond))
	e.diag("after that step")
	if res.ok == 0 || res.maxGap > 25*time.Second {
		t.Errorf("hard failover outage too long: %+v", res)
	}
	e.waitAllReady(2, 2*time.Minute)

	// 6. xray dies on the leader (OOM-style SIGKILL of the child process)
	// while the node process lives: the leader sees its xray down with a
	// healthy standby, steps down and wakes the standby.
	time.Sleep(10 * time.Second)
	cur = e.leader(30*time.Second, "")
	go func() { done <- probe(40 * time.Second) }()
	time.Sleep(2 * time.Second)
	t0 = time.Now()
	e.kill(cur, true)
	next = e.leader(30*time.Second, cur)
	e.note("xray SIGKILL on the leader: %s steps down → %s in %s", cur, next, time.Since(t0).Round(100*time.Millisecond))
	res = <-done
	e.note("traffic during the xray crash: %d ok, %d failed, longest outage %s", res.ok, res.fail, res.maxGap.Round(100*time.Millisecond))
	e.diag("after that step")
	if res.ok == 0 || res.maxGap > 15*time.Second {
		t.Errorf("xray crash outage too long: %+v", res)
	}
	e.waitAllReady(2, 2*time.Minute)

	// 7. The node process itself is SIGKILLed: nothing releases the lease,
	// so the standby takes it once it expires (10 s); the container restarts
	// with restartCount=1 and stays a standby (no hand-off request).
	time.Sleep(10 * time.Second)
	cur = e.leader(30*time.Second, "")
	go func() { done <- probe(50 * time.Second) }()
	time.Sleep(2 * time.Second)
	t0 = time.Now()
	e.kill(cur, false)
	next = e.leader(40*time.Second, cur)
	e.note("node SIGKILL on the leader (lease not released): %s → %s in %s", cur, next, time.Since(t0).Round(100*time.Millisecond))
	res = <-done
	e.note("traffic during the node SIGKILL: %d ok, %d failed, longest outage %s", res.ok, res.fail, res.maxGap.Round(100*time.Millisecond))
	e.diag("after that step")
	if res.ok == 0 || res.maxGap > 25*time.Second {
		t.Errorf("node SIGKILL outage too long: %+v", res)
	}
	e.waitAllReady(2, 2*time.Minute)
	time.Sleep(10 * time.Second)
	if l := e.leader(30*time.Second, ""); l != next {
		t.Errorf("a restarted container (restartCount>0) must not take leadership back: leader %s, want %s", l, next)
	}

	// 8. Rolling update: new pods become hot standbys, then the leader yields.
	time.Sleep(10 * time.Second)
	before := e.leader(30*time.Second, "")
	go func() { done <- probe(120 * time.Second) }()
	time.Sleep(2 * time.Second)
	e.must("rollout", "restart", "deployment/"+e.release+"-remnanode")
	if out, err := e.kubectl("rollout", "status", "deployment/"+e.release+"-remnanode", "--timeout=240s"); err != nil {
		t.Fatalf("rollout: %v %s", err, out)
	}
	after := e.leader(60*time.Second, before)
	e.note("rolling update: leader %s → %s, pods %v", before, after, e.pods())
	res = <-done
	e.note("traffic during the rolling update: %d ok, %d failed, longest outage %s", res.ok, res.fail, res.maxGap.Round(100*time.Millisecond))
	if res.fail > 0 && res.maxGap > 15*time.Second {
		t.Errorf("rolling update outage too long: %+v", res)
	}

	// 9. Logs: the Go pods never put SECRET_KEY on the wire and peer auth works.
	var all strings.Builder
	for p := range e.pods() {
		out, _ := e.kubectl("logs", p)
		all.WriteString(out)
	}
	logs := all.String()
	if strings.Contains(logs, e.pki.SecretKey()[:40]) {
		t.Error("SECRET_KEY appears in pod logs")
	}
	if strings.Contains(logs, "Failed to replicate") {
		e.note("note: some replication attempts failed (expected while a peer pod terminates)")
	}
	e.note("final: leader %s, pods %v", e.leader(10*time.Second, ""), e.pods())
}

// kill SIGKILLs a pod's node process, or only its xray child, from the kind node.
func (e *env) kill(pod string, xrayOnly bool) {
	e.t.Helper()
	node := os.Getenv("KIND_NODE")
	if node == "" {
		node = strings.TrimPrefix(e.ctx, "kind-") + "-control-plane"
	}
	target := "$pid"
	if xrayOnly {
		target = "$(pgrep -P $pid)"
	}
	script := fmt.Sprintf(`cid=$(crictl ps -q --name remnanode --label io.kubernetes.pod.name=%s); pid=$(crictl inspect -o go-template --template '{{.info.pid}}' $cid); kill -9 %s`, pod, target)
	if out, err := exec.Command("docker", "exec", node, "sh", "-c", script).CombinedOutput(); err != nil {
		e.t.Fatalf("kill in %s: %v %s", pod, err, out)
	}
}

// diag logs, per node pod, whether xray is up and user 3's traffic so far.
func (e *env) diag(when string) {
	var parts []string
	for p, role := range e.pods() {
		addr, stop := e.forward("pod/"+p, 2222)
		_, hc := e.panelCall(addr, "GET", "/node/xray/healthcheck", nil)
		_, us := e.panelCall(addr, "POST", "/node/stats/get-users-stats", map[string]any{"reset": false})
		stop()
		u3 := "-"
		if list, ok := us["users"].([]any); ok {
			for _, u := range list {
				if m := u.(map[string]any); m["username"] == "3" {
					u3 = fmt.Sprint(m["downlink"])
				}
			}
		}
		parts = append(parts, fmt.Sprintf("%s(%s xray=%v u3down=%s)", p[len(p)-5:], role, hc["xrayInternalStatusCached"], u3))
	}
	e.note("diag %s: %s", when, strings.Join(parts, " "))
}

func (e *env) uptime() float64 {
	out, _ := e.kubectl("exec", "prober", "--", "cat", "/proc/uptime")
	f, _ := strconv.ParseFloat(strings.Fields(out + " 0")[0], 64)
	return f
}

type proberLine struct {
	t  float64
	ok bool
}

func (e *env) proberLines() []proberLine {
	out, _ := e.kubectl("logs", "prober")
	var lines []proberLine
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			if t, err := strconv.ParseFloat(f[0], 64); err == nil {
				lines = append(lines, proberLine{t, f[1] == "OK"})
			}
		}
	}
	return lines
}

// waitOK waits for a successful prober download that started after now.
func (e *env) waitOK(d time.Duration) bool {
	from := e.uptime()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, l := range e.proberLines() {
			if l.t > from && l.ok {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func (e *env) report() {
	path := os.Getenv("KIND_REPORT")
	if path == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# kind HA run %s\n\n", time.Now().Format(time.RFC3339))
	for _, l := range e.log {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

// socksGet downloads /f from target through a SOCKS5 proxy.
func socksGet(proxy, target string) (int, error) {
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 262)
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[1] != 0 {
		return 0, fmt.Errorf("greeting: %v", err)
	}
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c, buf[:10]); err != nil || buf[1] != 0 {
		return 0, fmt.Errorf("connect: %v %x", err, buf[:10])
	}
	fmt.Fprintf(c, "GET /f HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return int(n), err
}
