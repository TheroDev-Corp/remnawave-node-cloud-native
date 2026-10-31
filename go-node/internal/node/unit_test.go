package node

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
)

func TestGenerateAPIConfig(t *testing.T) {
	in := map[string]any{
		"inbounds":  []any{map[string]any{"tag": "A"}},
		"outbounds": []any{map[string]any{"tag": "DIRECT"}},
		"policy":    map[string]any{"levels": map[string]any{"0": map[string]any{"handshake": 4, "statsUserOnline": false}}},
		"routing": map[string]any{"domainStrategy": "IPIfNonMatch", "rules": []any{
			map[string]any{"outboundTag": apiTag}, // stale API rule from the panel: dropped
			map[string]any{"ruleTag": "r1", "outboundTag": "DIRECT"},
			map[string]any{"ruleTag": "r2", "outboundTag": "DIRECT"},
		}},
	}
	out := GenerateAPIConfig(in, TorrentBlockerConfig{}, "@xtls-api-x", "")
	ib := out["inbounds"].([]any)
	if len(ib) != 2 || ib[0].(map[string]any)["listen"] != "@xtls-api-x" || ib[0].(map[string]any)["protocol"] != "tunnel" {
		t.Fatalf("api inbound must come first: %v", ib)
	}
	rules := out["routing"].(map[string]any)["rules"].([]any)
	if len(rules) != 3 || rules[0].(map[string]any)["outboundTag"] != apiTag {
		t.Fatalf("rules = %v", rules)
	}
	l0 := out["policy"].(map[string]any)["levels"].(map[string]any)["0"].(map[string]any)
	if l0["handshake"] != 4 || l0["statsUserOnline"] != true || l0["statsUserUplink"] != true {
		t.Errorf("policy level 0 keeps panel values and forces stats: %v", l0)
	}
	if out["routing"].(map[string]any)["domainStrategy"] != "IPIfNonMatch" {
		t.Error("routing keys other than rules are preserved")
	}

	// Torrent blocker at position 2 among the user rules, webhook on included tags.
	out = GenerateAPIConfig(in, TorrentBlockerConfig{Enabled: true, RulePosition: 2, IncludeRuleTags: map[string]bool{"r2": true}}, "@s", "@int:/internal/webhook?token=t")
	rules = out["routing"].(map[string]any)["rules"].([]any)
	if rules[3].(map[string]any)["outboundTag"] != torrentOutboundTag {
		t.Errorf("torrent rule position: %v", rules)
	}
	if rules[2].(map[string]any)["webhook"] == nil || rules[1].(map[string]any)["webhook"] != nil {
		t.Errorf("webhook only on included rule tags: %v", rules)
	}
	obs := out["outbounds"].([]any)
	if obs[len(obs)-1].(map[string]any)["tag"] != torrentOutboundTag {
		t.Error("blackhole outbound appended")
	}
	for pos, want := range map[int]int{0: 1, -5: 1, 1: 2, 99: 3} {
		if got := resolveRuleIndex(pos, 3); got != want {
			t.Errorf("resolveRuleIndex(%d) = %d, want %d", pos, got, want)
		}
	}
}

func TestExtractIP(t *testing.T) {
	s := func(v string) *string { return &v }
	for src, want := range map[string]string{
		"tcp:1.2.3.4:5555": "1.2.3.4", "udp:[2001:db8::1]:443": "2001:db8::1", "1.2.3.4": "1.2.3.4",
		"[::1]:80": "::1", "tcp:example.com:80": "", "": "",
	} {
		_, got, ok := extractIP(s(src))
		if (want == "") == ok || got != want {
			t.Errorf("extractIP(%q) = %q, %v", src, got, ok)
		}
	}
}

func TestIgnoreMatching(t *testing.T) {
	m := newIPMatcher([]string{"10.0.0.0/8", "1.1.1.1", "2001:db8::/32"})
	for ip, want := range map[string]bool{"10.200.0.1": true, "1.1.1.1": true, "1.1.1.2": false, "2001:db8::5": true, "::ffff:10.0.0.1": true} {
		if got := m.has(netip.MustParseAddr(ip)); got != want {
			t.Errorf("has(%s) = %v", ip, got)
		}
	}
	// DEFAULT_IGNORED_IPS' "0.0.0.0/0" must not turn into "ignore everyone".
	if defaultIgnored.has(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("default ignore list matched a public IP")
	}
	if !defaultIgnored.has(netip.MustParseAddr("127.0.0.53")) || !defaultIgnored.has(netip.MustParseAddr("::1")) {
		t.Error("loopback is ignored by default")
	}
}

