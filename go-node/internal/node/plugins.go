package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/contract"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// ASNResolver resolves an AS number to its prefixes (asn-lmdb module).
// Nil means no ASN data: asList entries resolve to nothing, as when the TS
// node's lookup misses.
type ASNResolver interface {
	Prefixes(asn uint32) (v4, v6 []string, ok bool)
}

// pluginConfig is the subset of @remnawave/node-plugins the node acts on.
// The full config is validated against the embedded NodePluginSchema first.
type pluginConfig struct {
	SharedLists []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Items json.RawMessage `json:"items"`
	} `json:"sharedLists"`
	TorrentBlocker *struct {
		Enabled         bool     `json:"enabled"`
		BlockDuration   int64    `json:"blockDuration"`
		RulePlacement   *int     `json:"rulePlacement"`
		WebhookURL      *string  `json:"webhookUrl"`
		IncludeRuleTags []string `json:"includeRuleTags"`
		IgnoreLists     struct {
			IP     []string      `json:"ip"`
			UserID []json.Number `json:"userId"`
		} `json:"ignoreLists"`
	} `json:"torrentBlocker"`
	PreStart *struct {
		Enabled        bool `json:"enabled"`
		CleanupSockets *struct {
			Enabled bool     `json:"enabled"`
			Files   []string `json:"files"`
		} `json:"cleanupSockets"`
	} `json:"preStart"`
	PostStart *struct {
		Enabled bool `json:"enabled"`
		Webhook *struct {
			Enabled bool   `json:"enabled"`
			URL     string `json:"url"`
		} `json:"webhook"`
	} `json:"postStart"`
}

// xrayWebhook is XrayWebhookSchema (libs/contract/models/xray-webhook.schema.ts).
type xrayWebhook struct {
	Email          *string  `json:"email"`
	Level          *float64 `json:"level"`
	Protocol       *string  `json:"protocol"`
	Network        string   `json:"network"`
	Source         *string  `json:"source"`
	Destination    string   `json:"destination"`
	RouteTarget    *string  `json:"routeTarget"`
	OriginalTarget *string  `json:"originalTarget"`
	InboundTag     *string  `json:"inboundTag"`
	InboundName    *string  `json:"inboundName"`
	InboundLocal   *string  `json:"inboundLocal"`
	OutboundTag    *string  `json:"outboundTag"`
	Ts             float64  `json:"ts"`
}

type torrentReport struct {
	ActionReport struct {
		Blocked       bool   `json:"blocked"`
		IP            string `json:"ip"`
		BlockDuration int64  `json:"blockDuration"`
		WillUnblockAt string `json:"willUnblockAt"`
		UserID        string `json:"userId"`
		ProcessedAt   string `json:"processedAt"`
	} `json:"actionReport"`
	XrayReport xrayWebhook `json:"xrayReport"`
}

// ipMatcher matches exact IPs and CIDR prefixes. Design fix vs the TS fork,
// which used an exact Set lookup so CIDRs from sharedLists (ipList ranges,
// asList prefixes) never matched any client IP.
type ipMatcher struct {
	exact    map[netip.Addr]bool
	prefixes []netip.Prefix
}

func newIPMatcher(items []string) ipMatcher {
	m := ipMatcher{exact: map[netip.Addr]bool{}}
	for _, it := range items {
		if p, err := netip.ParsePrefix(it); err == nil {
			m.prefixes = append(m.prefixes, p.Masked())
		} else if a, err := netip.ParseAddr(it); err == nil {
			m.exact[a.Unmap()] = true
		}
	}
	return m
}

func (m ipMatcher) has(a netip.Addr) bool {
	a = a.Unmap()
	if m.exact[a] {
		return true
	}
	for _, p := range m.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// defaultIgnored is DEFAULT_IGNORED_IPS. Its "0.0.0.0/0" entry is kept as
// a literal (as the TS exact-match lookup treats it): read as a CIDR it
// would exempt every IPv4 client.
var defaultIgnored = newIPMatcher([]string{"::", "::1", "0.0.0.0", "127.0.0.0/8", "255.255.255.255"})

type Plugins struct {
	n  *Node
	mu sync.Mutex

	configHash string
	details    *struct{ UUID, Name string }

	tbEnabled      bool
	tbDuration     int64
	tbIgnoredIPs   ipMatcher
	tbIgnoredUsers map[string]bool
	tbIncludeTags  map[string]bool
	tbWebhook      string
	tbPosition     int
	reports        []torrentReport

	preEnabled     bool
	preCleanup     bool
	preCleanupList []string

	postEnabled bool
	postWebhook string

	suspended map[string]*time.Timer
	ASN       ASNResolver
}

func newPlugins(n *Node) *Plugins {
	return &Plugins{n: n, tbIgnoredUsers: map[string]bool{}, tbIncludeTags: map[string]bool{}, suspended: map[string]*time.Timer{}}
}

func (p *Plugins) resetLocked() {
	p.tbEnabled, p.tbDuration, p.tbPosition = false, 0, 0
	p.tbIgnoredIPs = ipMatcher{}
	p.tbIgnoredUsers = map[string]bool{}
	p.tbIncludeTags = map[string]bool{}
	p.preEnabled, p.preCleanup, p.preCleanupList = false, false, nil
	p.postEnabled, p.postWebhook = false, ""
	// Like TorrentBlockerState.reset(), the webhook URL and pending reports survive.
}

// Reset is ResetPluginsCommand (resetState + cleanUpActivePlugin).
func (p *Plugins) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetLocked()
	p.configHash, p.details = "", nil
}

