package node

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// AddUserRequest is AddUserCommand.Request.
type AddUserRequest struct {
	Data []struct {
		Type       string `json:"type"`
		Tag        string `json:"tag"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		UUID       string `json:"uuid"`
		Flow       string `json:"flow"`
		CipherType int32  `json:"cipherType"`
		IVCheck    bool   `json:"ivCheck"`
	} `json:"data"`
	HashData struct {
		VlessUUID     string `json:"vlessUuid"`
		PrevVlessUUID string `json:"prevVlessUuid"`
	} `json:"hashData"`
}

// AddUsersRequest is AddUsersCommand.Request.
type AddUsersRequest struct {
	AffectedInboundTags []string `json:"affectedInboundTags"`
	Users               []struct {
		InboundData []struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
			Flow string `json:"flow"`
		} `json:"inboundData"`
		UserData struct {
			UserID         string `json:"userId"`
			HashUUID       string `json:"hashUuid"`
			VlessUUID      string `json:"vlessUuid"`
			TrojanPassword string `json:"trojanPassword"`
			SSPassword     string `json:"ssPassword"`
		} `json:"userData"`
	} `json:"users"`
}

type userResult struct {
	Success bool    `json:"success"`
	Error   *string `json:"error"`
}

// singleUserOps maps add-user items to xray operations exactly as
// HandlerService.addUser does: the ss22 key and shadowsocks cipherType are
// passed through as sent.
func singleUserOps(req *AddUserRequest) []xray.User {
	ops := make([]xray.User, 0, len(req.Data))
	for _, d := range req.Data {
		u := xray.User{Tag: d.Tag, Email: d.Username, Protocol: d.Type}
		switch d.Type {
		case "trojan", "shadowsocks22", "hysteria":
			u.Password = d.Password
		case "vless":
			u.UUID, u.Flow = d.UUID, d.Flow
		case "shadowsocks":
			u.Password, u.Cipher, u.IVCheck = d.Password, d.CipherType, false
		}
		ops = append(ops, u)
	}
	return ops
}

// applyUser removes the user from every known inbound, then adds the
// operations, keeping the per-inbound hash sets in sync. It returns the
// per-operation results.
func (n *Node) applyUser(ctx context.Context, userID, removeHash, addHash string, ops []xray.User) []xray.Result {
	for _, tag := range n.State.Tags() {
		n.XAPI.RemoveUser(ctx, tag, userID)
		n.State.RemoveUserFromInbound(tag, removeHash)
	}
	results := make([]xray.Result, 0, len(ops))
	for _, op := range ops {
		res := n.XAPI.AddUser(ctx, op)
		if res.OK {
			n.State.AddUserToInbound(op.Tag, addHash)
		}
		results = append(results, res)
	}
	return results
}

func allFailed(rs []xray.Result) (bool, *string) {
	for _, r := range rs {
		if r.OK {
			return false, nil
		}
	}
	for _, r := range rs {
		m := r.Message
		return len(rs) > 0, &m
	}
	return false, nil
}

// AddUser is HandlerService.addUser.
func (n *Node) AddUser(ctx context.Context, req *AddUserRequest) userResult {
	if len(req.Data) == 0 {
		return userResult{Success: true}
	}
	userID := req.Data[0].Username
	ops := singleUserOps(req)
	n.State.StoreDefinition(userID, UserDef{Ops: ops, Hash: req.HashData.VlessUUID})
	for _, d := range req.Data {
		n.State.AddTag(d.Tag)
	}
	removeHash := req.HashData.VlessUUID
	if req.HashData.PrevVlessUUID != "" {
		removeHash = req.HashData.PrevVlessUUID
	}
	rs := n.applyUser(ctx, userID, removeHash, req.HashData.VlessUUID, ops)
	if failed, msg := allFailed(rs); failed {
		n.Log.Error("Error adding users: " + deref(msg))
		return userResult{false, msg}
	}
	return userResult{Success: true}
}

// RemoveUser is HandlerService.removeUser.
func (n *Node) RemoveUser(ctx context.Context, username, vlessUUID string) userResult {
	tags := n.State.Tags()
	if len(tags) == 0 {
		return userResult{Success: true}
	}
	n.State.RemoveDefinition(username)
	var rs []xray.Result
	for _, tag := range tags {
		rs = append(rs, n.XAPI.RemoveUser(ctx, tag, username))
		n.State.RemoveUserFromInbound(tag, vlessUUID)
	}
	if failed, msg := allFailed(rs); failed {
		return userResult{false, msg}
	}
	return userResult{Success: true}
}

// bulkUserOps is addUsers' mapping: shadowsocks gets cipherType 0 and the
// ss22 key is base64(ssPassword). Unlike the TS fork, these exact values
// are what the user definition stores (see State).
func bulkUserOps(userID, vless, trojanPw, ssPw string, inbound []struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
	Flow string `json:"flow"`
}) []xray.User {
	ops := make([]xray.User, 0, len(inbound))
	for _, in := range inbound {
		u := xray.User{Tag: in.Tag, Email: userID, Protocol: in.Type}
		switch in.Type {
		case "trojan":
			u.Password = trojanPw
		case "vless":
			u.UUID, u.Flow = vless, in.Flow
		case "shadowsocks":
			u.Password, u.Cipher = ssPw, 0
		case "shadowsocks22":
			u.Password = base64.StdEncoding.EncodeToString([]byte(ssPw))
		case "hysteria":
			u.Password = vless
		default:
			continue
		}
		ops = append(ops, u)
	}
	return ops
}

// AddUsers is HandlerService.addUsers (always reports success, like TS).
func (n *Node) AddUsers(ctx context.Context, req *AddUsersRequest) userResult {
	t0 := time.Now()
	for _, tag := range req.AffectedInboundTags {
		n.State.AddTag(tag)
	}
	n.Log.Info(fmt.Sprintf("Adding %d users to inbounds: %v", len(req.Users), req.AffectedInboundTags))
	for _, u := range req.Users {
		ud := u.UserData
		ops := bulkUserOps(ud.UserID, ud.VlessUUID, ud.TrojanPassword, ud.SSPassword, u.InboundData)
		n.State.StoreDefinition(ud.UserID, UserDef{Ops: ops, Hash: ud.VlessUUID})
		n.applyUser(ctx, ud.UserID, ud.HashUUID, ud.VlessUUID, ops)
	}
	n.Log.Info("Users addition took: " + time.Since(t0).Round(time.Millisecond).String())
	return userResult{Success: true}
}

// RemoveUsers is HandlerService.removeUsers.
func (n *Node) RemoveUsers(ctx context.Context, users []struct {
	UserID   string `json:"userId"`
	HashUUID string `json:"hashUuid"`
}) userResult {
	tags := n.State.Tags()
	if len(tags) == 0 {
		return userResult{Success: true}
	}
	var rs []xray.Result
	for _, u := range users {
		n.State.RemoveDefinition(u.UserID)
		for _, tag := range tags {
			rs = append(rs, n.XAPI.RemoveUser(ctx, tag, u.UserID))
			n.State.RemoveUserFromInbound(tag, u.HashUUID)
		}
	}
	if failed, msg := allFailed(rs); failed {
		return userResult{false, msg}
	}
	return userResult{Success: true}
}

func (n *Node) registerHandler(rt *api.Router) {
	rt.Handle(http.MethodPost, "/node/handler/add-user", api.AuthJWT, func(r *api.Request) {
		var req AddUserRequest
		if !validated(r, "AddUserCommand", &req) {
			return
		}
		res := n.AddUser(r.R.Context(), &req)
		n.recordAndReplicate("addUser", r.Body)
		r.OK(res)
	})
	rt.Handle(http.MethodPost, "/node/handler/remove-user", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Username string `json:"username"`
			HashData struct {
				VlessUUID string `json:"vlessUuid"`
			} `json:"hashData"`
		}
		if !validated(r, "RemoveUserCommand", &req) {
			return
		}
		res := n.RemoveUser(r.R.Context(), req.Username, req.HashData.VlessUUID)
		n.recordAndReplicate("removeUser", r.Body)
		r.OK(res)
	})
	rt.Handle(http.MethodPost, "/node/handler/add-users", api.AuthJWT, func(r *api.Request) {
		var req AddUsersRequest
		if !validated(r, "AddUsersCommand", &req) {
			return
		}
		res := n.AddUsers(r.R.Context(), &req)
		n.recordAndReplicate("addUsers", r.Body)
		r.OK(res)
	})
	rt.Handle(http.MethodPost, "/node/handler/remove-users", api.AuthJWT, func(r *api.Request) {
		var req struct {
			Users []struct {
				UserID   string `json:"userId"`
				HashUUID string `json:"hashUuid"`
			} `json:"users"`
		}
		if !validated(r, "RemoveUsersCommand", &req) {
			return
		}
		res := n.RemoveUsers(r.R.Context(), req.Users)
		n.recordAndReplicate("removeUsers", r.Body)
		r.OK(res)
	})
	// The fork removed nftables/sockdestroy: both answer success and do nothing.
	rt.Handle(http.MethodPost, "/node/handler/drop-users-connections", api.AuthJWT, func(r *api.Request) {
		var req map[string]any
		if !validated(r, "DropUsersConnectionsCommand", &req) {
			return
		}
		r.OK(map[string]any{"success": true})
	})
	rt.Handle(http.MethodPost, "/node/handler/drop-ips", api.AuthJWT, func(r *api.Request) {
		var req map[string]any
		if !validated(r, "DropIpsCommand", &req) {
			return
		}
		r.OK(map[string]any{"success": true})
	})
}
