package secretkey

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
)

func TestLoadPanelShapedKey(t *testing.T) {
	p := testpki.New()
	m, checks, err := Load(p.SecretKey(), time.Now())
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, Report(checks, ""))
	}
	if len(checks) != 7 {
		t.Fatalf("want the 7 checks of validate-secret-key.util.ts, got %d", len(checks))
	}
	if m.SNI == "" || !m.SNIMatches(m.SNI) || m.SNIMatches("") || m.SNIMatches(m.SNI+"x") {
		t.Errorf("SNI matching broken: %q", m.SNI)
	}
}

func TestLoadRejects(t *testing.T) {
	p, other := testpki.New(), testpki.New()
	enc := func(m map[string]string) string {
		b, _ := json.Marshal(m)
		return base64.StdEncoding.EncodeToString(b)
	}
	base := map[string]string{"caCertPem": p.CACertPem, "jwtPublicKey": p.JWTPublicPem, "nodeCertPem": p.NodeCertPem, "nodeKeyPem": p.NodeKeyPem}
	with := func(k, v string) string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return enc(m)
	}
	for _, c := range []struct{ name, secret, failing string }{
		{"node cert from another CA", with("nodeCertPem", other.NodeCertPem), "node signed by CA"},
		{"key does not match cert", with("nodeKeyPem", other.NodeKeyPem), "node key matches cert"},
		{"garbage CA", with("caCertPem", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"), "CA parses"},
		{"garbage jwt key", with("jwtPublicKey", "nope"), "jwt public key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, checks, err := Load(c.secret, time.Now())
			if err == nil {
				t.Fatal("expected failure")
			}
			for _, ch := range checks {
				if ch.Name == c.failing && ch.OK {
					t.Errorf("check %q should fail", c.failing)
				}
			}
		})
	}
	for _, c := range []struct{ name, secret, msg string }{
		{"not base64 json", "!!!", "invalid JSON"},
		{"missing field", enc(map[string]string{"caCertPem": "x"}), "structure"},
	} {
		if _, _, err := Load(c.secret, time.Now()); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: got %v", c.name, err)
		}
	}
	// A CA outside its validity window fails "CA not expired".
	if _, checks, err := Load(p.SecretKey(), time.Now().AddDate(20, 0, 0)); err == nil || checks[1].OK {
		t.Error("expired CA must fail")
	}
}

func TestDeriveSNIMatchesTS(t *testing.T) {
	p := testpki.New()
	var ts string
	nodefixture.Run(t, map[string]any{"op": "deriveSni", "ca": p.CACertPem, "jwt": p.JWTPublicPem}, &ts)
	if got := DeriveSNI(p.CACertPem, p.JWTPublicPem); got != ts {
		t.Fatalf("DeriveSNI = %q, TS deriveSni = %q", got, ts)
	}
}

func TestNormalizePemMatchesTS(t *testing.T) {
	p := testpki.New()
	inputs := []string{
		p.CACertPem,
		strings.ReplaceAll(p.CACertPem, "\n", `\n`),                      // escaped newlines (env files)
		strings.ReplaceAll(p.CACertPem, "\n", "\r\n"),                    // CRLF
		strings.ReplaceAll(p.NodeKeyPem, "\n", ""),                       // one line
		"  " + strings.ReplaceAll(p.JWTPublicPem, "\n", "\n\n\n") + "  ", // blank lines, padding
	}
	var ts []string
	nodefixture.Run(t, map[string]any{"op": "normalizePem", "values": inputs}, &ts)
	for i, in := range inputs {
		if got := NormalizePem(in); got != ts[i] {
			t.Errorf("case %d: Go %q != TS %q", i, got, ts[i])
		}
	}
}