func (p *Plugins) TorrentBlockerConfig() TorrentBlockerConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	tags := map[string]bool{}
	for t := range p.tbIncludeTags {
		tags[t] = true
	}
	return TorrentBlockerConfig{Enabled: p.tbEnabled, IncludeRuleTags: tags, RulePosition: p.tbPosition}
}

func (p *Plugins) ReportsCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reports)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Sync is PluginService.sync. It returns `accepted`.
func (p *Plugins) Sync(ctx context.Context, plugin *struct {
	Config json.RawMessage `json:"config"`
	UUID   string          `json:"uuid"`
	Name   string          `json:"name"`
}) bool {
	log := p.n.Log
	if plugin == nil {
		p.mu.Lock()
		active := p.details != nil
		p.mu.Unlock()
		if !active {
			return false
		}
		log.Info("[PLUGIN] Received empty plugins, but there is an active plugin. Cleaning up...")
		p.Reset()
		p.n.StopXray(false, true)
		return true
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, plugin.Config); err != nil {
		compact.Write(plugin.Config)
	}
	sum := sha256.Sum256(compact.Bytes())
	hash := hex.EncodeToString(sum[:])
	p.mu.Lock()
	unchanged := p.configHash == hash
	p.mu.Unlock()
	if unchanged {
		log.Debug("[PLUGIN] Config unchanged. Skipping sync.")
		return true
	}

	if issues := contract.ValidatePlugin(plugin.Config); len(issues) > 0 {
		log.Error(fmt.Sprintf("[PLUGIN] Invalid config: %v", issues))
		p.Reset()
		p.n.StopXray(false, true)
		return false
	}
	var cfg pluginConfig
	dec := json.NewDecoder(bytes.NewReader(plugin.Config))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		log.Error("[PLUGIN] Invalid config: " + err.Error())
		p.Reset()
		p.n.StopXray(false, true)
		return false
	}

	p.mu.Lock()
	wasEnabled := p.tbEnabled
	prevTags := sortedKeys(p.tbIncludeTags)
	prevPos := p.tbPosition
	shared := p.resolveSharedLists(cfg)

	p.resetLocked()
	p.configHash, p.details = "", nil

	if tb := cfg.TorrentBlocker; tb != nil && tb.Enabled {
		ips := resolveIPList(tb.IgnoreLists.IP, shared, log.Warn)
		p.tbIgnoredIPs = newIPMatcher(ips)
		for _, u := range tb.IgnoreLists.UserID {
			p.tbIgnoredUsers[u.String()] = true
		}
		p.tbEnabled, p.tbDuration = true, tb.BlockDuration
		if tb.RulePlacement != nil {
			p.tbPosition = *tb.RulePlacement
		}
		p.tbWebhook = ""
		if tb.WebhookURL != nil {
			p.tbWebhook = *tb.WebhookURL
		}
		if tb.IncludeRuleTags != nil {
			for _, t := range tb.IncludeRuleTags {
				p.tbIncludeTags[t] = true
			}
		}
		log.Info(fmt.Sprintf("[PLUGIN] Torrent-Blocker: blockDuration=%ds, %d ignored IPs, %d ignored users", tb.BlockDuration, len(ips), len(p.tbIgnoredUsers)))
	}
	if ps := cfg.PreStart; ps != nil && ps.Enabled {
		p.preEnabled = true
		if ps.CleanupSockets != nil {
			p.preCleanup, p.preCleanupList = ps.CleanupSockets.Enabled, slices.Clone(ps.CleanupSockets.Files)
		}
	}
	if ps := cfg.PostStart; ps != nil && ps.Enabled {
		p.postEnabled = true
		if ps.Webhook != nil && ps.Webhook.Enabled {
			p.postWebhook = ps.Webhook.URL
		}
	}
	p.configHash = hash
	p.details = &struct{ UUID, Name string }{plugin.UUID, plugin.Name}
	nowEnabled := p.tbEnabled
	nowTags := sortedKeys(p.tbIncludeTags)
	nowPos := p.tbPosition
	includeTagsSent := cfg.TorrentBlocker != nil && cfg.TorrentBlocker.IncludeRuleTags != nil
	p.mu.Unlock()

	log.Info("[PLUGIN] Plugins changed...")
	if wasEnabled && !nowEnabled && !includeTagsSent {
		if err := p.n.XAPI.RemoveOutbound(ctx, torrentOutboundTag); err != nil {
			log.Error("remove torrent-blocker outbound: " + err.Error())
		}
		return true
	}
	still := wasEnabled && nowEnabled
	if wasEnabled != nowEnabled || (still && !slices.Equal(prevTags, nowTags)) || (still && prevPos != nowPos) {
		p.n.StopXray(false, true)
	}
	return true
}

