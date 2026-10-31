// Package health serves the fork's liveness/readiness answers, both on the
// mTLS port (/node/health/*, health.controller.ts) and on the plain-HTTP
// HEALTH_PORT (/health/*, health.service.ts).
package health

import (
	"encoding/json"
	"net/http"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

func role(st ha.State, leaderWord string) string {
	switch {
	case st.IsLeader():
		return "leader"
	case st.IsDraining():
		return "leader-draining"
	default:
		return leaderWord
	}
}

// Live is the /health/live body.
func Live(st ha.State) map[string]any {
	r := "follower"
	if st.IsLeader() {
		r = "leader"
	}
	return map[string]any{"status": "ok", "role": r, "identity": st.Identity()}
}

// Ready is the /health/ready status and body: a leader (or draining leader)
// is always ready so the panel can push the first config; a standby only
// once its xray is up.
func Ready(st ha.State, p *xray.Process) (int, map[string]any) {
	s := p.Status()
	ready := st.IsLeader() || st.IsDraining() || s.Up
	var pid any
	if s.PID > 0 {
		pid = s.PID
	}
	status, code := "ok", http.StatusOK
	if !ready {
		status, code = "standby-xray-not-ready", http.StatusServiceUnavailable
	}
	xr := "down"
	if s.Up {
		xr = "up"
	}
	return code, map[string]any{
		"status":         status,
		"role":           role(st, "standby"),
		"servingTraffic": st.IsServingTraffic(),
		"draining":       st.IsDraining(),
		"xray":           xr,
		"pid":            pid,
		"identity":       st.Identity(),
	}
}

// PlainServer is the HEALTH_PORT Express app (peer-sync and leader routes
// are added by the ha package in milestone 4).
func PlainServer(st ha.State, p *xray.Process) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Live(st))
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		code, body := Ready(st, p)
		writeJSON(w, code, body)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
