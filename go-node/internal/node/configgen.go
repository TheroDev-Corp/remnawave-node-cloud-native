package node

import "strings"

// TorrentBlockerConfig is what generateApiConfig needs from the plugin state.
type TorrentBlockerConfig struct {
	Enabled         bool
	IncludeRuleTags map[string]bool
	RulePosition    int
}

const (
	apiTag             = "REMNAWAVE_API"
	apiInboundTag      = "REMNAWAVE_API_INBOUND"
	torrentOutboundTag = "RW_TB_OUTBOUND_BLOCK"
)

// GenerateAPIConfig is generate-api-config.ts: take the panel's xray config
// and add stats, the API (Handler/Stats/Routing services on a tunnel
// inbound), forced user stats policy, the API routing rule, and the
// torrent-blocker outbound + webhook rules when that plugin is on.
// apiListen is "@<abstract socket>" in production.
func GenerateAPIConfig(cfg map[string]any, tb TorrentBlockerConfig, apiListen, webhookURL string) map[string]any {
	out := make(map[string]any, len(cfg)+4)
	for k, v := range cfg {
		out[k] = v
	}
	out["stats"] = map[string]any{}
	out["api"] = map[string]any{
		"services": []any{"HandlerService", "StatsService", "RoutingService"},
		"tag":      apiTag,
	}

	var inbounds []any
	inbounds = append(inbounds, apiInbound(apiListen))
	if in, ok := cfg["inbounds"].([]any); ok {
		inbounds = append(inbounds, in...)
	}
	out["inbounds"] = inbounds

	outbounds := []any{}
	if ob, ok := cfg["outbounds"].([]any); ok {
		outbounds = append(outbounds, ob...)
	}
	out["outbounds"] = outbounds

	level0 := map[string]any{}
	if pol, ok := cfg["policy"].(map[string]any); ok {
		if levels, ok := pol["levels"].(map[string]any); ok {
			if l0, ok := levels["0"].(map[string]any); ok {
				for k, v := range l0 {
					level0[k] = v
				}
			}
		}
	}
	level0["statsUserUplink"] = true
	level0["statsUserDownlink"] = true
	level0["statsUserOnline"] = true
	out["policy"] = map[string]any{
		"levels": map[string]any{"0": level0},
		"system": map[string]any{
			"statsInboundDownlink":  true,
			"statsInboundUplink":    true,
			"statsOutboundDownlink": true,
			"statsOutboundUplink":   true,
		},
	}

	routing := map[string]any{}
	if r, ok := cfg["routing"].(map[string]any); ok {
		for k, v := range r {
			routing[k] = v
		}
	}
	rules := []any{map[string]any{"inboundTag": []any{apiInboundTag}, "outboundTag": apiTag}}
	if r, ok := cfg["routing"].(map[string]any); ok {
		if rs, ok := r["rules"].([]any); ok {
			for _, rule := range rs {
				if m, ok := rule.(map[string]any); ok && m["outboundTag"] == apiTag {
					continue
				}
				rules = append(rules, rule)
			}
		}
	}

	if tb.Enabled {
		outbounds = append(outbounds, map[string]any{"tag": torrentOutboundTag, "protocol": "blackhole"})
		out["outbounds"] = outbounds
		tbRule := map[string]any{
			"protocol":    []any{"bittorrent"},
			"outboundTag": torrentOutboundTag,
			"webhook":     map[string]any{"url": webhookURL, "deduplication": 5},
		}
		idx := resolveRuleIndex(tb.RulePosition, len(rules))
		rules = append(rules[:idx], append([]any{tbRule}, rules[idx:]...)...)
		if len(tb.IncludeRuleTags) > 0 {
			for _, rule := range rules {
				m, ok := rule.(map[string]any)
				if !ok {
					continue
				}
				if tag, ok := m["ruleTag"].(string); ok && tag != "" && tb.IncludeRuleTags[tag] {
					m["webhook"] = map[string]any{"url": webhookURL, "deduplication": 5}
				}
			}
		}
	}
	routing["rules"] = rules
	out["routing"] = routing
	return out
}

func apiInbound(listen string) map[string]any {
	if host, port, ok := strings.Cut(listen, ":"); ok && !strings.HasPrefix(listen, "@") {
		// Non-Linux development: a loopback TCP port instead of an abstract socket.
		return map[string]any{"tag": apiInboundTag, "listen": host, "port": port, "protocol": "tunnel"}
	}
	return map[string]any{"tag": apiInboundTag, "listen": listen, "protocol": "tunnel"}
}

// resolveRuleIndex places the torrent rule after the API rule, at the
// requested position among the user rules (1-based), default first.
func resolveRuleIndex(position, rulesLength int) int {
	user := rulesLength - 1
	if position <= 0 {
		return 1
	}
	return 1 + min(position, user)
}

// WebhookURL is buildWebhookUrl(): xray posts to the internal abstract socket.
func WebhookURL(socketPath, token string) string {
	return "@" + socketPath + ":/internal/webhook?token=" + token
}
