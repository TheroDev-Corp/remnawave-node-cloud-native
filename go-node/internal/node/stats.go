package node

import (
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// Errors from libs/contract/constants/errors (HttpExceptionFilter bodies).
type apiErr struct {
	code, msg string
}

var (
	errSystemStats   = apiErr{"A010", "Failed to get system stats"}
	errUsersStats    = apiErr{"A011", "Failed to get users stats"}
	errInboundStats  = apiErr{"A012", "Failed to get inbound stats"}
	errOutboundStats = apiErr{"A013", "Failed to get outbound stats"}
	errInboundsStats = apiErr{"A015", "Failed to get inbounds stats"}
	errCombined      = apiErr{"A017", "Failed to get combined stats"}
	errGeocheck      = apiErr{"A018", "Failed to get geocheck report"}
)

func fail(r *api.Request, e apiErr, msg string) {
	if msg == "" {
		msg = e.msg
	}
	r.Error(http.StatusInternalServerError, msg, e.code)
}

// counter JSON: numbers, never strings (the panel adds them with JS `+`).
type userTraffic struct {
	Username string `json:"username"`
	Downlink int64  `json:"downlink"`
	Uplink   int64  `json:"uplink"`
}

type tagTraffic struct {
	Inbound  string `json:"inbound,omitempty"`
	Outbound string `json:"outbound,omitempty"`
	Downlink int64  `json:"downlink"`
	Uplink   int64  `json:"uplink"`
}

func inbounds(ts []xray.Traffic) []tagTraffic {
	out := make([]tagTraffic, 0, len(ts))
	for _, t := range ts {
		out = append(out, tagTraffic{Inbound: t.Name, Downlink: t.Downlink, Uplink: t.Uplink})
	}
	return out
}

func outbounds(ts []xray.Traffic) []tagTraffic {
	out := make([]tagTraffic, 0, len(ts))
	for _, t := range ts {
		out = append(out, tagTraffic{Outbound: t.Name, Downlink: t.Downlink, Uplink: t.Uplink})
	}
	return out
}

type ipEntry struct {
	IP       string `json:"ip"`
	LastSeen string `json:"lastSeen"` // new Date(ts*1000) serialized by JSON.stringify
}

func ipEntries(ips []xray.IPSeen) []ipEntry {
	out := make([]ipEntry, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ipEntry{ip.IP, time.Unix(ip.LastSeen, 0).UTC().Format("2006-01-02T15:04:05.000Z")})
	}
	return out
}

type xrayInfo struct {
	NumGoroutine uint32 `json:"numGoroutine"`
	NumGC        uint32 `json:"numGC"`
	Alloc        uint64 `json:"alloc"`
	TotalAlloc   uint64 `json:"totalAlloc"`
	Sys          uint64 `json:"sys"`
	Mallocs      uint64 `json:"mallocs"`
	Frees        uint64 `json:"frees"`
	LiveObjects  uint64 `json:"liveObjects"`
	PauseTotalNs uint64 `json:"pauseTotalNs"`
	Uptime       uint32 `json:"uptime"`
}