func (p *Plugins) resolveSharedLists(cfg pluginConfig) map[string][]string {
	out := map[string][]string{}
	for _, l := range cfg.SharedLists {
		switch l.Type {
		case "ipList":
			var items []string
			_ = json.Unmarshal(l.Items, &items)
			out[l.Name] = items
		case "asList":
			var asns []uint32
			_ = json.Unmarshal(l.Items, &asns)
			var prefixes []string
			for _, asn := range asns {
				if p.ASN == nil {
					p.n.Log.Warn(fmt.Sprintf("[PLUGIN] ASN %d not found", asn))
					continue
				}
				v4, v6, ok := p.ASN.Prefixes(asn)
				if !ok {
					p.n.Log.Warn(fmt.Sprintf("[PLUGIN] ASN %d not found", asn))
					continue
				}
				prefixes = append(append(prefixes, v4...), v6...)
			}
			out[l.Name] = prefixes
		}
	}
	return out
}

func resolveIPList(ips []string, shared map[string][]string, warn func(string, ...any)) []string {
	var out []string
	for _, ip := range ips {
		if strings.HasPrefix(ip, "ext:") {
			list, ok := shared[ip]
			if !ok {
				warn(fmt.Sprintf("[PLUGIN] Shared IP list %q not found", ip))
				continue
			}
			out = append(out, list...)
			continue
		}
		out = append(out, ip)
	}
	return out
}

// Collect is collectReports(): flush and return the pending reports.
func (p *Plugins) Collect() []torrentReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.reports
	p.reports = nil
	if r == nil {
		r = []torrentReport{}
	}
	return r
}

var sourceRe = regexp.MustCompile(`^(?:(?:tcp|udp):)?(?:\[(.+?)\]|(.+?))(?::(\d+))?$`)

func extractIP(source *string) (netip.Addr, string, bool) {
	if source == nil || *source == "" {
		return netip.Addr{}, "", false
	}
	cand := *source
	if m := sourceRe.FindStringSubmatch(cand); m != nil {
		cand = m[1]
		if cand == "" {
			cand = m[2]
		}
	}
	a, err := netip.ParseAddr(cand)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, "", false
	}
	return a, cand, true
}

// HandleWebhook is XrayWebhookHandler: on a torrent detection, suspend the
// user (unless ignored), record a report and forward it to the webhook.
func (p *Plugins) HandleWebhook(body json.RawMessage) {
	p.mu.Lock()
	enabled, duration, hook := p.tbEnabled, p.tbDuration, p.tbWebhook
	p.mu.Unlock()
	if !enabled {
		return
	}
	var w xrayWebhook
	if err := json.Unmarshal(body, &w); err != nil || !webhookShapeOK(body) {
		p.n.Log.Error("Invalid webhook")
		return
	}
	addr, ip, ok := extractIP(w.Source)
	if !ok || w.Email == nil || *w.Email == "" {
		return
	}
	p.mu.Lock()
	ignored := p.tbIgnoredIPs.has(addr) || defaultIgnored.has(addr) || p.tbIgnoredUsers[*w.Email]
	p.mu.Unlock()
	if ignored {
		return
	}
	p.n.Log.Warn(fmt.Sprintf("[TORRENT-BLOCKER] Detected torrent (client: %s, user: %s). Suspending user for %ds...", ip, *w.Email, duration))
	blocked := p.Suspend(context.Background(), *w.Email, duration, true)
	now := time.Now().UTC()
	var r torrentReport
	r.ActionReport.Blocked, r.ActionReport.IP, r.ActionReport.BlockDuration = blocked, ip, duration
	r.ActionReport.WillUnblockAt = now.Add(time.Duration(duration) * time.Second).Format("2006-01-02T15:04:05.000Z")
	r.ActionReport.UserID, r.ActionReport.ProcessedAt = *w.Email, now.Format("2006-01-02T15:04:05.000Z")
	r.XrayReport = w
	p.mu.Lock()
	p.reports = append(p.reports, r)
	p.mu.Unlock()
	if hook != "" {
		go sendWebhook(hook, r, nil)
	}
}

