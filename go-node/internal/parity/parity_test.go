// Package parity runs the TS node and the Go node side by side in Docker,
// plays the panel against both with the same SECRET_KEY and the same
// requests, and diffs everything the panel can observe: status codes,
// headers, response bodies, the generated xray config, traffic counters
// from real proxied traffic, and the torrent-blocker flow triggered by a
// real BitTorrent handshake.
//
//	PARITY_TS_IMAGE=ghcr.io/therodev-corp/remnawave-node-cloud-native:3.4.5-HA \
//	PARITY_GO_IMAGE=remnawave-node-go:amd64 go test -v -run TestParity ./internal/parity/
//
// Optional: PARITY_PLATFORM (default linux/amd64), PARITY_REPORT (write a
// markdown report there).
package parity

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/hashedset"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
)

const payloadSize = 512 << 10

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

func dockerRun(args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerRun(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// node is one container under test, as the panel sees it.
type node struct {
	label, name string
	base        string
	client      *http.Client
	sock, token string // internal socket path and token
}

type reply struct {
	dropped bool
	status  int
	header  http.Header
	raw     []byte
	body    any
}

type harness struct {
	t        *testing.T
	pki      *testpki.PKI
	ts, gn   *node
	platform string
	tsImage  string
	diffs    []string
	log      []string
	raw      []string
}

func (h *harness) note(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	h.log = append(h.log, s)
	h.t.Log(s)
}

func (h *harness) diff(step, format string, a ...any) {
	s := step + ": " + fmt.Sprintf(format, a...)
	h.diffs = append(h.diffs, s)
	h.t.Error(s)
}

func (h *harness) call(n *node, method, path string, body any, zstdBody bool, jwt string) reply {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		if zstdBody {
			// The panel's settings: level 1, long-distance matching, windowLog 25.
			enc, _ := zstd.NewWriter(nil, zstd.WithWindowSize(1<<25), zstd.WithEncoderLevel(zstd.SpeedFastest))
			raw = enc.EncodeAll(raw, nil)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, n.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if zstdBody {
			req.Header.Set("Content-Encoding", "zstd")
		}
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return reply{dropped: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var dec io.Reader
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		dec, _ = gzip.NewReader(bytes.NewReader(raw))
	case "br":
		dec = brotli.NewReader(bytes.NewReader(raw))
	case "deflate":
		dec, _ = zlib.NewReader(bytes.NewReader(raw))
	}
	if dec != nil {
		if plain, err := io.ReadAll(dec); err == nil {
			raw = plain
		}
	}
	r := reply{status: resp.StatusCode, header: resp.Header, raw: raw}
	jd := json.NewDecoder(bytes.NewReader(raw))
	jd.UseNumber()
	if jd.Decode(&r.body) != nil {
		r.body = string(raw)
	}
	return r
}

// both sends the same request to both nodes and diffs what the panel sees.
func (h *harness) both(step, method, path string, body any, zstdBody bool) (reply, reply) {
	h.t.Helper()
	jwt := h.pki.PanelJWT()
	a := h.call(h.ts, method, path, body, zstdBody, jwt)
	b := h.call(h.gn, method, path, body, zstdBody, jwt)
	h.compare(step, a, b)
	return a, b
}

func (h *harness) compare(step string, a, b reply) {
	if a.dropped || b.dropped {
		if a.dropped != b.dropped {
			h.diff(step, "TS dropped=%v (status %d), Go dropped=%v (status %d)", a.dropped, a.status, b.dropped, b.status)
		} else {
			h.note("%s: both dropped the socket", step)
		}
		return
	}
	h.raw = append(h.raw, fmt.Sprintf("### %s\n\nTS %d: `%.1500s`\n\nGo %d: `%.1500s`\n", step, a.status, a.raw, b.status, b.raw))
	if a.status != b.status {
		h.diff(step, "status TS %d, Go %d\nTS: %.400s\nGo: %.400s", a.status, b.status, a.raw, b.raw)
		return
	}
	na, nb := normalize(a.body, ""), normalize(b.body, "")
	same := true
	if !reflect.DeepEqual(na, nb) {
		ja, _ := json.MarshalIndent(na, "", " ")
		jb, _ := json.MarshalIndent(nb, "", " ")
		h.diff(step, "body differs\nTS: %s\nGo: %s", ja, jb)
		same = false
	}
	ha, hb := headerView(a.header), headerView(b.header)
	if ha["content-encoding"] != hb["content-encoding"] && (len(a.raw) >= 1024) != (len(b.raw) >= 1024) {
		// Bodies on different sides of compression()'s 1 KiB threshold (host-specific values).
		h.note("%s: compression differs only because the bodies straddle 1 KiB (%d / %d bytes)", step, len(a.raw), len(b.raw))
		delete(ha, "content-encoding")
		delete(hb, "content-encoding")
	}
	if !reflect.DeepEqual(ha, hb) {
		h.diff(step, "headers differ\nTS: %v\nGo: %v", ha, hb)
		same = false
	}
	if same {
		h.note("%s: %d, identical", step, a.status)
	}
}

// headerView keeps what a client can rely on: every header name, and the
// value of every header except the per-response ones.
func headerView(hd http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range hd {
		k = strings.ToLower(k)
		switch k {
		case "date", "content-length", "keep-alive", "connection":
			continue
		case "etag":
			out[k] = "W/<etag>"
			if !strings.HasPrefix(v[0], `W/"`) {
				out[k] = "<strong etag>"
			}
		case "content-encoding":
			out[k] = v[0]
		default:
			out[k] = strings.Join(v, ", ")
		}
	}
	return out
}

// Keys whose values depend on time, host or traffic volume: only their JSON
// type is compared. Counters are checked separately with real traffic.
var volatileKeys = map[string]bool{
	"timestamp": true, "lastSeen": true, "processedAt": true, "willUnblockAt": true, "ts": true,
	"downlink": true, "uplink": true, "data": true,
}

func typeOf(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		return "<bool>"
	case json.Number:
		return "<number>"
	case string:
		return "<string>"
	case []any:
		return "<array>"
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = typeOf(e)
		}
		return out
	}
	return fmt.Sprintf("<%T>", v)
}

var (
	reSeconds   = regexp.MustCompile(`\d+ seconds`)
	reConfigSrc = regexp.MustCompile(`failed to load config files: \[[^\]]*\]`)
	reLogTime   = regexp.MustCompile(`\d{4}/\d\d/\d\d \d\d:\d\d:\d\d(\.\d+)?`)
)

var sortKeys = []string{"username", "userId", "inbound", "outbound", "ip", "tag"}

func normalize(v any, path string) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			p := path + "." + k
			switch {
			case volatileKeys[k] || strings.HasPrefix(p, ".response.system.") || strings.HasPrefix(p, ".response.xrayInfo."):
				out[k] = typeOf(e)
			case k == "error":
				if str, ok := e.(string); ok {
					str = reSeconds.ReplaceAllString(str, "<n> seconds")
					// xray names its config source: the TS node's internal socket URL
					// (with the internal token) vs the Go node's stdin. Documented.
					str = reConfigSrc.ReplaceAllString(str, "failed to load config files: [<config source>]")
					out[k] = reLogTime.ReplaceAllString(str, "<time>")
				} else {
					out[k] = e
				}
			case k == "source" || k == "inboundLocal":
				// source "172.18.0.5:41234": the client port differs per connection;
				// inboundLocal is the node's own address, which differs per container.
				if s, ok := e.(string); ok {
					if i := strings.LastIndex(s, ":"); i > 0 {
						if k == "source" {
							s = s[:i] + ":<port>"
						} else {
							s = "<node-ip>" + s[i:]
						}
					}
					out[k] = s
				} else {
					out[k] = e
				}
			default:
				out[k] = normalize(e, p)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e, path+"[]")
		}
		sort.SliceStable(out, func(i, j int) bool { return sortKey(out[i]) < sortKey(out[j]) })
		return out
	}
	return v
}