func TestDefinitionFromConfig(t *testing.T) {
	cfg := map[string]any{"inbounds": []any{
		map[string]any{"tag": "V", "protocol": "vless", "settings": map[string]any{"clients": []any{map[string]any{"id": "u-1", "email": "7", "flow": "xtls-rprx-vision"}}}},
		map[string]any{"tag": "S", "protocol": "shadowsocks", "settings": map[string]any{"clients": []any{map[string]any{"password": "p", "method": "chacha20-ietf-poly1305", "email": "7"}}}},
		map[string]any{"tag": "S22", "protocol": "shadowsocks", "settings": map[string]any{"method": "2022-blake3-aes-128-gcm", "clients": []any{map[string]any{"password": "k==", "email": "7"}}}},
		map[string]any{"tag": "H", "protocol": "hysteria", "settings": map[string]any{"clients": []any{map[string]any{"auth": "a", "email": "7"}}}},
		map[string]any{"tag": "X", "protocol": "vless", "settings": map[string]any{"clients": []any{map[string]any{"id": "other", "email": "8"}}}},
	}}
	d, ok := definitionFromConfig(cfg, "7")
	if !ok || len(d.Ops) != 4 || d.Hash != "u-1" {
		t.Fatalf("%+v", d)
	}
	if d.Ops[1].Protocol != "shadowsocks" || d.Ops[1].Cipher != 7 || d.Ops[2].Protocol != "shadowsocks22" || d.Ops[2].Password != "k==" || d.Ops[3].Password != "a" {
		t.Errorf("ops = %+v", d.Ops)
	}
}

func TestNeedRestart(t *testing.T) {
	s := NewState()
	var h Hashes
	h.EmptyConfig = "e"
	if r, _ := s.NeedRestart(h); !r {
		t.Error("no previous config → restart")
	}
	cfg := map[string]any{"inbounds": []any{map[string]any{"tag": "A", "settings": map[string]any{"clients": []any{map[string]any{"id": "x"}}}}}}
	h.Inbounds = append(h.Inbounds, struct {
		UsersCount int    `json:"usersCount"`
		Hash       string `json:"hash"`
		Tag        string `json:"tag"`
	}{1, "", "A"})
	s.ExtractUsers(h, cfg)
	cur, _ := s.InboundHash("A")
	h.Inbounds[0].Hash = cur
	if r, why := s.NeedRestart(h); r {
		t.Errorf("same hashes → no restart (%s)", why)
	}
	s.RemoveUserFromInbound("A", "x") // last user gone: the inbound disappears
	if r, _ := s.NeedRestart(h); !r {
		t.Error("emptied inbound → restart on next push, like the TS node")
	}
}

type fakeASN map[uint32][2][]string

func (f fakeASN) Prefixes(asn uint32) ([]string, []string, bool) {
	e, ok := f[asn]
	return e[0], e[1], ok
}

func TestASListIgnore(t *testing.T) {
	n := New(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), ha.Standalone{}, nil, nil, "", nil)
	n.Plugins.ASN = fakeASN{13335: {{"1.1.1.0/24", "104.16.0.0/13"}, {"2606:4700::/32"}}}
	var cfg pluginConfig
	_ = json.Unmarshal([]byte(`{"sharedLists":[{"type":"asList","name":"ext:cf","items":[13335,64496]}]}`), &cfg)
	shared := n.Plugins.resolveSharedLists(cfg)
	if got := shared["ext:cf"]; len(got) != 3 {
		t.Fatalf("asList ext:cf = %v (unknown ASNs are skipped)", got)
	}
	m := newIPMatcher(resolveIPList([]string{"ext:cf"}, shared, func(string, ...any) {}))
	for ip, want := range map[string]bool{"1.1.1.7": true, "104.20.1.1": true, "2606:4700::1": true, "8.8.8.8": false} {
		if got := m.has(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s ignored=%v, want %v", ip, got, want)
		}
	}
}
