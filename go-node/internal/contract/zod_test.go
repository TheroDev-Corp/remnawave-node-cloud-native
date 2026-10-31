package contract

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
)

// TestZodParity runs bodies through the TS contract's real zod schemas and
// through the Go walker; the issues must match exactly.
func TestZodParity(t *testing.T) {
	const u = "9e5b6ed0-c0b4-4d4d-a0c4-6c8d7a0d4a8e"
	cases := []struct{ cmd, body string }{
		{"GetUsersStatsCommand", `{"reset":true}`},
		{"GetUsersStatsCommand", `{"reset":true,"extra":1}`},
		{"GetUsersStatsCommand", `{"reset":"yes"}`},
		{"GetUsersStatsCommand", `{}`},
		{"GetUsersStatsCommand", `[1]`},
		{"GetUsersStatsCommand", `null`},
		{"GetUsersStatsCommand", `"x"`},
		{"GetInboundStatsCommand", `{"reset":false}`},
		{"GetInboundStatsCommand", `{"tag":7,"reset":null}`},
		{"RemoveUserCommand", `{"username":"1","hashData":{"vlessUuid":"` + u + `"}}`},
		{"RemoveUserCommand", `{"username":"1"}`},
		{"RemoveUserCommand", `{"username":"1","hashData":{"vlessUuid":"nope"}}`},
		{"RemoveUserCommand", `{"username":"1","hashData":[]}`},
		{"AddUserCommand", `{"data":[{"type":"vless","tag":"t","username":"1","uuid":"` + u + `","flow":""}],"hashData":{"vlessUuid":"` + u + `"}}`},
		{"AddUserCommand", `{"data":[{"type":"vless","tag":"t","username":"1","uuid":"` + u + `","flow":"bad"}],"hashData":{"vlessUuid":"` + u + `"}}`},
		{"AddUserCommand", `{"data":[{"type":"nope","tag":"t","username":"1"}],"hashData":{"vlessUuid":"x"}}`},
		{"AddUserCommand", `{"data":[{"tag":"t"}],"hashData":{"vlessUuid":"` + u + `"}}`},
		{"AddUserCommand", `{"data":["x"],"hashData":{"vlessUuid":"` + u + `"}}`},
		{"AddUserCommand", `{"data":[{"type":"shadowsocks","tag":"SS","username":"9","password":"p","cipherType":99,"ivCheck":false}],"hashData":{"vlessUuid":"not-a-uuid"}}`},
		{"AddUserCommand", `{"data":[{"type":"shadowsocks","tag":"SS","username":"9","password":"p","cipherType":"6"}],"hashData":{"vlessUuid":"` + u + `","prevVlessUuid":5}}`},
		{"AddUsersCommand", `{"affectedInboundTags":["a"],"users":[{"inboundData":[{"type":"vless","tag":"a"}],"userData":{"userId":"1"}}]}`},
		{"RemoveUsersCommand", `{"users":[{"userId":5,"hashUuid":"` + u + `"}]}`},
		{"DropIpsCommand", `{"ips":[]}`},
		{"DropIpsCommand", `{"ips":[1]}`},
		{"DropUsersConnectionsCommand", `{}`},
		{"BlockIpsCommand", `{"ips":[{"ip":"999.1.1.1","timeout":1}]}`},
		{"BlockIpsCommand", `{"ips":[{"ip":"::1"}]}`},
		{"UnblockIpsCommand", `{"ips":["1.1.1.1","x"]}`},
		{"GetGeocheckCommand", `{"ip":1,"interface":true}`},
		{"SyncCommand", `{"plugin":null}`},
		{"SyncCommand", `{}`},
		{"SyncCommand", `{"plugin":{"uuid":"x","name":1}}`},
		{"SyncCommand", `{"plugin":{"uuid":"` + u + `","name":"n","config":[]}}`},
		{"StartXrayCommand", `{"internals":{"hashes":{"emptyConfig":"e","inbounds":[]}},"xrayConfig":{}}`},
		{"StartXrayCommand", `{"internals":{"forceRestart":"no","hashes":{"emptyConfig":1,"inbounds":[{"tag":"a"}]},"metadata":{"name":"n"}},"xrayConfig":[]}`},
		{"StartXrayCommand", `{"internals":{"forceRestart":false,"hashes":{"emptyConfig":"e","inbounds":[]},"integrations":{"a":1}},"xrayConfig":{"log":null}}`},
	}
	items := make([]any, len(cases))
	for i, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.body), &v); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		items[i] = map[string]any{"command": c.cmd, "value": v}
	}
	var want [][]map[string]any
	nodefixture.Run(t, map[string]any{"op": "zodIssues", "items": items}, &want)
	for i, c := range cases {
		got := ValidateRequest(c.cmd, []byte(c.body))
		raw, _ := json.Marshal(got)
		var gotNorm []map[string]any
		_ = json.Unmarshal(raw, &gotNorm)
		if len(gotNorm) == 0 && len(want[i]) == 0 {
			continue
		}
		if !reflect.DeepEqual(gotNorm, want[i]) {
			w, _ := json.Marshal(want[i])
			t.Errorf("%s %s\n zod: %s\n  go: %s", c.cmd, c.body, w, raw)
		}
	}
}