func sortKey(v any) string {
	if m, ok := v.(map[string]any); ok {
		for _, k := range sortKeys {
			if s, ok := m[k].(string); ok {
				return k + "=" + s
			}
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func resp(r reply) map[string]any {
	m, _ := r.body.(map[string]any)
	out, _ := m["response"].(map[string]any)
	return out
}

func num(v any) int64 {
	n, _ := v.(json.Number).Int64()
	return n
}

// ---- panel-side data -------------------------------------------------------

type user struct{ id, vless, trojan, ss, hy string }

func newUser(i int) user {
	return user{strconv.Itoa(i), uuid(), randHex(12), randHex(16), randHex(12)}
}

var tags = []string{"VLESS", "TROJAN", "SS", "HY"}

const (
	portVless  = 21001
	portTrojan = 21002
	portSS     = 21003
	portHY     = 21004
)

func pemLines(s string) []any {
	var out []any
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		out = append(out, l)
	}
	return out
}

// xrayConfig is what the panel pushes: client entries carry id = vlessUuid
// (xray-config.validator.ts), inbounds listen on all interfaces.
func xrayConfig(users []user, cert testpki.ServerCert) (map[string]any, map[string]any) {
	var vl, tr, ss, hy []any
	sets := map[string]*hashedset.Set{}
	for _, tag := range tags {
		sets[tag] = hashedset.New()
	}
	for _, u := range users {
		vl = append(vl, map[string]any{"id": u.vless, "email": u.id})
		tr = append(tr, map[string]any{"password": u.trojan, "email": u.id, "id": u.vless})
		ss = append(ss, map[string]any{"password": u.ss, "method": "aes-256-gcm", "email": u.id, "id": u.vless})
		hy = append(hy, map[string]any{"auth": u.hy, "email": u.id, "id": u.vless})
		for _, s := range sets {
			s.Add(u.vless)
		}
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "VLESS", "listen": "0.0.0.0", "port": portVless, "protocol": "vless",
				"settings": map[string]any{"clients": vl, "decryption": "none"},
				"sniffing": map[string]any{"enabled": true, "destOverride": []any{"http", "tls"}}},
			map[string]any{"tag": "TROJAN", "listen": "0.0.0.0", "port": portTrojan, "protocol": "trojan", "settings": map[string]any{"clients": tr}},
			map[string]any{"tag": "SS", "listen": "0.0.0.0", "port": portSS, "protocol": "shadowsocks", "settings": map[string]any{"clients": ss, "network": "tcp,udp"}},
			map[string]any{"tag": "HY", "listen": "0.0.0.0", "port": portHY, "protocol": "hysteria",
				"settings": map[string]any{"version": 2, "clients": hy},
				"streamSettings": map[string]any{"network": "hysteria", "security": "tls", "hysteriaSettings": map[string]any{"version": 2},
					"tlsSettings": map[string]any{"alpn": []any{"h3"}, "certificates": []any{map[string]any{
						"certificate": pemLines(cert.CertPEM), "key": pemLines(cert.KeyPEM)}}}}},
		},
		// xray ≥ 26 blackholes private targets for proxied users by default;
		// the payload server is on the Docker network.
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "DIRECT", "settings": map[string]any{
			"finalRules": []any{map[string]any{"action": "allow", "ip": []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}}}}}},
		"routing": map[string]any{"rules": []any{}},
	}
	var ib []any
	for _, tag := range tags {
		ib = append(ib, map[string]any{"usersCount": sets[tag].Len(), "hash": sets[tag].Hash64String(), "tag": tag})
	}
	return cfg, map[string]any{"emptyConfig": "parity-empty-config-hash", "inbounds": ib}
}

