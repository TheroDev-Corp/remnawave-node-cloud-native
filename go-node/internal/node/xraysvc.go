package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/contract"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

var validateRequest = contract.ValidateRequest

// StartRequest is StartXrayCommand.Request.
type StartRequest struct {
	Internals struct {
		ForceRestart bool            `json:"forceRestart"`
		Hashes       Hashes          `json:"hashes"`
		Metadata     json.RawMessage `json:"metadata,omitempty"`
		Integrations json.RawMessage `json:"integrations,omitempty"`
	} `json:"internals"`
	XrayConfig map[string]any `json:"xrayConfig"`
}

type startSystem struct {
	Info      SystemInfo     `json:"info"`
	Stats     SystemStats    `json:"stats"`
	Interface *InterfaceRate `json:"interface"`
}

// StartResponse is StartXrayResponseModel.
type StartResponse struct {
	IsStarted       bool    `json:"isStarted"`
	Version         *string `json:"version"`
	Error           *string `json:"error"`
	NodeInformation struct {
		Version *string `json:"version"`
	} `json:"nodeInformation"`
	System startSystem `json:"system"`
}

func strp(s string) *string { return &s }

func (n *Node) startResponse(started bool, version *string, errMsg *string, sys startSystem) StartResponse {
	r := StartResponse{IsStarted: started, Version: version, Error: errMsg, System: sys}
	r.NodeInformation.Version = strp(Version)
	return r
}

// StartXray is XrayService.startXray.
func (n *Node) StartXray(ctx context.Context, req *StartRequest, raw json.RawMessage, ip string) StartResponse {
	t0 := time.Now()
	// startXray computes system stats with interface: null and puts the
	// interface rates next to them (system.interface), which is what the
	// panel receives.
	sys := startSystem{Info: GetSystemInfo(), Stats: GetSystemStats(), Interface: n.Net.Default()}

	if !n.starting.CompareAndSwap(false, true) {
		n.Log.Warn("Request already in progress")
		return n.startResponse(false, n.XrayVersion(), strp("Request already in progress"), sys)
	}
	defer func() {
		n.starting.Store(false)
		n.Log.Info(fmt.Sprintf("Attempt to start XTLS took: %s (IP: %s)", time.Since(t0).Round(time.Millisecond), ip))
	}()

	if n.online.Load() && !n.Cfg.DisableHashedSetCheck && !req.Internals.ForceRestart {
		restart := true
		if _, err := n.XAPI.SysStats(ctx); err == nil {
			var why string
			restart, why = n.State.NeedRestart(req.Internals.Hashes)
			if restart {
				n.Log.Warn(why)
			} else {
				n.Log.Info("Xray Core configuration is up-to-date - no restart required")
			}
		} else {
			n.online.Store(false)
			n.Log.Warn("Xray Core health check failed, restarting...")
		}
		if !restart {
			n.State.SetLastStart(raw)
			return n.startResponse(true, n.XrayVersion(), nil, sys)
		}
	}
	if req.Internals.ForceRestart {
		n.Log.Warn("Force restart requested")
	}

	full := GenerateAPIConfig(req.XrayConfig, n.Plugins.TorrentBlockerConfig(), n.APIListen,
		WebhookURL(n.Cfg.InternalSocketPath, n.Cfg.InternalRestToken))
	n.State.SetLastStart(raw)
	n.State.ExtractUsers(req.Internals.Hashes, full)

	n.prepareCore(ctx, full["geodata"])
	n.prepareAssets(ctx, full["geodata"])

	if err := n.restartXrayProcess(full); err != nil {
		n.Log.Error("Failed to (re)start Xray process: " + err.Error())
		return n.startResponse(false, nil, strp(err.Error()), sys)
	}

	if err := xray.WaitReady(ctx, n.XAPI, n.Proc); err != nil {
		n.online.Store(false)
		msg := "Xray Core did not become ready in time"
		if errors.Is(err, xray.ErrProcessDown) {
			msg = strings.TrimPrefix(err.Error(), xray.ErrProcessDown.Error()+": ")
		}
		tail := n.Proc.Tail(5)
		if len(tail) > 0 {
			reason := strings.TrimSpace(tail[len(tail)-1])
			if len(reason) > 500 {
				reason = reason[:500]
			}
			msg += " · " + reason
		}
		n.Log.Error(fmt.Sprintf("Xray Core v%s failed to start.", deref(n.XrayVersion())))
		return n.startResponse(false, n.XrayVersion(), &msg, sys)
	}

	n.online.Store(true)
	n.State.SetLastStart(raw)
	n.RefreshXrayVersion(ctx)
	n.Log.Info(fmt.Sprintf("✔ XRay Core v%s is up and running.", deref(n.XrayVersion())))
	n.Plugins.RunPostStart(req.Internals.Metadata)
	return n.startResponse(true, n.XrayVersion(), nil, sys)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// restartXrayProcess is stop → pre-start plugin → start with the config.
func (n *Node) restartXrayProcess(cfg map[string]any) error {
	if err := n.Proc.Stop(); err != nil {
		return err
	}
	n.Plugins.RunPreStart()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return n.Proc.Start(b.Bytes())
}

// StopXray is XrayService.stopXray.
func (n *Node) StopXray(withPluginCleanup, withOnlineCheck bool) bool {
	if withPluginCleanup {
		n.Plugins.Reset()
	}
	if withOnlineCheck && !n.online.Load() {
		return true
	}
	if err := n.Proc.Stop(); err != nil {
		n.Log.Error("Failed to stop Xray Process: " + err.Error())
		return false
	}
	n.online.Store(false)
	n.State.Cleanup(true)
	return true
}

// IsXrayReady is isXrayReady(): online, not starting, and the process is
// really up (an OOM-killed xray flips the node offline).
func (n *Node) IsXrayReady() bool {
	if n.starting.Load() || !n.online.Load() {
		return false
	}
	if !n.Proc.Status().Up {
		n.Log.Warn("Xray process is down despite isXrayOnline=true (possibly OOMKilled or crashed). Marking Xray offline.")
		n.online.Store(false)
		return false
	}
	return true
}

func (n *Node) IsOnline() bool { return n.online.Load() }

func (n *Node) registerXray(rt *api.Router) {
	rt.Handle(http.MethodPost, "/node/xray/start", api.AuthJWT, func(r *api.Request) {
		var req StartRequest
		if !validated(r, "StartXrayCommand", &req) {
			return
		}
		resp := n.StartXray(r.R.Context(), &req, append(json.RawMessage(nil), r.Body...), clientIP(r.R))
		n.Repl.Replicate("startXray", json.RawMessage(r.Body))
		r.OK(resp)
	})
	rt.Handle(http.MethodGet, "/node/xray/stop", api.AuthJWT, func(r *api.Request) {
		n.Log.Info("Remnawave requested to stop Xray.")
		r.OK(map[string]any{"isStopped": n.StopXray(true, false)})
	})
	rt.Handle(http.MethodGet, "/node/xray/healthcheck", api.AuthJWT, func(r *api.Request) {
		r.OK(map[string]any{
			"isAlive":                  true,
			"xrayInternalStatusCached": n.IsXrayReady(),
			"xrayVersion":              n.XrayVersion(),
			"nodeVersion":              Version,
		})
	})
}

// clientIP is Nest's @Ip(): the socket address as Express reports it.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if !strings.Contains(host, ":") {
		return "::ffff:" + host
	}
	return host
}
