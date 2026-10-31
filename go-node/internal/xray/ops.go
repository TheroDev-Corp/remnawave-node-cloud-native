package xray

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	statscmd "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	hyacc "github.com/xtls/xray-core/proxy/hysteria/account"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	ss2022 "github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/vless"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const callTimeout = 15 * time.Second

// Result mirrors the xtls-sdk's ISdkResponse: an OK flag plus the error
// message the SDK would report.
type Result struct {
	OK      bool
	Message string
}

// niceGrpcMessage formats a gRPC error the way nice-grpc's ClientError does
// ("/pkg.Service/Method CODE: details"), which is what the SDK embeds in its
// error messages.
func niceGrpcMessage(method string, err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}
	return fmt.Sprintf("%s %s: %s", method, strings.ToUpper(codeName(st.Code())), st.Message())
}

func codeName(c codes.Code) string {
	switch c {
	case codes.InvalidArgument:
		return "INVALID_ARGUMENT"
	case codes.DeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case codes.NotFound:
		return "NOT_FOUND"
	case codes.AlreadyExists:
		return "ALREADY_EXISTS"
	case codes.PermissionDenied:
		return "PERMISSION_DENIED"
	case codes.ResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case codes.FailedPrecondition:
		return "FAILED_PRECONDITION"
	case codes.OutOfRange:
		return "OUT_OF_RANGE"
	case codes.DataLoss:
		return "DATA_LOSS"
	}
	return strings.ToUpper(c.String())
}

const alterInbound = "/xray.app.proxyman.command.HandlerService/AlterInbound"

// User is the account for one AddUserOperation.
type User struct {
	Tag      string
	Email    string // the panel's userId
	Protocol string // trojan, vless, shadowsocks, shadowsocks22, hysteria
	Password string // trojan / shadowsocks password, ss2022 key, hysteria auth
	UUID     string // vless id
	Flow     string // vless flow
	Cipher   int32  // shadowsocks cipherType
	IVCheck  bool
}

func (u User) account() (proto.Message, error) {
	switch u.Protocol {
	case "trojan":
		return &trojan.Account{Password: u.Password}, nil
	case "vless":
		return &vless.Account{Id: u.UUID, Flow: u.Flow, Encryption: "none"}, nil
	case "shadowsocks":
		return &shadowsocks.Account{Password: u.Password, CipherType: shadowsocks.CipherType(u.Cipher), IvCheck: u.IVCheck}, nil
	case "shadowsocks22":
		return &ss2022.Account{Key: u.Password}, nil
	case "hysteria":
		return &hyacc.Account{Auth: u.Password}, nil
	}
	return nil, fmt.Errorf("unsupported protocol %q", u.Protocol)
}

// AddUser is handler.add<Protocol>User: level 0, "already exists" counts
// as success.
func (a *API) AddUser(ctx context.Context, u User) Result {
	acc, err := u.account()
	if err != nil {
		return Result{false, "Error adding user: " + err.Error()}
	}
	op := &handlercmd.AddUserOperation{User: &protocol.User{Email: u.Email, Level: 0, Account: serial.ToTypedMessage(acc)}}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err = a.Handler.AlterInbound(ctx, &handlercmd.AlterInboundRequest{Tag: u.Tag, Operation: serial.ToTypedMessage(op)})
	if err == nil {
		return Result{OK: true}
	}
	msg := niceGrpcMessage(alterInbound, err)
	if strings.Contains(msg, "already exists") {
		return Result{OK: true}
	}
	return Result{false, "Error adding user: " + msg}
}

// RemoveUser is handler.removeUser: "not found" counts as success.
func (a *API) RemoveUser(ctx context.Context, tag, email string) Result {
	op := &handlercmd.RemoveUserOperation{Email: email}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := a.Handler.AlterInbound(ctx, &handlercmd.AlterInboundRequest{Tag: tag, Operation: serial.ToTypedMessage(op)})
	if err == nil {
		return Result{OK: true}
	}
	msg := niceGrpcMessage(alterInbound, err)
	if strings.Contains(msg, "not found") {
		return Result{OK: true}
	}
	return Result{false, "Error removing user: " + msg}
}

// RemoveOutbound is handler.rawClient.removeOutbound.
func (a *API) RemoveOutbound(ctx context.Context, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := a.Handler.RemoveOutbound(ctx, &handlercmd.RemoveOutboundRequest{Tag: tag})
	return err
}