func startBody(cfg, hashes map[string]any) map[string]any {
	return map[string]any{
		"internals":  map[string]any{"forceRestart": false, "hashes": hashes, "metadata": map[string]any{"uuid": "7b0e0e6a-1d2b-4c3d-8e4f-5a6b7c8d9e0f", "name": "parity", "countryCode": "XX", "id": 1, "tags": []any{}}},
		"xrayConfig": cfg,
	}
}

// ---- the test ---------------------------------------------------------------

func TestParity(t *testing.T) {
	tsImage, goImage := os.Getenv("PARITY_TS_IMAGE"), os.Getenv("PARITY_GO_IMAGE")
	if tsImage == "" || goImage == "" {
		t.Skip("set PARITY_TS_IMAGE and PARITY_GO_IMAGE to run the container parity harness")
	}
	platform := os.Getenv("PARITY_PLATFORM")
	if platform == "" {
		platform = "linux/amd64"
	}
	h := &harness{t: t, pki: testpki.New(), platform: platform, tsImage: tsImage}
	defer h.writeReport()
	id := randHex(3)
	netName := "rwp-" + id
	var containers []string
	t.Cleanup(func() {
		if os.Getenv("PARITY_KEEP") != "" {
			t.Logf("PARITY_KEEP set: leaving containers %v and network %s", containers, netName)
			return
		}
		for _, c := range containers {
			_, _ = dockerRun("rm", "-f", c)
		}
		_, _ = dockerRun("network", "rm", netName)
	})
	docker(t, "network", "create", netName)

	web := "rwp-web-" + id
	containers = append(containers, web)
	docker(t, "run", "-d", "--name", web, "--network", netName, "--platform", platform, "--entrypoint", "node", tsImage, "-e",
		fmt.Sprintf(`const b=require('crypto').randomBytes(%d);require('http').createServer((q,r)=>r.end(b)).listen(8080)`, payloadSize))

	secret := h.pki.SecretKey()
	start := func(label, image string, extra ...string) *node {
		name := "rwp-" + label + "-" + id
		containers = append(containers, name)
		args := append([]string{"run", "-d", "--name", name, "--network", netName, "--platform", platform,
			"-e", "NODE_PORT=2222", "-e", "SECRET_KEY=" + secret, "-p", "127.0.0.1::2222"}, extra...)
		docker(t, append(args, image)...)
		hostPort := docker(t, "port", name, "2222/tcp")
		hostPort = strings.Split(hostPort, "\n")[0]
		return &node{label: label, name: name, base: "https://" + hostPort}
	}
	goToken := randHex(32)
	h.ts = start("ts", tsImage)
	h.gn = start("go", goImage, "-e", "INTERNAL_SOCKET_PATH=rwint-parity"+id, "-e", "INTERNAL_REST_TOKEN="+goToken, "-e", "XTLS_API_SOCKET_PATH=xtls-api-parity"+id)
	h.gn.sock, h.gn.token = "rwint-parity"+id, goToken

	sni := deriveSNI(t, secret)
	for _, n := range []*node{h.ts, h.gn} {
		n.client = &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{TLSClientConfig: h.pki.ClientTLS(sni), DisableCompression: true}}
		h.waitReady(n)
	}
	h.ts.sock = docker(t, "exec", h.ts.name, "cat", "/run/s6/container_environment/INTERNAL_SOCKET_PATH")
	h.ts.token = docker(t, "exec", h.ts.name, "cat", "/run/s6/container_environment/INTERNAL_REST_TOKEN")

	// Before any config: health, the xray-down stats error, plugin sync(null).
	h.both("healthcheck before start", "GET", "/node/xray/healthcheck", nil, false)
	h.both("get-system-stats with xray down", "GET", "/node/stats/get-system-stats", nil, false)
	h.both("plugin sync null", "POST", "/node/plugin/sync", map[string]any{"plugin": nil}, true)

	// Auth and routing edge cases.
	h.compare("bad JWT", h.call(h.ts, "GET", "/node/xray/healthcheck", nil, false, "not-a-jwt"), h.call(h.gn, "GET", "/node/xray/healthcheck", nil, false, "not-a-jwt"))
	h.both("unknown route", "GET", "/node/does-not-exist", nil, false)
	h.both("invalid body", "POST", "/node/stats/get-users-stats", map[string]any{"reset": "yes"}, false)
	h.both("trailing slash + case", "GET", "/Node/Xray/Healthcheck/", nil, false)
	h.both("validation: extra key", "POST", "/node/stats/get-users-stats", map[string]any{"reset": false, "extra": 1}, false)
	h.both("validation: missing field", "POST", "/node/stats/get-inbound-stats", map[string]any{"reset": false}, false)
	h.both("validation: empty body", "POST", "/node/stats/get-users-stats", map[string]any{}, false)
	h.both("validation: array body", "POST", "/node/stats/get-users-stats", []any{1}, false)
	h.both("validation: bad enum + uuid", "POST", "/node/handler/add-user", map[string]any{
		"data":     []any{map[string]any{"type": "shadowsocks", "tag": "SS", "username": "9", "password": "p", "cipherType": 99, "ivCheck": false}},
		"hashData": map[string]any{"vlessUuid": "not-a-uuid"},
	}, false)
	h.both("validation: unknown union member", "POST", "/node/handler/add-user", map[string]any{
		"data":     []any{map[string]any{"type": "wireguard", "tag": "X", "username": "9"}},
		"hashData": map[string]any{"vlessUuid": uuid()},
	}, false)
	h.both("validation: nested wrong type", "POST", "/node/handler/remove-users", map[string]any{"users": []any{map[string]any{"userId": 5, "hashUuid": uuid()}}}, true)
	h.both("validation: min items", "POST", "/node/handler/drop-ips", map[string]any{"ips": []any{}}, false)
	h.both("validation: bad ip", "POST", "/node/plugin/nftables/unblock-ips", map[string]any{"ips": []any{"999.1.1.1"}}, false)

	// Both nodes report system.interface = null until their 1 s network
	// sampler has a rate; give the TS node's sampler time to tick.
	time.Sleep(3 * time.Second)
	// A config xray refuses: both nodes must report the same failure text.
	bad := map[string]any{"inbounds": []any{map[string]any{"tag": "BAD", "listen": "0.0.0.0", "port": 21009, "protocol": "definitely-not-a-protocol"}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "DIRECT"}}}
	h.both("xray/start with a config xray rejects", "POST", "/node/xray/start", startBody(bad, map[string]any{"emptyConfig": "bad", "inbounds": []any{}}), true)

	cert := testpki.SelfSigned("node.test")
	users := []user{newUser(1), newUser(2), newUser(3)}
	cfg, hashes := xrayConfig(users, cert)
	a, b := h.both("xray/start", "POST", "/node/xray/start", startBody(cfg, hashes), true)
	if resp(a)["isStarted"] != true || resp(b)["isStarted"] != true {
		h.dumpLogs()
		t.Fatalf("xray did not start: TS %v / Go %v", resp(a), resp(b))
	}
	h.diffConfigs("generated xray config after start")
	h.both("xray/start again (same hashes)", "POST", "/node/xray/start", startBody(cfg, hashes), true)
	h.both("healthcheck after start", "GET", "/node/xray/healthcheck", nil, false)
	h.resources("idle, xray running")

	// Incremental user changes, exactly as the panel sends them.
	u4, u5 := newUser(4), newUser(5)
	h.both("add-user", "POST", "/node/handler/add-user", map[string]any{
		"data": []any{
			map[string]any{"type": "vless", "tag": "VLESS", "username": u4.id, "uuid": u4.vless, "flow": ""},
			map[string]any{"type": "trojan", "tag": "TROJAN", "username": u4.id, "password": u4.trojan},
			map[string]any{"type": "shadowsocks", "tag": "SS", "username": u4.id, "password": u4.ss, "cipherType": 6, "ivCheck": false},
			map[string]any{"type": "hysteria", "tag": "HY", "username": u4.id, "password": u4.hy},
		},
		"hashData": map[string]any{"vlessUuid": u4.vless},
	}, false)
	h.both("add-users", "POST", "/node/handler/add-users", map[string]any{
		"affectedInboundTags": []any{"VLESS", "TROJAN", "SS", "HY"},
		"users": []any{map[string]any{
			"inboundData": []any{
				map[string]any{"type": "vless", "tag": "VLESS", "flow": ""},
				map[string]any{"type": "trojan", "tag": "TROJAN"},
				map[string]any{"type": "shadowsocks", "tag": "SS"},
				map[string]any{"type": "hysteria", "tag": "HY"},
			},
			"userData": map[string]any{"userId": u5.id, "hashUuid": u5.vless, "vlessUuid": u5.vless, "trojanPassword": u5.trojan, "ssPassword": u5.ss},
		}},
	}, true)
	cfg45, hashes45 := xrayConfig(append(append([]user{}, users...), u4, u5), cert)
	_ = cfg45
	h.both("xray/start after add-user(s) with the panel's new hashes", "POST", "/node/xray/start", startBody(cfg, hashes45), true)
	h.both("remove-user", "POST", "/node/handler/remove-user", map[string]any{"username": u4.id, "hashData": map[string]any{"vlessUuid": u4.vless}}, false)
	h.both("remove-users", "POST", "/node/handler/remove-users", map[string]any{"users": []any{map[string]any{"userId": u5.id, "hashUuid": u5.vless}}}, true)
	h.both("xray/start after removals with the original hashes", "POST", "/node/xray/start", startBody(cfg, hashes), true)
	h.diffConfigs("generated xray config after user changes (start skipped)")

	// Stats before traffic.
	h.both("get-users-stats reset", "POST", "/node/stats/get-users-stats", map[string]any{"reset": true}, false)
	h.both("get-combined-stats reset", "POST", "/node/stats/get-combined-stats", map[string]any{"reset": true}, false)
	h.both("online status before traffic", "POST", "/node/stats/get-user-online-status", map[string]any{"username": "1"}, false)

	// Real traffic from a client container: users 1/2/3 over VLESS, Trojan
	// and Hysteria 2 (self-signed cert pinned by sha256) through each node.
	cli := h.startClient(id, netName, users, cert)
	containers = append(containers, cli.name)
	target := web + ":8080"
	for _, n := range []*node{h.ts, h.gn} {
		for _, proto := range []string{"vless", "trojan", "hy"} {
			got, err := socksGet(cli.socks[n.label+"-"+proto], target, "/f")
			if err != nil || got != payloadSize {
				h.dumpLogs()
				t.Fatalf("download through %s/%s: %d bytes, %v\n%s", n.label, proto, got, err, docker(t, "logs", "--tail", "30", cli.name))
			}
		}
	}
	h.note("downloaded %d KiB through each of VLESS, Trojan, Hysteria 2 on both nodes", payloadSize>>10)
	h.resources("after traffic")
	h.compareCounters()

	// Held connections: online status and IP lists see the client container's address.
	var held []net.Conn
	for _, n := range []*node{h.ts, h.gn} {
		c, err := socksConnect(cli.socks[n.label+"-vless"], target)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "GET /f HTTP/1.1\r\nHost: x\r\n") // incomplete request keeps the upstream connection open
		held = append(held, c)
	}
	time.Sleep(300 * time.Millisecond)
	h.both("online status during traffic", "POST", "/node/stats/get-user-online-status", map[string]any{"username": "1"}, false)
	a, b = h.both("get-user-ip-list during traffic", "POST", "/node/stats/get-user-ip-list", map[string]any{"userId": "1"}, false)
	cliIP := docker(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", cli.name)
	for _, r := range []reply{a, b} {
		ips, _ := resp(r)["ips"].([]any)
		if len(ips) != 1 || ips[0].(map[string]any)["ip"] != cliIP {
			h.diff("get-user-ip-list during traffic", "want the client container's %s, got %v", cliIP, ips)
		}
	}
	h.both("get-users-ip-list during traffic", "GET", "/node/stats/get-users-ip-list", nil, false)
	for _, c := range held {
		c.Close()
	}

	h.both("get-all-inbounds-stats", "POST", "/node/stats/get-all-inbounds-stats", map[string]any{"reset": false}, false)
	h.both("get-all-outbounds-stats", "POST", "/node/stats/get-all-outbounds-stats", map[string]any{"reset": false}, false)
	h.both("get-inbound-stats", "POST", "/node/stats/get-inbound-stats", map[string]any{"tag": "HY", "reset": false}, false)
	h.both("get-outbound-stats", "POST", "/node/stats/get-outbound-stats", map[string]any{"tag": "DIRECT", "reset": false}, false)
	h.both("get-system-stats", "GET", "/node/stats/get-system-stats", nil, false)
	// geocheck probes the internet; whether it fails fast or hits the 45 s
	// limit depends on the network, so only the error contract is compared.
	jwt := h.pki.PanelJWT()
	ga := h.call(h.ts, "POST", "/node/stats/get-geocheck", map[string]any{}, false, jwt)
	gb := h.call(h.gn, "POST", "/node/stats/get-geocheck", map[string]any{}, false, jwt)
	for _, r := range []reply{ga, gb} {
		m, _ := r.body.(map[string]any)
		if msg, _ := m["message"].(string); r.status != 500 || m["errorCode"] != "A018" || !strings.HasPrefix(msg, "Geocheck via default route ") {
			h.diff("get-geocheck (no network)", "want 500 A018 'Geocheck via default route …', got %d %v", r.status, m)
		}
	}
	h.note("get-geocheck without internet: TS %q / Go %q", ga.body.(map[string]any)["message"], gb.body.(map[string]any)["message"])
	h.both("get-geocheck invalid ip", "POST", "/node/stats/get-geocheck", map[string]any{"ip": "nope"}, false)
	h.both("nftables block-ips", "POST", "/node/plugin/nftables/block-ips", map[string]any{"ips": []any{map[string]any{"ip": "1.2.3.4", "timeout": 10}}}, false)
	h.both("nftables unblock-ips", "POST", "/node/plugin/nftables/unblock-ips", map[string]any{"ips": []any{"1.2.3.4"}}, false)
	h.both("nftables recreate-tables", "POST", "/node/plugin/nftables/recreate-tables", map[string]any{}, false)
	h.both("drop-users-connections", "POST", "/node/handler/drop-users-connections", map[string]any{"userIds": []any{"1"}}, false)
	h.both("drop-ips", "POST", "/node/handler/drop-ips", map[string]any{"ips": []any{"1.2.3.4"}}, false)

	// Torrent blocker: enable, push, then a real BitTorrent handshake from user 1.
	plugin := map[string]any{"plugin": map[string]any{"uuid": "0d7c3f4e-2b1a-4c5d-9e8f-7a6b5c4d3e2f", "name": "parity", "config": map[string]any{
		"sharedLists":    []any{map[string]any{"type": "ipList", "name": "ext:office", "items": []any{"203.0.113.0/24"}}},
		"torrentBlocker": map[string]any{"enabled": true, "blockDuration": 3600, "ignoreLists": map[string]any{"ip": []any{"ext:office"}}},
	}}}
	h.both("plugin sync: torrent blocker on", "POST", "/node/plugin/sync", plugin, true)
	h.both("xray/start with torrent blocker", "POST", "/node/xray/start", startBody(cfg, hashes), true)
	h.diffConfigs("generated xray config with torrent blocker")
	for _, n := range []*node{h.ts, h.gn} {
		if err := socksBitTorrent(cli.socks[n.label+"-vless"], target); err != nil {
			h.note("%s: bittorrent handshake: %v", n.label, err)
		}
	}
	time.Sleep(3 * time.Second)
	a, b = h.both("torrent-blocker collect", "POST", "/node/plugin/torrent-blocker/collect", map[string]any{}, false)
	for _, r := range []reply{a, b} {
		if reps, _ := resp(r)["reports"].([]any); len(reps) != 1 {
			h.diff("torrent-blocker collect", "want 1 report per node, got %v", resp(r))
		}
	}
	// User 1 is suspended on both: VLESS as user 1 no longer works, user 2 (Trojan) still does.
	for _, n := range []*node{h.ts, h.gn} {
		got1, _ := socksGet(cli.socks[n.label+"-vless"], target, "/f")
		got2, _ := socksGet(cli.socks[n.label+"-trojan"], target, "/f")
		h.note("%s after the block: user 1 got %d bytes, user 2 got %d bytes", n.label, got1, got2)
		if got1 != 0 || got2 != payloadSize {
			h.diff("suspension", "%s: user 1 must be cut off and user 2 unaffected (got %d / %d)", n.label, got1, got2)
		}
	}
	h.both("get-system-stats with a report collected", "GET", "/node/stats/get-system-stats", nil, false)
	h.both("plugin sync null (disable)", "POST", "/node/plugin/sync", map[string]any{"plugin": nil}, true)

	// Stop.
	h.both("xray/stop", "GET", "/node/xray/stop", nil, false)
	h.both("get-system-stats after stop", "GET", "/node/stats/get-system-stats", nil, false)
	h.both("healthcheck after stop", "GET", "/node/xray/healthcheck", nil, false)

	if len(h.diffs) == 0 {
		h.note("0 diffs")
	}
}

