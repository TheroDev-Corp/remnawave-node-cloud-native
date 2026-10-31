// Package node wires the routes of the TS node's Nest modules onto the api
// router. Milestone 1: health and the internal (token) routes; the 20 panel
// endpoints follow in milestone 2.
package node

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/health"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// Version is the node version reported to the panel. It must be valid
// semver ≥ 2.7.0 (start-node.processor.ts gates on it); by default it is
// the TS fork release this port tracks.
var Version = "3.4.5-HA"

type Node struct {
	Cfg   *config.Config
	Log   *slog.Logger
	HA    ha.State
	Proc  *xray.Process
	XAPI  *xray.API
	XVer  string // xray semver, e.g. 26.10.21
	mu    sync.Mutex
	xcfg  map[string]any // last generated xray config (/internal/get-config)
	hooks []func(json.RawMessage)
}

// Config returns the current generated xray config.
func (n *Node) Config() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.xcfg
}

// OnWebhook registers a consumer for xray routing webhooks
// (XrayWebhookEvent in the TS node; the torrent blocker listens to it).
func (n *Node) OnWebhook(f func(json.RawMessage)) { n.hooks = append(n.hooks, f) }

// Register adds the milestone-1 routes.
func (n *Node) Register(rt *api.Router) {
	rt.Handle(http.MethodGet, "/node/health/live", api.AuthNone, func(r *api.Request) {
		r.JSON(http.StatusOK, health.Live(n.HA))
	})
	rt.Handle(http.MethodGet, "/node/health/ready", api.AuthNone, func(r *api.Request) {
		code, body := health.Ready(n.HA, n.Proc)
		r.JSON(code, body)
	})
	n.RegisterInternal(rt)
}

// RegisterInternal adds InternalController's routes (no /node prefix,
// ?token= guard). They are served on the mTLS port and on the internal
// abstract socket xray posts its webhooks to.
func (n *Node) RegisterInternal(rt *api.Router) {
	rt.Handle(http.MethodGet, "/internal/get-config", api.AuthToken, func(r *api.Request) {
		cfg := n.Config()
		if cfg == nil {
			cfg = map[string]any{}
		}
		r.JSON(http.StatusOK, cfg)
	})
	rt.Handle(http.MethodPost, "/internal/webhook", api.AuthToken, func(r *api.Request) {
		for _, h := range n.hooks {
			h(r.Body)
		}
		r.Empty(http.StatusOK)
	})
}
