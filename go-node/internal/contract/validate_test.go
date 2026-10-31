package contract

import "testing"

func TestValidateRequest(t *testing.T) {
	for _, c := range []struct {
		cmd, body string
		ok        bool
	}{
		{"GetUsersStatsCommand", `{"reset":true}`, true},
		{"GetUsersStatsCommand", `{"reset":"yes"}`, false},
		{"GetUsersStatsCommand", `{}`, false}, // reset is required
		{"RemoveUserCommand", `{"username":"1","hashData":{"vlessUuid":"9e5b6ed0-c0b4-4d4d-a0c4-6c8d7a0d4a8e"}}`, true},
		{"RemoveUserCommand", `{"username":"1"}`, false},
		{"GetSystemStatsCommand", `anything`, true}, // no request schema
		{"AddUserCommand", `{"data":[{"type":"vless","tag":"t","username":"1","uuid":"9e5b6ed0-c0b4-4d4d-a0c4-6c8d7a0d4a8e","flow":""}],"hashData":{"vlessUuid":"9e5b6ed0-c0b4-4d4d-a0c4-6c8d7a0d4a8e"}}`, true},
		{"AddUserCommand", `{"data":[{"type":"nope","tag":"t","username":"1"}],"hashData":{"vlessUuid":"x"}}`, false},
	} {
		issues := ValidateRequest(c.cmd, []byte(c.body))
		if (len(issues) == 0) != c.ok {
			t.Errorf("%s %s: issues=%v, want ok=%v", c.cmd, c.body, issues, c.ok)
		}
	}
}

func TestValidatePlugin(t *testing.T) {
	ok := `{"sharedLists":[{"type":"ipList","name":"ext:office","items":["1.1.1.1","10.0.0.0/8"]},{"type":"asList","name":"ext:as","items":[13335]}],
"torrentBlocker":{"enabled":true,"blockDuration":60,"ignoreLists":{"ip":["ext:office","8.8.8.8"],"userId":[1,2]}}}`
	if issues := ValidatePlugin([]byte(ok)); issues != nil {
		t.Fatalf("valid plugin rejected: %v", issues)
	}
	for _, bad := range []string{
		`{"torrentBlocker":{"enabled":"x","blockDuration":60,"ignoreLists":{}}}`,
		`{"sharedLists":[{"type":"ipList","name":"office","items":[]}]}`, // name must start with ext:
		`{"sharedLists":[{"type":"ipList","name":"ext:x","items":["not-an-ip"]}]}`,
	} {
		if ValidatePlugin([]byte(bad)) == nil {
			t.Errorf("invalid plugin accepted: %s", bad)
		}
	}
}