func (h *harness) waitReady(n *node) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		r := h.call(n, "GET", "/node/xray/healthcheck", nil, false, h.pki.PanelJWT())
		if !r.dropped && r.status == 200 {
			return
		}
		if time.Now().After(deadline) {
			out, _ := dockerRun("logs", "--tail", "50", n.name)
			h.t.Fatalf("%s never became ready:\n%s", n.label, out)
		}
		time.Sleep(time.Second)
	}
}

func (h *harness) dumpLogs() {
	dir := os.Getenv("PARITY_DUMP")
	for _, n := range []*node{h.ts, h.gn} {
		out, _ := dockerRun("logs", "--tail", "40", n.name)
		h.t.Logf("--- %s logs ---\n%s", n.label, out)
		if dir == "" {
			continue
		}
		_, _ = dockerRun("cp", n.name+":/var/log/xray/current", dir+"/xray-"+n.label+".log")
		if b, err := json.MarshalIndent(h.getConfig(n), "", "  "); err == nil {
			_ = os.WriteFile(dir+"/config-"+n.label+".json", b, 0o644)
		}
	}
}

// getConfig reads /internal/get-config over the node's abstract socket from
// a helper container sharing its network namespace.
func (h *harness) getConfig(n *node) map[string]any {
	js := `const http=require('http');const [s,t]=process.argv.slice(1);` +
		`http.get({socketPath:'\0'+s,path:'/internal/get-config?token='+encodeURIComponent(t)},r=>{let d='';r.on('data',c=>d+=c);r.on('end',()=>process.stdout.write(d))}).on('error',e=>{console.error(String(e));process.exit(1)})`
	out := docker(h.t, "run", "--rm", "--platform", h.platform, "--network", "container:"+n.name, "--entrypoint", "node", h.tsImage, "-e", js, n.sock, n.token)
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		h.t.Fatalf("%s get-config: %v\n%.300s", n.label, err, out)
	}
	return m
}

