package node

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
)

// The node side of ha.Local: what peer sync reads from and does to this node.

func (n *Node) IsStarting() bool                 { return n.starting.Load() }
func (n *Node) LastStart() json.RawMessage       { return n.State.LastStart() }
func (n *Node) SetLastStart(b json.RawMessage)   { n.State.SetLastStart(b) }
func (n *Node) XrayConfig() map[string]any       { return n.State.XrayConfig() }
func (n *Node) SetXrayConfig(cfg map[string]any) { n.State.SetXrayConfig(cfg) }

func (n *Node) PluginSnapshot() json.RawMessage {
	b, _ := n.lastPluginSync.Load().(json.RawMessage)
	return b
}

// StartFrom is startXray(body, '127.0.0.1').
func (n *Node) StartFrom(ctx context.Context, body json.RawMessage) {
	var req StartRequest
	if err := json.Unmarshal(body, &req); err != nil {
		n.Log.Error("[PEER-SYNC] Stored start request is invalid: " + err.Error())
		return
	}
	n.StartXray(ctx, &req, body, "127.0.0.1")
}

func (n *Node) resetJournal() {
	n.journalMu.Lock()
	n.journal = nil
	n.journalMu.Unlock()
}

func (n *Node) record(action string, body json.RawMessage) {
	n.journalMu.Lock()
	n.journal = append(n.journal, ha.Action{Type: action, Body: append(json.RawMessage(nil), body...)})
	n.journalMu.Unlock()
}

func (n *Node) recordAndReplicate(action string, body json.RawMessage) {
	n.record(action, body)
	n.Repl.Replicate(action, json.RawMessage(body))
}

// Journal is the user operations since the last full start, in order.
func (n *Node) Journal() []ha.Action {
	n.journalMu.Lock()
	defer n.journalMu.Unlock()
	return append([]ha.Action(nil), n.journal...)
}

// replayJournal re-applies the journal after xray restarted from the same
// start request, so users added or removed since then survive the restart.
func (n *Node) replayJournal(ctx context.Context) {
	ops := n.Journal()
	for _, a := range ops {
		if err := n.applyOp(ctx, a.Type, a.Body); err != nil {
			n.Log.Warn(fmt.Sprintf("Journal %q not re-applied: %v", a.Type, err))
		}
	}
	if len(ops) > 0 {
		n.Log.Info(fmt.Sprintf("Re-applied %d user operations made since the last config push", len(ops)))
	}
}

// Apply runs a replicated action on this node without replicating it again.
func (n *Node) Apply(ctx context.Context, action string, body json.RawMessage) error {
	switch action {
	case "addUser", "addUsers", "removeUser", "removeUsers", "suspendUser", "restoreUser":
		n.record(action, body)
	}
	return n.applyOp(ctx, action, body)
}

func (n *Node) applyOp(ctx context.Context, action string, body json.RawMessage) error {
	switch action {
	case "addUser":
		var req AddUserRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		n.AddUser(ctx, &req)
	case "addUsers":
		var req AddUsersRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		n.AddUsers(ctx, &req)
	case "removeUser":
		var req struct {
			Username string `json:"username"`
			HashData struct {
				VlessUUID string `json:"vlessUuid"`
			} `json:"hashData"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		n.RemoveUser(ctx, req.Username, req.HashData.VlessUUID)
	case "removeUsers":
		var req struct {
			Users []struct {
				UserID   string `json:"userId"`
				HashUUID string `json:"hashUuid"`
			} `json:"users"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		n.RemoveUsers(ctx, req.Users)
	case "suspendUser":
		var req struct {
			UserID          string `json:"userId"`
			DurationSeconds int64  `json:"durationSeconds"`
			UnbanAt         int64  `json:"unbanAt"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		// Math.max(1, Math.round((unbanAt - Date.now()) / 1000)); 0 stays permanent.
		secs := int64(math.Max(1, math.Round(float64(req.UnbanAt-time.Now().UnixMilli())/1000)))
		if req.DurationSeconds == 0 {
			secs = 0
		}
		n.Plugins.Suspend(ctx, req.UserID, secs, false)
	case "restoreUser":
		var req struct {
			UserID string `json:"userId"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		n.Plugins.Restore(ctx, req.UserID, false)
	case "pluginSync":
		var req struct {
			Plugin *struct {
				Config json.RawMessage `json:"config"`
				UUID   string          `json:"uuid"`
				Name   string          `json:"name"`
			} `json:"plugin"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		n.lastPluginSync.Store(append(json.RawMessage(nil), body...))
		n.Plugins.Sync(ctx, req.Plugin)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return nil
}
