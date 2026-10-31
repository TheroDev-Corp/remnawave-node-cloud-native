// Package secretkey decodes and checks SECRET_KEY exactly like
// src/common/utils/decode-node-payload in the TS node.
package secretkey

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Payload is base64(JSON{nodeCertPem, nodeKeyPem, caCertPem, jwtPublicKey}).
type Payload struct {
	CACertPem    string `json:"caCertPem"`
	JWTPublicKey string `json:"jwtPublicKey"`
	NodeCertPem  string `json:"nodeCertPem"`
	NodeKeyPem   string `json:"nodeKeyPem"`
}

var (
	reBegin    = regexp.MustCompile(`(-----BEGIN [A-Z ]+-----)`)
	reEnd      = regexp.MustCompile(`(-----END [A-Z ]+-----)`)
	reNewlines = regexp.MustCompile(`\n+`)
)

// NormalizePem is normalizePem() from decode-node-payload.ts.
func NormalizePem(p string) string {
	p = strings.ReplaceAll(p, `\n`, "\n")
	p = strings.ReplaceAll(p, "\r\n", "\n")
	p = reBegin.ReplaceAllString(p, "$1\n")
	p = reEnd.ReplaceAllString(p, "\n$1")
	p = reNewlines.ReplaceAllString(p, "\n")
	return strings.TrimSpace(p)
}

// Decode parses SECRET_KEY without the integrity checks
// (parseNodePayloadFromConfigService).
func Decode(secret string) (*Payload, error) {
	raw, err := decodeBase64Lenient(secret)
	if err != nil {
		return nil, errors.New("SECRET_KEY contains invalid JSON")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errors.New("SECRET_KEY contains invalid JSON")
	}
	get := func(k string) (string, bool) { s, ok := obj[k].(string); return s, ok }
	ca, ok1 := get("caCertPem")
	jwtKey, ok2 := get("jwtPublicKey")
	cert, ok3 := get("nodeCertPem")
	key, ok4 := get("nodeKeyPem")
	if !(ok1 && ok2 && ok3 && ok4) {
		return nil, errors.New("Invalid SECRET_KEY payload structure")
	}
	return &Payload{
		CACertPem:    NormalizePem(ca),
		JWTPublicKey: NormalizePem(jwtKey),
		NodeCertPem:  NormalizePem(cert),
		NodeKeyPem:   NormalizePem(key),
	}, nil
}

// decodeBase64Lenient matches Buffer.from(s, 'base64'), which also accepts
// the URL alphabet and missing padding.
func decodeBase64Lenient(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("-", "+", "_", "/", "\n", "", "\r", "", " ", "").Replace(s)
	s = strings.TrimRight(s, "=")
	return base64.RawStdEncoding.DecodeString(s)
}

// Check is one row of the "SECRET_KEY OK / INVALID" report.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Material is the parsed key material the node runs with.
type Material struct {
	Payload  *Payload
	CA       *x509.Certificate
	CAPool   *x509.CertPool
	NodeCert tls.Certificate
	JWTKey   crypto.PublicKey
	SNI      string // deriveSni(); only enforced with SNI_VERIFICATION=true
}