// diffConfigs compares the xray configs both nodes generated. The API
// socket name and the webhook URL embed per-container random values.
func (h *harness) diffConfigs(step string) {
	a, b := h.getConfig(h.ts), h.getConfig(h.gn)
	sa, _ := json.Marshal(a)
	sb, _ := json.Marshal(b)
	clean := func(s, sock, token string) string {
		api := strings.Replace(sock, "rwint-", "xtls-api-", 1)
		s = strings.ReplaceAll(s, token, "<token>")
		s = strings.ReplaceAll(s, api, "<api-socket>")
		return strings.ReplaceAll(s, sock, "<internal-socket>")
	}
	ca, cb := clean(string(sa), h.ts.sock, h.ts.token), clean(string(sb), h.gn.sock, h.gn.token)
	var na, nb any
	_ = json.Unmarshal([]byte(ca), &na)
	_ = json.Unmarshal([]byte(cb), &nb)
	if !reflect.DeepEqual(na, nb) {
		ja, _ := json.MarshalIndent(na, "", " ")
		jb, _ := json.MarshalIndent(nb, "", " ")
		h.diff(step, "generated configs differ\nTS: %s\nGo: %s", ja, jb)
		return
	}
	h.note("%s: identical (%d bytes)", step, len(ca))
}

// compareCounters checks the user counters after identical traffic.
func (h *harness) compareCounters() {
	a, b := h.both("get-users-stats after traffic", "POST", "/node/stats/get-users-stats", map[string]any{"reset": false}, false)
	read := func(r reply) map[string][2]int64 {
		out := map[string][2]int64{}
		for _, u := range resp(r)["users"].([]any) {
			m := u.(map[string]any)
			out[m["username"].(string)] = [2]int64{num(m["downlink"]), num(m["uplink"])}
		}
		return out
	}
	ca, cb := read(a), read(b)
	for _, id := range []string{"1", "2", "3"} {
		ta, tb := ca[id], cb[id]
		h.note("user %s counters: TS down=%d up=%d, Go down=%d up=%d", id, ta[0], ta[1], tb[0], tb[1])
		if ta[0] < payloadSize || tb[0] < payloadSize {
			h.diff("traffic counters", "user %s downlink below the %d-byte payload (TS %d, Go %d)", id, payloadSize, ta[0], tb[0])
		}
		if d := ta[0] - tb[0]; d > ta[0]/100 || -d > ta[0]/100 {
			h.diff("traffic counters", "user %s downlink differs by more than 1%%: TS %d, Go %d", id, ta[0], tb[0])
		}
	}
	h.both("get-users-stats reset after traffic", "POST", "/node/stats/get-users-stats", map[string]any{"reset": true}, false)
	a, b = h.both("get-users-stats after reset", "POST", "/node/stats/get-users-stats", map[string]any{"reset": false}, false)
	for _, r := range []reply{a, b} {
		for _, c := range read(r) {
			if c[0] > 4096 {
				h.diff("traffic counters", "counter after reset: %v", c)
			}
		}
	}
}

