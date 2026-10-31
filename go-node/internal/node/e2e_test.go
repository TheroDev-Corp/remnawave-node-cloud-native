package node

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	handlercmd "github.com/xtls/xray-core/app/proxyman/command"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/hashedset"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func b64key(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// panel plays the panel: mTLS client cert, RS256 JWT, zstd bodies.
type panel struct {
	t      *testing.T
	pki    *testpki.PKI
	sni    string
	base   string
	client *http.Client
	checks []map[string]any
}

func (p *panel) call(method, path, command string, body any, zstdBody bool) (int, map[string]any, error) {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		if zstdBody {
			enc, _ := zstd.NewWriter(nil, zstd.WithWindowSize(1<<25))
			raw = enc.EncodeAll(raw, nil)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, p.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+p.pki.PanelJWT())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if zstdBody {
			req.Header.Set("Content-Encoding", "zstd")
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	_ = dec.Decode(&out)
	if command != "" && resp.StatusCode/100 == 2 {
		p.checks = append(p.checks, map[string]any{"command": command, "kind": "response", "value": out})
	}
	return resp.StatusCode, out, nil
}

func (p *panel) must(method, path, command string, body any, zstdBody bool, wantStatus int) map[string]any {
	p.t.Helper()
	code, out, err := p.call(method, path, command, body, zstdBody)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	if code != wantStatus {
		p.t.Fatalf("%s %s: status %d, want %d, body %v", method, path, code, wantStatus, out)
	}
	return out
}

func resp(m map[string]any) map[string]any { r, _ := m["response"].(map[string]any); return r }

type testUser struct{ id, vless, trojan, ss, hy string }

func newUser(id int) testUser {
	return testUser{fmt.Sprint(id), uuid(), b64key(12), b64key(16), b64key(12)}
}

// hyCert is the self-signed certificate on the Hysteria inbound; the test
// client pins it by sha256, as an operator without a public CA would.
var hyCert = testpki.SelfSigned("node.test", "127.0.0.1")

func pemLines(s string) []any {
	var out []any
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		out = append(out, l)
	}
	return out
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

var managedTags = []string{"VLESS", "TROJAN", "SS", "SS22", "HY"}

// xrayConfigFor builds the config the panel would push: five managed
// inbounds (vless, trojan, shadowsocks aes-256-gcm, shadowsocks 2022,
// hysteria 2 over TLS with a self-signed cert) with every client carrying
// id = vlessUuid, as xray-config.validator.ts does.
func xrayConfigFor(t *testing.T, users []testUser, ss22ServerKey string) (map[string]any, Hashes) {
	ports := []int{freePort(t), freePort(t), freePort(t), freePort(t)}
	var vl, tr, ss, s22, hy []any
	sets := map[string]*hashedset.Set{}
	for _, tag := range managedTags {
		sets[tag] = hashedset.New()
	}
	for _, u := range users {
		vl = append(vl, map[string]any{"id": u.vless, "email": u.id})
		tr = append(tr, map[string]any{"password": u.trojan, "email": u.id, "id": u.vless})
		ss = append(ss, map[string]any{"password": u.ss, "method": "aes-256-gcm", "email": u.id, "id": u.vless})
		s22 = append(s22, map[string]any{"password": b64key(16), "email": u.id, "id": u.vless})
		hy = append(hy, map[string]any{"auth": u.hy, "email": u.id, "id": u.vless})
		for _, s := range sets {
			s.Add(u.vless)
		}
	}
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "VLESS", "listen": "127.0.0.1", "port": ports[0], "protocol": "vless", "settings": map[string]any{"clients": vl, "decryption": "none"}},
			map[string]any{"tag": "TROJAN", "listen": "127.0.0.1", "port": ports[1], "protocol": "trojan", "settings": map[string]any{"clients": tr}},
			map[string]any{"tag": "SS", "listen": "127.0.0.1", "port": ports[2], "protocol": "shadowsocks", "settings": map[string]any{"clients": ss, "network": "tcp,udp"}},
			map[string]any{"tag": "SS22", "listen": "127.0.0.1", "port": ports[3], "protocol": "shadowsocks", "settings": map[string]any{"method": "2022-blake3-aes-128-gcm", "password": ss22ServerKey, "clients": s22, "network": "tcp,udp"}},
			map[string]any{"tag": "HY", "listen": "127.0.0.1", "port": freeUDPPort(t), "protocol": "hysteria",
				"settings": map[string]any{"version": 2, "clients": hy},
				"streamSettings": map[string]any{"network": "hysteria", "security": "tls", "hysteriaSettings": map[string]any{"version": 2},
					"tlsSettings": map[string]any{"alpn": []any{"h3"}, "certificates": []any{map[string]any{
						"certificate": pemLines(hyCert.CertPEM), "key": pemLines(hyCert.KeyPEM)}}}}},
		},
		// xray ≥ 26 blackholes private targets for proxied users by default;
		// the test's payload server lives on loopback, so allow just that.
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "DIRECT", "settings": map[string]any{
			"finalRules": []any{map[string]any{"action": "allow", "ip": []any{"127.0.0.1/32"}}}}}},
		"routing": map[string]any{"rules": []any{}},
	}
	var h Hashes
	h.EmptyConfig = "empty-config-hash-1"
	for _, tag := range managedTags {
		h.Inbounds = append(h.Inbounds, struct {
			UsersCount int    `json:"usersCount"`
			Hash       string `json:"hash"`
			Tag        string `json:"tag"`
		}{sets[tag].Len(), sets[tag].Hash64String(), tag})
	}
	return cfg, h
}