// webhookShapeOK enforces XrayWebhookSchema's required non-null fields.
func webhookShapeOK(body json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	for _, k := range []string{"email", "level", "protocol", "network", "source", "destination", "routeTarget", "originalTarget", "inboundTag", "inboundName", "inboundLocal", "outboundTag", "ts"} {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	for _, k := range []string{"network", "destination", "ts"} {
		if string(m[k]) == "null" {
			return false
		}
	}
	return true
}

// Suspend is UserSuspensionService.suspendUser: remove the user from every
// inbound for `seconds`, then restore. With no stored definition (a user
// that came with the initial xray config), the definition is derived from
// the live config so the user can be restored — the TS fork could not.
func (p *Plugins) Suspend(ctx context.Context, userID string, seconds int64, replicate bool) bool {
	p.mu.Lock()
	if t, ok := p.suspended[userID]; ok {
		t.Stop()
	}
	p.mu.Unlock()

	def, ok := p.n.State.Definition(userID)
	if !ok {
		if d, found := definitionFromConfig(p.n.State.XrayConfig(), userID); found {
			p.n.State.StoreDefinition(userID, d)
		}
	}
	_ = def
	for _, tag := range p.n.State.Tags() {
		p.n.XAPI.RemoveUser(ctx, tag, userID)
	}
	t := time.AfterFunc(time.Duration(seconds)*time.Second, func() { p.Restore(context.Background(), userID, true) })
	p.mu.Lock()
	p.suspended[userID] = t
	p.mu.Unlock()
	if replicate {
		p.n.Repl.Replicate("suspendUser", map[string]any{"userId": userID, "durationSeconds": seconds, "unbanAt": time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()})
	}
	return true
}

// Restore is restoreUser: re-apply the user's stored operations.
func (p *Plugins) Restore(ctx context.Context, userID string, replicate bool) bool {
	p.mu.Lock()
	if t, ok := p.suspended[userID]; ok {
		t.Stop()
		delete(p.suspended, userID)
	}
	p.mu.Unlock()
	def, ok := p.n.State.Definition(userID)
	if !ok {
		p.n.Log.Warn(fmt.Sprintf("[SUSPENSION] No user definition found to restore user %q. User might need panel sync.", userID))
		return false
	}
	p.n.Log.Info(fmt.Sprintf("[SUSPENSION] Restoring user %q back into Xray inbounds...", userID))
	for _, op := range def.Ops {
		p.n.XAPI.AddUser(ctx, op)
	}
	if replicate {
		p.n.Repl.Replicate("restoreUser", map[string]any{"userId": userID})
	}
	return true
}

var ssCiphers = map[string]int32{
	"aes-128-gcm": 5, "aes-256-gcm": 6,
	"chacha20-poly1305": 7, "chacha20-ietf-poly1305": 7,
	"xchacha20-poly1305": 8, "xchacha20-ietf-poly1305": 8,
}

// definitionFromConfig rebuilds the add-user operations for a user from the
// inbounds of the generated xray config.
func definitionFromConfig(cfg map[string]any, userID string) (UserDef, bool) {
	inbounds, _ := cfg["inbounds"].([]any)
	var d UserDef
	for _, raw := range inbounds {
		in, _ := raw.(map[string]any)
		tag, _ := in["tag"].(string)
		proto, _ := in["protocol"].(string)
		settings, _ := in["settings"].(map[string]any)
		clients, _ := settings["clients"].([]any)
		for _, c := range clients {
			cm, _ := c.(map[string]any)
			if email, _ := cm["email"].(string); email != userID {
				continue
			}
			str := func(k string) string { s, _ := cm[k].(string); return s }
			u := xray.User{Tag: tag, Email: userID}
			switch proto {
			case "vless":
				u.Protocol, u.UUID, u.Flow = "vless", str("id"), str("flow")
				d.Hash = str("id")
			case "trojan":
				u.Protocol, u.Password = "trojan", str("password")
			case "shadowsocks":
				method, _ := settings["method"].(string)
				if m := str("method"); m != "" {
					method = m
				}
				if strings.HasPrefix(method, "2022-") {
					u.Protocol, u.Password = "shadowsocks22", str("password")
				} else {
					u.Protocol, u.Password, u.Cipher = "shadowsocks", str("password"), ssCiphers[strings.ToLower(method)]
				}
			case "hysteria":
				u.Protocol, u.Password = "hysteria", str("auth")
			default:
				continue
			}
			d.Ops = append(d.Ops, u)
		}
	}
	return d, len(d.Ops) > 0
}

// RunPreStart is PreStartService.run: remove stale unix sockets matching
// the configured paths/globs (at most 256 matches per pattern).
func (p *Plugins) RunPreStart() {
	p.mu.Lock()
	enabled, cleanup, files := p.preEnabled, p.preCleanup, slices.Clone(p.preCleanupList)
	p.mu.Unlock()
	if !enabled || !cleanup || len(files) == 0 {
		return
	}
	removed := 0
	for _, pattern := range files {
		paths := []string{pattern}
		if strings.ContainsAny(pattern, "*?[]") {
			m, err := filepath.Glob(pattern)
			if err != nil {
				p.n.Log.Warn(fmt.Sprintf("[PRE-START] Cleanup Sockets: failed to expand pattern %q: %v", pattern, err))
			}
			if len(m) > 256 {
				p.n.Log.Warn(fmt.Sprintf("[PRE-START] Cleanup Sockets: pattern %q matched more than 256 entries, the rest is skipped.", pattern))
				m = m[:256]
			}
			paths = m
		}
		for _, path := range paths {
			st, err := os.Lstat(path)
			if err != nil || st.Mode()&fs.ModeSocket == 0 {
				continue
			}
			if os.Remove(path) == nil {
				removed++
			}
		}
	}
	p.n.Log.Info(fmt.Sprintf("[PRE-START] Cleanup Sockets: %d stale socket(s) removed.", removed))
}

// RunPostStart is PostStartService.run: the service.core_started webhook.
func (p *Plugins) RunPostStart(metadata json.RawMessage) {
	p.mu.Lock()
	enabled, url := p.postEnabled, p.postWebhook
	p.mu.Unlock()
	if !enabled || url == "" {
		return
	}
	var md any = map[string]any{}
	if len(metadata) > 0 && string(metadata) != "null" {
		md = metadata
	}
	go sendWebhook(url, map[string]any{
		"scope": "service", "event": "service.core_started",
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "metadata": md,
	}, func(err error) { p.n.Log.Warn("[POST-START] Webhook failed: " + err.Error()) })
}

// sendWebhook is send-webhook.ts: fire-and-forget POST, 5 s timeout.
func sendWebhook(url string, body any, onErr func(error)) {
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		if onErr != nil {
			onErr(err)
		}
		return
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if onErr != nil {
			onErr(err)
		}
		return
	}
	resp.Body.Close()
	if (resp.StatusCode < 200 || resp.StatusCode > 299) && onErr != nil {
		onErr(fmt.Errorf("HTTP %s", strconv.Itoa(resp.StatusCode)))
	}
}