// resources records docker stats for both nodes.
func (h *harness) resources(when string) {
	for _, n := range []*node{h.ts, h.gn} {
		out, err := dockerRun("stats", "--no-stream", "--format", "{{.MemUsage}} | CPU {{.CPUPerc}} | PIDs {{.PIDs}}", n.name)
		if err == nil {
			h.note("resources %s, %s: %s", when, n.label, out)
		}
		// Per process: the node itself vs xray (RSS in KiB from ps).
		if top, err := dockerRun("top", n.name, "-eo", "rss,comm"); err == nil {
			var parts []string
			for _, l := range strings.Split(top, "\n")[1:] {
				f := strings.Fields(l)
				if len(f) >= 2 {
					if kb, err := strconv.Atoi(f[0]); err == nil && kb > 4096 {
						parts = append(parts, fmt.Sprintf("%s %.1f MiB", strings.Join(f[1:], " "), float64(kb)/1024))
					}
				}
			}
			h.note("rss %s, %s: %s", when, n.label, strings.Join(parts, ", "))
		}
	}
}

func (h *harness) writeReport() {
	path := os.Getenv("PARITY_REPORT")
	if path == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Parity run %s\n\nTS image: `%s`  \nGo image: `%s`  \nPlatform: %s\n\n## Diffs: %d\n\n", time.Now().Format(time.RFC3339), os.Getenv("PARITY_TS_IMAGE"), os.Getenv("PARITY_GO_IMAGE"), h.platform, len(h.diffs))
	for _, d := range h.diffs {
		fmt.Fprintf(&b, "```\n%s\n```\n\n", d)
	}
	b.WriteString("## Log\n\n")
	for _, l := range h.log {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	b.WriteString("\n## Raw responses\n\n")
	for _, r := range h.raw {
		b.WriteString(r + "\n")
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

// ---- client container -------------------------------------------------------

type client struct {
	name  string
	socks map[string]string // "<node>-<proto>" → host:port of its SOCKS inbound
}

// startClient runs xray as a client in its own container: one SOCKS inbound
// per node and protocol, published on the host's loopback.
func (h *harness) startClient(id, netName string, users []user, cert testpki.ServerCert) client {
	c := client{name: "rwp-cli-" + id, socks: map[string]string{}}
	var inbounds, outbounds, rules []any
	port := 10801
	var order []string
	for _, n := range []*node{h.ts, h.gn} {
		outs := map[string]map[string]any{
			"vless": {"protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": n.name, "port": portVless,
				"users": []any{map[string]any{"id": users[0].vless, "encryption": "none"}}}}}},
			"trojan": {"protocol": "trojan", "settings": map[string]any{"servers": []any{map[string]any{"address": n.name, "port": portTrojan, "password": users[1].trojan}}}},
			"hy": {"protocol": "hysteria", "settings": map[string]any{"version": 2, "address": n.name, "port": portHY},
				"streamSettings": map[string]any{"network": "hysteria", "security": "tls",
					"hysteriaSettings": map[string]any{"version": 2, "auth": users[2].hy},
					"tlsSettings":      map[string]any{"serverName": "node.test", "alpn": []any{"h3"}, "pinnedPeerCertSha256": cert.SHA256}}},
		}
		for _, proto := range []string{"vless", "trojan", "hy"} {
			key := n.label + "-" + proto
			ob := outs[proto]
			ob["tag"] = "out-" + key
			outbounds = append(outbounds, ob)
			inbounds = append(inbounds, map[string]any{"tag": "in-" + key, "listen": "0.0.0.0", "port": port, "protocol": "socks", "settings": map[string]any{"udp": false}})
			rules = append(rules, map[string]any{"inboundTag": []any{"in-" + key}, "outboundTag": "out-" + key})
			order = append(order, key)
			port++
		}
	}
	cfg, _ := json.Marshal(map[string]any{"log": map[string]any{"loglevel": "warning"}, "inbounds": inbounds, "outbounds": outbounds, "routing": map[string]any{"rules": rules}})
	f, _ := os.CreateTemp("", "rwp-client-*.json")
	_, _ = f.Write(cfg)
	f.Close()
	defer os.Remove(f.Name())
	args := []string{"create", "--name", c.name, "--network", netName, "--platform", h.platform, "--entrypoint", "/usr/local/bin/xray"}
	for i := range order {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", 10801+i))
	}
	docker(h.t, append(args, os.Getenv("PARITY_GO_IMAGE"), "run", "-c", "/tmp/client.json")...)
	docker(h.t, "cp", f.Name(), c.name+":/tmp/client.json")
	docker(h.t, "start", c.name)
	for i, key := range order {
		hp := strings.Split(docker(h.t, "port", c.name, fmt.Sprintf("%d/tcp", 10801+i)), "\n")[0]
		c.socks[key] = hp
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if conn, err := socksConnect(c.socks[order[0]], "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "greeting") {
			if conn != nil {
				conn.Close()
			}
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("client xray did not come up: %s", docker(h.t, "logs", c.name))
		}
		time.Sleep(300 * time.Millisecond)
	}
	return c
}