// Traffic is one user / inbound / outbound counter pair.
type Traffic struct {
	Name     string
	Uplink   int64
	Downlink int64
}

// QueryTraffic is stats.getAll{Users,Inbounds,Outbounds}Stats(reset): a
// QueryStats over "<kind>>>>" aggregated by the second name segment, in
// first-seen order (the SDK's Map preserves insertion order).
func (a *API) QueryTraffic(ctx context.Context, pattern string, reset bool) ([]Traffic, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := a.Stats.QueryStats(ctx, &statscmd.QueryStatsRequest{Pattern: pattern, Reset_: reset})
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	var out []Traffic
	for _, s := range resp.Stat {
		parts := strings.Split(s.Name, ">>>")
		name, kind := "", ""
		if len(parts) > 1 {
			name = parts[1]
		}
		if len(parts) > 3 {
			kind = parts[3]
		}
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, Traffic{Name: name})
		}
		switch kind {
		case "uplink":
			out[i].Uplink += s.Value
		case "downlink":
			out[i].Downlink += s.Value
		}
	}
	return out, nil
}

// UserOnline is stats.getUserOnlineStatus: an "online not found." error
// means offline.
func (a *API) UserOnline(ctx context.Context, user string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := a.Stats.GetStatsOnline(ctx, &statscmd.GetStatsRequest{Name: "user>>>" + user + ">>>online"})
	if err == nil {
		return true, nil
	}
	if strings.Contains(niceGrpcMessage("/xray.app.stats.command.StatsService/GetStatsOnline", err), "online not found.") {
		return false, nil
	}
	return false, err
}

// IPSeen is one online IP with its last-seen unix time.
type IPSeen struct {
	IP       string
	LastSeen int64
}

// UserIPs is rawClient.getStatsOnlineIpList({name: user>>>id>>>online, reset:true}).
// codes.NotFound means no IPs.
func (a *API) UserIPs(ctx context.Context, user string) ([]IPSeen, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := a.Stats.GetStatsOnlineIpList(ctx, &statscmd.GetStatsRequest{Name: "user>>>" + user + ">>>online", Reset_: true})
	if err != nil {
		return nil, err
	}
	return sortedIPs(resp.Ips), nil
}

// sortedIPs keeps a stable order for a Go map (JS Object.entries keeps
// insertion order, which xray's map does not define either).
func sortedIPs(m map[string]int64) []IPSeen {
	out := make([]IPSeen, 0, len(m))
	for ip, ts := range m {
		out = append(out, IPSeen{ip, ts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// UserIPList is one user in stats.getUsersStats(false, false).
type UserIPList struct {
	UserID string
	IPs    []IPSeen
}

// UsersIPs is stats.getUsersStats(includeTraffic=false, reset=false), with
// the SDK's fallback to GetAllOnlineUsers + GetStatsOnlineIpList when the
// core lacks GetUsersStats.
func (a *API) UsersIPs(ctx context.Context) ([]UserIPList, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := a.Stats.GetUsersStats(cctx, &statscmd.GetUsersStatsRequest{})
	if err == nil {
		out := make([]UserIPList, 0, len(resp.Users))
		for _, u := range resp.Users {
			ips := make([]IPSeen, 0, len(u.Ips))
			for _, ip := range u.Ips {
				ips = append(ips, IPSeen{ip.Ip, ip.LastSeen})
			}
			out = append(out, UserIPList{UserID: u.Email, IPs: ips})
		}
		return out, nil
	}
	if status.Code(err) != codes.Unimplemented {
		return nil, err
	}
	online, err := a.Stats.GetAllOnlineUsers(cctx, &statscmd.GetAllOnlineUsersRequest{})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []UserIPList
	for _, metric := range online.Users {
		if seen[metric] {
			continue
		}
		seen[metric] = true
		email := strings.TrimSuffix(strings.TrimPrefix(metric, "user>>>"), ">>>online")
		r, err := a.Stats.GetStatsOnlineIpList(cctx, &statscmd.GetStatsRequest{Name: metric})
		if err != nil || len(r.Ips) == 0 {
			continue
		}
		out = append(out, UserIPList{UserID: email, IPs: sortedIPs(r.Ips)})
	}
	return out, nil
}