func startBody(cfg map[string]any, h Hashes, force bool) map[string]any {
	return map[string]any{
		"internals":  map[string]any{"forceRestart": force, "hashes": h, "metadata": map[string]any{"uuid": uuid(), "name": "test", "countryCode": "XX", "id": 1, "tags": []any{}}},
		"xrayConfig": cfg,
	}
}

func TestPanelContractEndToEnd(t *testing.T) {
	bin := os.Getenv("XRAY_BINARY")
	if bin == "" {
		t.Skip("set XRAY_BINARY to run the end-to-end contract test against a real xray")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pki := testpki.New()
	mat, _, err := secretkey.Load(pki.SecretKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{XrayBinary: bin, InternalSocketPath: "rwint-test", InternalRestToken: "tok"}
	apiListen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	xapi, _ := xray.Dial(apiListen)
	proc := xray.NewProcess(bin, "", log)
	n := New(cfg, log, ha.Standalone{ID: "test-pod"}, proc, xapi, apiListen, nil)
	n.CoreLink = t.TempDir() + "/rw-core" // keep the core loader away from /usr/local/bin
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

	// Must-match: get-system-stats is non-2xx while xray is down.
	if code, body, _ := p.call("GET", "/node/stats/get-system-stats", "", nil, false); code != 500 || body["errorCode"] != "A010" {
		t.Fatalf("system stats with xray down: %d %v", code, body)
	}
	hc := resp(p.must("GET", "/node/xray/healthcheck", "GetNodeHealthCheckCommand", nil, false, 200))
	if hc["isAlive"] != true || hc["xrayInternalStatusCached"] != false || hc["xrayVersion"] == nil {
		t.Fatalf("healthcheck before start: %v", hc)
	}
	var semverOK []bool
	nodefixture.Run(t, map[string]any{"op": "semverGte", "versions": []any{hc["nodeVersion"]}, "min": "2.7.0"}, &semverOK)
	if !semverOK[0] {
		t.Fatalf("nodeVersion %v must be valid semver ≥ 2.7.0", hc["nodeVersion"])
	}

	if r := resp(p.must("POST", "/node/plugin/sync", "SyncCommand", map[string]any{"plugin": nil}, true, 201)); r["accepted"] != false {
		t.Errorf("sync(null) with no active plugin → accepted=false, got %v", r)
	}

	users := []testUser{newUser(1), newUser(2), newUser(3)}
	ss22Key := b64key(16)
	xc, hashes := xrayConfigFor(t, users, ss22Key)
	st := resp(p.must("POST", "/node/xray/start", "StartXrayCommand", startBody(xc, hashes, false), true, 201))
	if st["isStarted"] != true {
		t.Fatalf("xray/start: %v\n%s", st, strings.Join(proc.Tail(10), "\n"))
	}
	pid := proc.Status().PID
	if sys, _ := st["system"].(map[string]any); sys["stats"].(map[string]any)["interface"] != nil {
		t.Error("xray/start reports system.stats.interface = null like the TS node")
	}

	// Same hashes again: no restart (HashedSet + emptyConfig match).
	if resp(p.must("POST", "/node/xray/start", "StartXrayCommand", startBody(xc, hashes, false), true, 201))["isStarted"] != true || proc.Status().PID != pid {
		t.Fatalf("identical push must not restart xray (pid %d → %d)", pid, proc.Status().PID)
	}

	// Single add-user per protocol, exactly as the panel's add-user-to-node handler sends it.
	u4 := newUser(4)
	add := map[string]any{
		"data": []any{
			map[string]any{"type": "vless", "tag": "VLESS", "username": u4.id, "uuid": u4.vless, "flow": ""},
			map[string]any{"type": "trojan", "tag": "TROJAN", "username": u4.id, "password": u4.trojan},
			map[string]any{"type": "shadowsocks", "tag": "SS", "username": u4.id, "password": u4.ss, "cipherType": 6, "ivCheck": false},
			map[string]any{"type": "shadowsocks22", "tag": "SS22", "username": u4.id, "password": b64key(16)},
			map[string]any{"type": "hysteria", "tag": "HY", "username": u4.id, "password": u4.hy},
		},
		"hashData": map[string]any{"vlessUuid": u4.vless},
	}
	if r := resp(p.must("POST", "/node/handler/add-user", "AddUserCommand", add, false, 201)); r["success"] != true {
		t.Fatalf("add-user: %v", r)
	}
	// Ask xray itself: u4 must now be a user of all five inbounds.
	for _, tag := range managedTags {
		r, err := xapi.Handler.GetInboundUsers(t.Context(), &handlercmd.GetInboundUserRequest{Tag: tag, Email: u4.id})
		if err != nil || len(r.Users) != 1 || r.Users[0].Email != u4.id {
			t.Errorf("xray has no user %s in %s after add-user: %v %v", u4.id, tag, r, err)
		}
	}
	// The node's per-inbound hash now equals the panel's view with u4.
	users = append(users, u4)
	xc2, hashes2 := xrayConfigFor(t, users, ss22Key)
	for _, in := range hashes2.Inbounds {
		if got, _ := n.State.InboundHash(in.Tag); got != in.Hash {
			t.Errorf("%s hash after add-user: node %s, panel %s", in.Tag, got, in.Hash)
		}
	}
	_ = xc2
	if resp(p.must("POST", "/node/xray/start", "StartXrayCommand", startBody(xc, hashes2, false), true, 201))["isStarted"] != true || proc.Status().PID != pid {
		t.Fatal("push after incremental add-user must not restart xray")
	}

	// Bulk add (zstd), with ss22 key base64(ssPassword) and cipherType 0 for ss.
	u5 := newUser(5)
	addMany := map[string]any{
		"affectedInboundTags": []any{"VLESS", "TROJAN", "SS", "SS22", "HY"},
		"users": []any{map[string]any{
			"inboundData": []any{
				map[string]any{"type": "vless", "tag": "VLESS", "flow": ""},
				map[string]any{"type": "trojan", "tag": "TROJAN"},
				map[string]any{"type": "shadowsocks", "tag": "SS"},
				map[string]any{"type": "shadowsocks22", "tag": "SS22"},
				map[string]any{"type": "hysteria", "tag": "HY"},
			},
			"userData": map[string]any{"userId": u5.id, "hashUuid": u5.vless, "vlessUuid": u5.vless, "trojanPassword": u5.trojan, "ssPassword": "1234567890123456"},
		}},
	}
	if r := resp(p.must("POST", "/node/handler/add-users", "AddUsersCommand", addMany, true, 201)); r["success"] != true {
		t.Fatalf("add-users: %v", r)
	}
	def, ok := n.State.Definition(u5.id)
	if !ok || def.Ops[3].Password != base64.StdEncoding.EncodeToString([]byte("1234567890123456")) || def.Ops[2].Cipher != 0 {
		t.Errorf("stored definition must hold the applied values (ss22 base64 key, ss cipher 0): %+v", def)
	}

	for _, tag := range managedTags {
		r, err := xapi.Handler.GetInboundUsers(t.Context(), &handlercmd.GetInboundUserRequest{Tag: tag, Email: u5.id})
		if err != nil || len(r.Users) != 1 {
			t.Errorf("xray has no user %s in %s after add-users: %v %v", u5.id, tag, r, err)
		}
	}
	if r := resp(p.must("POST", "/node/handler/remove-user", "RemoveUserCommand", map[string]any{"username": u4.id, "hashData": map[string]any{"vlessUuid": u4.vless}}, false, 201)); r["success"] != true {
		t.Fatalf("remove-user: %v", r)
	}
	if r := resp(p.must("POST", "/node/handler/remove-users", "RemoveUsersCommand", map[string]any{"users": []any{map[string]any{"userId": u5.id, "hashUuid": u5.vless}}}, true, 201)); r["success"] != true {
		t.Fatalf("remove-users: %v", r)
	}
	// With an email filter xray answers one entry, empty when the user is absent.
	for _, tag := range managedTags {
		if r, err := xapi.Handler.GetInboundUsers(t.Context(), &handlercmd.GetInboundUserRequest{Tag: tag, Email: u4.id}); err != nil || (len(r.Users) > 0 && r.Users[0].Email != "") {
			t.Errorf("remove-user left %s in xray's %s inbound: %v %v", u4.id, tag, r, err)
		}
	}
	// Back to the original user set: the hashes match the first push again.
	for _, in := range hashes.Inbounds {
		if got, _ := n.State.InboundHash(in.Tag); got != in.Hash {
			t.Errorf("%s hash after removals: node %s, panel %s", in.Tag, got, in.Hash)
		}
	}

	// Stats: shapes, numbers, and reset semantics. The API inbound counts
	// our own gRPC calls, so a reset leaves it near zero.
	p.must("POST", "/node/stats/get-users-stats", "GetUsersStatsCommand", map[string]any{"reset": true}, false, 201)
	p.must("POST", "/node/stats/get-combined-stats", "GetCombinedStatsCommand", map[string]any{"reset": true}, false, 201)
	c2 := resp(p.must("POST", "/node/stats/get-combined-stats", "GetCombinedStatsCommand", map[string]any{"reset": true}, false, 201))
	for _, in := range c2["inbounds"].([]any) {
		m := in.(map[string]any)
		if m["inbound"] == apiInboundTag {
			if up, _ := m["uplink"].(json.Number).Int64(); up > 4096 {
				t.Errorf("reset:true should have zeroed the counters, API inbound uplink=%d", up)
			}
		}
	}
	if r := resp(p.must("POST", "/node/stats/get-user-online-status", "GetUserOnlineStatusCommand", map[string]any{"username": "1"}, false, 201)); r["isOnline"] != false {
		t.Errorf("online status before any traffic: %v", r)
	}

	// Real traffic: a second xray plays the client. User 1 goes through
	// Hysteria 2 (TLS, self-signed cert pinned by sha256), user 2 through
	// VLESS. The node's counters, online status and IP list must see it.
	const payload = 512 << 10
	target := startPayloadServer(t, payload)
	hySocks, vlSocks, clientLog := startClientXray(t, bin, xc, users[0], users[1])
	for _, via := range []string{hySocks, vlSocks} {
		if got, err := socksGet(via, target); err != nil || got < payload {
			t.Fatalf("download via %s: %d bytes, %v\nclient xray:\n%s\nnode xray:\n%s", via, got, err, clientLog(), strings.Join(proc.Tail(20), "\n"))
		}
	}
	us := resp(p.must("POST", "/node/stats/get-users-stats", "GetUsersStatsCommand", map[string]any{"reset": false}, false, 201))
	down := map[string]int64{}
	for _, u := range us["users"].([]any) {
		m := u.(map[string]any)
		down[m["username"].(string)], _ = m["downlink"].(json.Number).Int64()
	}
	if down["1"] < payload || down["2"] < payload {
		t.Errorf("user downlink after %d-byte downloads: %v", payload, down)
	}
	if r := resp(p.must("POST", "/node/stats/get-user-online-status", "GetUserOnlineStatusCommand", map[string]any{"username": "1"}, false, 201)); r["isOnline"] != true {
		t.Errorf("hysteria user must be online after traffic: %v", r)
	}
	ips := resp(p.must("POST", "/node/stats/get-user-ip-list", "GetUserIpListCommand", map[string]any{"userId": "1"}, false, 201))["ips"].([]any)
	// xray never records 127.0.0.1 in a user's IP list (app/stats/online_map.go);
	// the container parity harness covers real client IPs.
	if len(ips) != 0 {
		t.Errorf("user 1 ip list from loopback: %v", ips)
	}
	hyIn := resp(p.must("POST", "/node/stats/get-inbound-stats", "GetInboundStatsCommand", map[string]any{"tag": "HY", "reset": false}, false, 201))
	if d, _ := hyIn["downlink"].(json.Number).Int64(); d < payload {
		t.Errorf("HY inbound downlink: %v", hyIn)
	}
	// reset:true hands out the counters once, then they start over.
	p.must("POST", "/node/stats/get-users-stats", "GetUsersStatsCommand", map[string]any{"reset": true}, false, 201)
	for _, u := range resp(p.must("POST", "/node/stats/get-users-stats", "GetUsersStatsCommand", map[string]any{"reset": false}, false, 201))["users"].([]any) {
		if d, _ := u.(map[string]any)["downlink"].(json.Number).Int64(); d > 4096 {
			t.Errorf("user counter after reset: %v", u)
		}
	}

	p.must("POST", "/node/stats/get-all-inbounds-stats", "GetAllInboundsStatsCommand", map[string]any{"reset": false}, false, 201)
	p.must("POST", "/node/stats/get-all-outbounds-stats", "GetAllOutboundsStatsCommand", map[string]any{"reset": false}, false, 201)
	p.must("POST", "/node/stats/get-inbound-stats", "GetInboundStatsCommand", map[string]any{"tag": "VLESS", "reset": false}, false, 201)
	p.must("POST", "/node/stats/get-outbound-stats", "GetOutboundStatsCommand", map[string]any{"tag": "DIRECT", "reset": false}, false, 201)
	p.must("GET", "/node/stats/get-users-ip-list", "GetUsersIpListCommand", nil, false, 200)
	sys := resp(p.must("GET", "/node/stats/get-system-stats", "GetSystemStatsCommand", nil, false, 200))
	if sys["xrayInfo"] == nil {
		t.Error("system stats must carry xrayInfo while xray runs")
	}
	if code, body, _ := p.call("POST", "/node/stats/get-geocheck", "", map[string]any{}, false); code != 500 || body["errorCode"] != "A018" {
		t.Errorf("geocheck without the binary → 500 A018, got %d %v", code, body)
	}

	// Plugins and the fork's no-op routes.
	p.must("POST", "/node/plugin/torrent-blocker/collect", "CollectReportsCommand", map[string]any{}, false, 201)
	p.must("POST", "/node/plugin/nftables/block-ips", "BlockIpsCommand", map[string]any{"ips": []any{map[string]any{"ip": "1.2.3.4", "timeout": 10}}}, false, 201)
	p.must("POST", "/node/plugin/nftables/unblock-ips", "UnblockIpsCommand", map[string]any{"ips": []any{"1.2.3.4"}}, false, 201)
	p.must("POST", "/node/plugin/nftables/recreate-tables", "RecreateTablesCommand", map[string]any{}, false, 201)
	p.must("POST", "/node/handler/drop-users-connections", "DropUsersConnectionsCommand", map[string]any{"userIds": []any{"1"}}, false, 201)
	p.must("POST", "/node/handler/drop-ips", "DropIpsCommand", map[string]any{"ips": []any{"1.2.3.4"}}, false, 201)

	// Validation: a malformed body gets 400 like ZodValidationPipe.
	if code, _, _ := p.call("POST", "/node/stats/get-users-stats", "", map[string]any{"reset": "yes"}, false); code != 400 {
		t.Errorf("invalid body → 400, got %d", code)
	}

	// Torrent-blocker plugin: enabling it restarts xray with the blackhole outbound and webhook rule.
	plugin := map[string]any{"plugin": map[string]any{"uuid": uuid(), "name": "tb", "config": map[string]any{
		"sharedLists":    []any{map[string]any{"type": "ipList", "name": "ext:office", "items": []any{"10.0.0.0/8"}}},
		"torrentBlocker": map[string]any{"enabled": true, "blockDuration": 60, "ignoreLists": map[string]any{"ip": []any{"ext:office"}}},
	}}}
	if r := resp(p.must("POST", "/node/plugin/sync", "SyncCommand", plugin, true, 201)); r["accepted"] != true {
		t.Fatalf("plugin sync: %v", r)
	}
	if n.IsOnline() {
		t.Error("enabling the torrent blocker stops xray so the next push regenerates the config")
	}
	if resp(p.must("POST", "/node/xray/start", "StartXrayCommand", startBody(xc, hashes, false), true, 201))["isStarted"] != true {
		t.Fatalf("restart with torrent blocker: %s", strings.Join(proc.Tail(10), "\n"))
	}
	gen := n.State.XrayConfig()
	found := false
	for _, ob := range gen["outbounds"].([]any) {
		if ob.(map[string]any)["tag"] == torrentOutboundTag {
			found = true
		}
	}
	if !found {
		t.Error("generated config lacks the torrent-blocker outbound")
	}

	// Torrent-blocker flow with the fork's restore bugs fixed. A 1 s block
	// changes only the config hash: no restart needed.
	plugin["plugin"].(map[string]any)["config"].(map[string]any)["torrentBlocker"].(map[string]any)["blockDuration"] = 1
	p.must("POST", "/node/plugin/sync", "SyncCommand", plugin, true, 201)
	if !n.IsOnline() {
		t.Fatal("changing only blockDuration must not stop xray")
	}
	ss22Client := xc["inbounds"].([]any)[3].(map[string]any)["settings"].(map[string]any)["clients"].([]any)[0].(map[string]any)["password"]
	hook := func(email, source string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"email": email, "level": 0, "protocol": "bittorrent", "network": "tcp", "source": source,
			"destination": "tcp:1.1.1.1:6881", "routeTarget": nil, "originalTarget": nil, "inboundTag": "VLESS", "inboundName": nil,
			"inboundLocal": nil, "outboundTag": torrentOutboundTag, "ts": time.Now().Unix()})
		return b
	}
	userIn := func(tag, email string) (bool, string) {
		r, err := xapi.Handler.GetInboundUsers(t.Context(), &handlercmd.GetInboundUserRequest{Tag: tag, Email: email})
		if err != nil || len(r.Users) == 0 || r.Users[0].Email == "" {
			return false, ""
		}
		inst, _ := r.Users[0].Account.GetInstance()
		return true, fmt.Sprint(inst)
	}
	// An IP inside an ext: CIDR list is ignored (TS fork: exact match only, never ignored).
	n.Plugins.HandleWebhook(hook("1", "tcp:10.1.2.3:40000"))
	if ok, _ := userIn("VLESS", "1"); !ok || n.Plugins.ReportsCount() != 0 {
		t.Fatal("a client inside an ignored CIDR must not be suspended")
	}
	// User "1" came with the initial config (no add-user definition): the TS
	// fork could not restore such users; the Go node derives the definition.
	n.Plugins.HandleWebhook(hook("1", "tcp:203.0.113.9:40000"))
	for _, tag := range managedTags {
		if ok, _ := userIn(tag, "1"); ok {
			t.Errorf("suspended user still in %s", tag)
		}
	}
	collected := resp(p.must("POST", "/node/plugin/torrent-blocker/collect", "CollectReportsCommand", map[string]any{}, false, 201))
	if reps := collected["reports"].([]any); len(reps) != 1 {
		t.Fatalf("want 1 report, got %v", reps)
	}
	time.Sleep(1500 * time.Millisecond)
	for _, tag := range managedTags {
		if ok, _ := userIn(tag, "1"); !ok {
			t.Errorf("user not restored into %s after the block", tag)
		}
	}
	if _, acc := userIn("SS22", "1"); !strings.Contains(acc, fmt.Sprint(ss22Client)) {
		t.Errorf("restored SS2022 user must keep its key %v, xray has %s", ss22Client, acc)
	}

	// Stop: isStopped, then system stats fail again.
	if r := resp(p.must("GET", "/node/xray/stop", "StopXrayCommand", nil, false, 200)); r["isStopped"] != true {
		t.Fatalf("stop: %v", r)
	}
	if code, _, _ := p.call("GET", "/node/stats/get-system-stats", "", nil, false); code != 500 {
		t.Errorf("system stats after stop → 500, got %d", code)
	}

	// The checker itself must catch the classic mistake: a string counter.
	p.checks = append(p.checks, map[string]any{"command": "GetUsersStatsCommand", "kind": "response",
		"value": map[string]any{"response": map[string]any{"users": []any{map[string]any{"username": "1", "downlink": "5", "uplink": 0}}}}})
	// Every 2xx body above, checked by the TS contract's own zod schemas.
	var results []struct {
		OK      bool     `json:"ok"`
		Unknown []string `json:"unknown"`
		Issues  []string `json:"issues"`
	}
	nodefixture.Run(t, map[string]any{"op": "checkMany", "items": p.checks}, &results)
	if last := results[len(results)-1]; last.OK {
		t.Fatal("the zod checker accepted a string counter; the conformance check is not working")
	}
	results = results[:len(results)-1]
	for i, r := range results {
		cmd := p.checks[i]["command"]
		if !r.OK {
			t.Errorf("%s response rejected by the zod schema: %v", cmd, r.Issues)
		}
		if len(r.Unknown) > 0 && cmd != "StartXrayCommand" { // system.interface is a TS quirk, see StartXray
			t.Errorf("%s response has keys the contract doesn't know: %v", cmd, r.Unknown)
		}
	}
	t.Logf("%d responses checked against the TS zod schemas", len(results))
}