// socksConnect opens a SOCKS5 CONNECT to target (host:port, domain or IP).
func socksConnect(proxy, target string) (net.Conn, error) {
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("greeting: %w", err)
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 262)
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		c.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("greeting: %v %x", err, buf[:2])
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host).To4(); ip != nil {
		req = append(append(req, 1), ip...)
	} else {
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	if _, err := io.ReadFull(c, buf[:10]); err != nil || buf[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks connect: %v %x", err, buf[:10])
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// socksGet downloads path from target through the proxy and returns the body length.
func socksGet(proxy, target, path string) (int, error) {
	c, err := socksConnect(proxy, target)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, target)
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	defer r.Body.Close()
	n, err := io.Copy(io.Discard, r.Body)
	return int(n), err
}

// socksBitTorrent sends a BitTorrent handshake through the proxy, which
// xray's sniffer classifies as bittorrent.
func socksBitTorrent(proxy, target string) error {
	c, err := socksConnect(proxy, target)
	if err != nil {
		return err
	}
	defer c.Close()
	hs := append([]byte{19}, "BitTorrent protocol"...)
	hs = append(hs, make([]byte, 8)...)
	id := make([]byte, 40)
	_, _ = rand.Read(id)
	hs = append(hs, id...)
	_, err = c.Write(hs)
	time.Sleep(500 * time.Millisecond)
	return err
}

func deriveSNI(t *testing.T, secret string) string {
	mat, _, err := secretkey.Load(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return mat.SNI
}