func (n *Node) registerStats(rt *api.Router) {
	rt.Handle(http.MethodPost, "/node/stats/get-user-online-status", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Username string `json:"username"`
		}
		if !validated(r, "GetUserOnlineStatusCommand", &req) {
			return
		}
		online, err := n.XAPI.UserOnline(r.R.Context(), req.Username)
		if err != nil {
			online = false
		}
		r.OK(map[string]any{"isOnline": online})
	})

	// get-system-stats doubles as the panel's health check: it must fail
	// (non-2xx) while xray is down, so the panel reconnects and re-pushes.
	rt.Handle(http.MethodGet, "/node/stats/get-system-stats", api.AuthJWT, func(r *api.Request) {
		s, err := n.XAPI.SysStats(r.R.Context())
		if err != nil {
			n.Log.Warn("Failed to get system stats", "err", err)
			fail(r, errSystemStats, "")
			return
		}
		st := GetSystemStats()
		st.Interface = n.Net.Default()
		r.OK(map[string]any{
			"xrayInfo": xrayInfo{s.NumGoroutine, s.NumGC, s.Alloc, s.TotalAlloc, s.Sys, s.Mallocs, s.Frees, s.LiveObjects, s.PauseTotalNs, s.Uptime},
			"plugins":  map[string]any{"torrentBlocker": map[string]any{"reportsCount": n.Plugins.ReportsCount()}},
			"system":   map[string]any{"stats": st},
		})
	})

	// reset:true reads and zeroes the counters in one QueryStats call, so a
	// failed request never loses traffic.
	rt.Handle(http.MethodPost, "/node/stats/get-users-stats", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Reset bool `json:"reset"`
		}
		if !validated(r, "GetUsersStatsCommand", &req) {
			return
		}
		ts, err := n.XAPI.QueryTraffic(r.R.Context(), "user>>>", req.Reset)
		if err != nil {
			fail(r, errUsersStats, "")
			return
		}
		users := make([]userTraffic, 0, len(ts))
		for _, t := range ts {
			if t.Uplink != 0 || t.Downlink != 0 {
				users = append(users, userTraffic{t.Name, t.Downlink, t.Uplink})
			}
		}
		r.OK(map[string]any{"users": users})
	})

	one := func(path, command, pattern string, e apiErr, key string) {
		rt.Handle(http.MethodPost, path, api.AuthJWT, func(r *api.Request) {
			var req struct {
				Tag   string `json:"tag"`
				Reset bool   `json:"reset"`
			}
			if !validated(r, command, &req) {
				return
			}
			ts, err := n.XAPI.QueryTraffic(r.R.Context(), pattern+">>>"+req.Tag+">>>", req.Reset)
			if err != nil || len(ts) == 0 {
				fail(r, e, "")
				return
			}
			r.OK(map[string]any{key: ts[0].Name, "downlink": ts[0].Downlink, "uplink": ts[0].Uplink})
		})
	}
	one("/node/stats/get-inbound-stats", "GetInboundStatsCommand", "inbound", errInboundStats, "inbound")
	one("/node/stats/get-outbound-stats", "GetOutboundStatsCommand", "outbound", errOutboundStats, "outbound")

	rt.Handle(http.MethodPost, "/node/stats/get-all-inbounds-stats", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Reset bool `json:"reset"`
		}
		if !validated(r, "GetAllInboundsStatsCommand", &req) {
			return
		}
		ts, err := n.XAPI.QueryTraffic(r.R.Context(), "inbound>>>", req.Reset)
		if err != nil {
			fail(r, errInboundsStats, "")
			return
		}
		r.OK(map[string]any{"inbounds": inbounds(ts)})
	})
	rt.Handle(http.MethodPost, "/node/stats/get-all-outbounds-stats", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Reset bool `json:"reset"`
		}
		if !validated(r, "GetAllOutboundsStatsCommand", &req) {
			return
		}
		ts, err := n.XAPI.QueryTraffic(r.R.Context(), "outbound>>>", req.Reset)
		if err != nil {
			fail(r, errInboundsStats, "") // the TS node reports the inbounds error here too
			return
		}
		r.OK(map[string]any{"outbounds": outbounds(ts)})
	})
	rt.Handle(http.MethodPost, "/node/stats/get-combined-stats", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Reset bool `json:"reset"`
		}
		if !validated(r, "GetCombinedStatsCommand", &req) {
			return
		}
		in, err1 := n.XAPI.QueryTraffic(r.R.Context(), "inbound>>>", req.Reset)
		out, err2 := n.XAPI.QueryTraffic(r.R.Context(), "outbound>>>", req.Reset)
		if err1 != nil || err2 != nil {
			fail(r, errCombined, "")
			return
		}
		r.OK(map[string]any{"inbounds": inbounds(in), "outbounds": outbounds(out)})
	})

	rt.Handle(http.MethodPost, "/node/stats/get-user-ip-list", api.AuthJWT, func(r *api.Request) {
		var req struct {
			UserID string `json:"userId"`
		}
		if !validated(r, "GetUserIpListCommand", &req) {
			return
		}
		ips, err := n.XAPI.UserIPs(r.R.Context(), req.UserID)
		if err != nil && status.Code(err) != codes.NotFound {
			n.Log.Error("get-user-ip-list: " + err.Error())
		}
		r.OK(map[string]any{"ips": ipEntries(ips)})
	})
	rt.Handle(http.MethodGet, "/node/stats/get-users-ip-list", api.AuthJWT, func(r *api.Request) {
		users, err := n.XAPI.UsersIPs(r.R.Context())
		out := make([]map[string]any, 0, len(users))
		if err != nil {
			n.Log.Error("get-users-ip-list: " + err.Error())
		}
		for _, u := range users {
			out = append(out, map[string]any{"userId": u.UserID, "ips": ipEntries(u.IPs)})
		}
		r.OK(map[string]any{"users": out})
	})
	rt.Handle(http.MethodPost, "/node/stats/get-geocheck", api.AuthJWT, func(r *api.Request) {
		var req struct {
			IP        string `json:"ip"`
			Interface string `json:"interface"`
		}
		if !validated(r, "GetGeocheckCommand", &req) {
			return
		}
		report, err := n.geocheck(r.R.Context(), req.IP, req.Interface)
		if err != nil {
			fail(r, errGeocheck, err.Error())
			return
		}
		r.OK(report)
	})
}