// startPayloadServer serves n random bytes on a loopback port.
func startPayloadServer(t *testing.T, n int) string {
	body := make([]byte, n)
	_, _ = rand.Read(body)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// startClientXray runs xray as a client with two SOCKS inbounds: one via
// Hysteria 2 as hyUser (pinning the node's self-signed cert), one via VLESS
// as vlUser. It returns the two SOCKS addresses.
func startClientXray(t *testing.T, bin string, server map[string]any, hyUser, vlUser testUser) (string, string, func() string) {
	port := func(tag string) any {
		for _, in := range server["inbounds"].([]any) {
			if m := in.(map[string]any); m["tag"] == tag {
				return m["port"]
			}
		}
		t.Fatalf("no inbound %s", tag)
		return nil
	}
	hySocks, vlSocks := freePort(t), freePort(t)
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "in-hy", "listen": "127.0.0.1", "port": hySocks, "protocol": "socks", "settings": map[string]any{"udp": false}},
			map[string]any{"tag": "in-vl", "listen": "127.0.0.1", "port": vlSocks, "protocol": "socks", "settings": map[string]any{"udp": false}},
		},
		"outbounds": []any{
			map[string]any{"tag": "hy", "protocol": "hysteria", "settings": map[string]any{"version": 2, "address": "127.0.0.1", "port": port("HY")},
				"streamSettings": map[string]any{"network": "hysteria", "security": "tls",
					"hysteriaSettings": map[string]any{"version": 2, "auth": hyUser.hy},
					"tlsSettings":      map[string]any{"serverName": "node.test", "alpn": []any{"h3"}, "pinnedPeerCertSha256": hyCert.SHA256}}},
			map[string]any{"tag": "vl", "protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{
				"address": "127.0.0.1", "port": port("VLESS"), "users": []any{map[string]any{"id": vlUser.vless, "encryption": "none"}}}}}},
		},
		"routing": map[string]any{"rules": []any{
			map[string]any{"inboundTag": []any{"in-hy"}, "outboundTag": "hy"},
			map[string]any{"inboundTag": []any{"in-vl"}, "outboundTag": "vl"},
		}},
	}
	raw, _ := json.Marshal(cfg)
	cmd := exec.Command(bin, "run", "-config", "stdin:", "-format", "json")
	cmd.Stdin = bytes.NewReader(raw)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for _, p := range []int{hySocks, vlSocks} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err == nil {
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("client xray didn't come up: %s", out.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return fmt.Sprintf("127.0.0.1:%d", hySocks), fmt.Sprintf("127.0.0.1:%d", vlSocks), out.String
}

// socksGet does an HTTP GET to target (ip:port) through a SOCKS5 proxy and
// returns the body length.
func socksGet(proxy, target string) (int, error) {
	host, portStr, _ := net.SplitHostPort(target)
	var port int
	fmt.Sscan(portStr, &port)
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	buf := make([]byte, 10)
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[1] != 0 {
		return 0, fmt.Errorf("socks greeting: %v %x", err, buf[:2])
	}
	req := append([]byte{5, 1, 0, 1}, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c, buf); err != nil || buf[1] != 0 {
		return 0, fmt.Errorf("socks connect: %v %x", err, buf)
	}
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return int(n), err
}
