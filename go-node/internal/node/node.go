// Package node implements the TS node's Nest modules (xray-core, handler,
// stats, plugins, internal, health) on top of the api router.
package node

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/health"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// Version is the node version reported to the panel. It must be valid
// semver ≥ 2.7.0 (start-node.processor.ts gates on it); by default it is
// the TS fork release this port tracks.
var Version = "3.5.0-HA"

// Replicator forwards state-changing calls to standby pods (milestone 4).
type Replicator interface {
	Replicate(action string, body any)
}

type noReplication struct{}

func (noReplication) Replicate(string, any) {}

type Node struct {
	Cfg       *config.Config
	Log       *slog.Logger
	HA        ha.State
	Proc      *xray.Process
	XAPI      *xray.API
	APIListen string // xray api inbound listen value, "@xtls-api-…" in production
	Net       *NetStats
	State     *State
	Plugins   *Plugins
	Repl      Replicator

	// CoreLink etc. are the custom-core paths (core-loader.service.ts).
	CoreLink, CoreStock, CoreCustom, AssetsDir string

	xrayVersion atomic.Pointer[string]
	online      atomic.Bool
	starting    atomic.Bool
	// lastPluginSync is the last /node/plugin/sync body, handed to a
	// catching-up standby in the peer snapshot.
	lastPluginSync atomic.Value

	// journal holds the user operations applied since the last full xray
	// start, so a catching-up standby gets them too (the TS fork's snapshot
	// carried only the start request, and those users were lost on hand-off).
	// traffic keeps monotonic traffic totals for /metrics (nil when
	// METRICS_PORT is off); metricsMu serializes scrapes.
	traffic   *trafficTracker
	metricsMu sync.Mutex

	journalMu sync.Mutex
	journal   []ha.Action
	startMu   sync.Mutex
}

// New builds a node with its state; call Register to add the routes.
func New(cfg *config.Config, log *slog.Logger, st ha.State, proc *xray.Process, api *xray.API, apiListen string, net *NetStats) *Node {
	n := &Node{
		Cfg: cfg, Log: log, HA: st, Proc: proc, XAPI: api, APIListen: apiListen, Net: net,
		State: NewState(), Repl: noReplication{},
		CoreLink: "/usr/local/bin/rw-core", CoreStock: "/usr/local/bin/xray", CoreCustom: "/usr/local/bin/xray-custom",
		AssetsDir: "/usr/local/share/xray",
	}
	n.Plugins = newPlugins(n)
	if cfg.MetricsPort != 0 {
		n.traffic = newTrafficTracker()
	}
	return n
}

// XrayVersion is the semver of the current core binary, or nil.
func (n *Node) XrayVersion() *string { return n.xrayVersion.Load() }

func (n *Node) RefreshXrayVersion(ctx context.Context) {
	if _, v, err := xray.Version(ctx, n.Cfg.XrayBinary); err == nil && v != "" {
		n.xrayVersion.Store(&v)
	} else if err != nil {
		n.Log.Warn("Failed to read xray core version", "err", err)
	}
}

// Register adds every route of the TS node's Nest controllers.
func (n *Node) Register(rt *api.Router) {
	rt.Handle(http.MethodGet, "/node/health/live", api.AuthNone, func(r *api.Request) {
		r.JSON(http.StatusOK, health.Live(n.HA))
	})
	rt.Handle(http.MethodGet, "/node/health/ready", api.AuthNone, func(r *api.Request) {
		code, body := health.Ready(n.HA, n.Proc)
		r.JSON(code, body)
	})
	n.registerXray(rt)
	n.registerHandler(rt)
	n.registerStats(rt)
	n.registerPlugins(rt)
	n.RegisterInternal(rt)
}

// RegisterInternal adds InternalController's routes (no /node prefix,
// ?token= guard), served on the mTLS port and on the internal socket.
func (n *Node) RegisterInternal(rt *api.Router) {
	rt.Handle(http.MethodGet, "/internal/get-config", api.AuthToken, func(r *api.Request) {
		cfg := n.State.XrayConfig()
		if cfg == nil {
			cfg = map[string]any{}
		}
		r.JSON(http.StatusOK, cfg)
	})
	rt.Handle(http.MethodPost, "/internal/webhook", api.AuthToken, func(r *api.Request) {
		body := append(json.RawMessage(nil), r.Body...)
		go n.Plugins.HandleWebhook(body)
		r.Empty(http.StatusOK)
	})
}

// validated decodes a request body after checking it against the
// command's zod-derived schema; it answers 400 itself on failure.
func validated(r *api.Request, command string, v any) bool {
	if issues := validateRequest(command, r.Body); len(issues) > 0 {
		r.ValidationError(issues)
		return false
	}
	if err := r.Decode(v); err != nil {
		r.ValidationError([]map[string]any{{"code": "custom", "path": []any{}, "message": err.Error()}})
		return false
	}
	return true
}