// Load decodes SECRET_KEY and runs the same checks as
// validate-secret-key.util.ts. All checks are always reported; the error is
// non-nil if any failed.
func Load(secret string, now time.Time) (*Material, []Check, error) {
	p, err := Decode(secret)
	if err != nil {
		return nil, nil, err
	}
	var checks []Check
	add := func(name string, fn func() (string, error)) {
		d, err := fn()
		if err != nil {
			checks = append(checks, Check{name, false, firstLine(err.Error())})
			return
		}
		checks = append(checks, Check{name, true, d})
	}

	var ca, node *x509.Certificate
	add("CA parses", func() (string, error) {
		c, err := parseCert(p.CACertPem)
		if err != nil {
			return "", err
		}
		ca = c
		return fingerprint(c)[:24], nil
	})
	add("CA not expired", func() (string, error) {
		if ca == nil {
			return "", errors.New("CA unavailable")
		}
		if ca.NotBefore.After(now) {
			return "", errors.New("not yet valid")
		}
		if ca.NotAfter.Before(now) {
			return "", errors.New("expired")
		}
		return "until " + ca.NotAfter.UTC().Format("Jan _2 15:04:05 2006 GMT"), nil
	})
	add("CA self-signature", func() (string, error) {
		if ca == nil {
			return "", errors.New("CA unavailable")
		}
		if err := ca.CheckSignature(ca.SignatureAlgorithm, ca.RawTBSCertificate, ca.Signature); err != nil {
			return "", errors.New("mismatch – corrupted")
		}
		return "valid", nil
	})
	add("node cert parses", func() (string, error) {
		c, err := parseCert(p.NodeCertPem)
		if err != nil {
			return "", err
		}
		node = c
		return fingerprint(c)[:24], nil
	})
	add("node signed by CA", func() (string, error) {
		if ca == nil || node == nil {
			return "", errors.New("cert unavailable")
		}
		if err := ca.CheckSignature(node.SignatureAlgorithm, node.RawTBSCertificate, node.Signature); err != nil {
			return "", errors.New("not signed by this CA")
		}
		return "valid", nil
	})
	var nodeTLS tls.Certificate
	add("node key matches cert", func() (string, error) {
		if node == nil {
			return "", errors.New("cert unavailable")
		}
		c, err := tls.X509KeyPair([]byte(p.NodeCertPem), []byte(p.NodeKeyPem))
		if err != nil {
			return "", errors.New("key does not match cert")
		}
		nodeTLS = c
		return "valid", nil
	})
	var jwtKey crypto.PublicKey
	add("jwt public key", func() (string, error) {
		k, err := parsePublicKey(p.JWTPublicKey)
		if err != nil {
			return "", err
		}
		jwtKey = k
		return "ok", nil
	})

	for _, c := range checks {
		if !c.OK {
			return nil, checks, errors.New("SECRET_KEY payload validation failed. Double check your SECRET_KEY.")
		}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &Material{
		Payload: p, CA: ca, CAPool: pool, NodeCert: nodeTLS, JWTKey: jwtKey,
		SNI: DeriveSNI(p.CACertPem, p.JWTPublicKey),
	}, checks, nil
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

func parseCert(p string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return nil, errors.New("error:1E08010C:DECODER routines::unsupported")
	}
	return x509.ParseCertificate(b.Bytes)
}

// fingerprint is X509Certificate.fingerprint256: colon-separated upper hex.
func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// parsePublicKey accepts what crypto.createPublicKey accepts in practice: an
// SPKI or PKCS#1 public key, or a certificate.
func parsePublicKey(p string) (crypto.PublicKey, error) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return nil, errors.New("error:1E08010C:DECODER routines::unsupported")
	}
	switch b.Type {
	case "RSA PUBLIC KEY":
		return x509.ParsePKCS1PublicKey(b.Bytes)
	case "CERTIFICATE":
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		return c.PublicKey, nil
	default:
		k, err := x509.ParsePKIXPublicKey(b.Bytes)
		if err != nil {
			return nil, err
		}
		switch k.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			return k, nil
		}
		return nil, fmt.Errorf("unsupported key type %T", k)
	}
}

var (
	rePemArmor = regexp.MustCompile(`-----[^-]+-----`)
	reNonB64   = regexp.MustCompile(`[^A-Za-z0-9+/=]`)
)

var tlds = []string{"com", "net", "org", "io", "dev", "app"}

// DeriveSNI is deriveSni() from decode-servername.util.ts:
// HKDF-SHA256(canon(jwtPub) || canon(ca), salt="", info="rw-v1", 22 bytes).
func DeriveSNI(caCertPem, jwtPublicKey string) string {
	canon := func(p string) string { return reNonB64.ReplaceAllString(rePemArmor.ReplaceAllString(p, ""), "") }
	ikm := append([]byte(canon(jwtPublicKey)), canon(caCertPem)...)
	okm, err := hkdf.Key(sha256.New, ikm, nil, "rw-v1", 22)
	if err != nil {
		panic(err) // only for invalid lengths
	}
	return hex.EncodeToString(okm[:16]) + "." + hex.EncodeToString(okm[16:21]) + "." + tlds[int(okm[21])%len(tlds)]
}

// SNIMatches is makeSniVerifier(): constant-time compare, empty never matches.
func (m *Material) SNIMatches(serverName string) bool {
	if serverName == "" || len(serverName) != len(m.SNI) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(serverName), []byte(m.SNI)) == 1
}

// Report renders the checks the way the TS node logs them (plain text).
func Report(checks []Check, sni string) string {
	ok := true
	w := 0
	for _, c := range checks {
		ok = ok && c.OK
		w = max(w, len(c.Name))
	}
	var b bytes.Buffer
	if ok {
		b.WriteString("SECRET_KEY OK\n")
	} else {
		b.WriteString("SECRET_KEY INVALID\n")
	}
	for _, c := range checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		fmt.Fprintf(&b, "  %s %-*s  %s\n", mark, w, c.Name, c.Detail)
	}
	if ok {
		b.WriteString("  " + sni + "\n")
	}
	return b.String()
}