func (n *Node) registerPlugins(rt *api.Router) {
	rt.Handle(http.MethodPost, "/node/plugin/sync", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Plugin *struct {
				Config json.RawMessage `json:"config"`
				UUID   string          `json:"uuid"`
				Name   string          `json:"name"`
			} `json:"plugin"`
		}
		if !validated(r, "SyncCommand", &req) {
			return
		}
		accepted := n.Plugins.Sync(r.R.Context(), req.Plugin)
		// Fork fix: plugin sync is replicated so standbys get the same
		// torrent-blocker rules (the TS fork never sent it to peers).
		n.Repl.Replicate("pluginSync", json.RawMessage(r.Body))
		r.OK(map[string]any{"accepted": accepted})
	})
	rt.Handle(http.MethodPost, "/node/plugin/torrent-blocker/collect", api.AuthJWT, func(r *api.Request) {
		r.OK(map[string]any{"reports": n.Plugins.Collect()})
	})
	// nftables was removed from the fork: these stay successful no-ops.
	for _, path := range []string{"/node/plugin/nftables/block-ips", "/node/plugin/nftables/unblock-ips", "/node/plugin/nftables/recreate-tables"} {
		rt.Handle(http.MethodPost, path, api.AuthJWT, func(r *api.Request) {
			r.OK(map[string]any{"accepted": true})
		})
	}
}
