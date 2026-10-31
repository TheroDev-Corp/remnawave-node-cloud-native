package api

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/testpki"
)

type env struct {
	pki  *testpki.PKI
	mat  *secretkey.Material
	addr string
}

func start(t *testing.T, sni bool) *env {
	t.Helper()
	p := testpki.New()
	mat, _, err := secretkey.Load(p.SecretKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := NewRouter(log, NewJWTVerifier(mat.JWTKey), "internal-token")
	rt.Handle("POST", "/node/echo", AuthJWT, func(r *Request) {
		var v any
		if err := r.Decode(&v); err != nil {
			r.Error(400, err.Error(), "E000")
			return
		}
		r.OK(map[string]any{"got": v, "len": len(r.Body)})
	})
	rt.Handle("GET", "/node/big", AuthJWT, func(r *Request) { r.OK(strings.Repeat("x", 4096)) })
	rt.Handle("GET", "/node/health/live", AuthNone, func(r *Request) { r.JSON(200, map[string]any{"status": "ok"}) })
	rt.Handle("GET", "/internal/get-config", AuthToken, func(r *Request) { r.JSON(200, map[string]any{}) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", rt, TLSConfig(mat, sni), log)
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return &env{pki: p, mat: mat, addr: ln.Addr().String()}
}

type reqOpts struct {
	tls      *tls.Config
	jwt      string
	noJWT    bool
	body     []byte
	encoding string
	ae       string
}

// do returns (resp, body, err); a dropped connection or failed handshake is an error.
func (e *env) do(t *testing.T, method, path string, o reqOpts) (*http.Response, []byte, error) {
	t.Helper()
	cfg := o.tls
	if cfg == nil {
		cfg = e.pki.ClientTLS(e.mat.SNI)
	}
	tr := &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: false}
	var rd io.Reader
	if o.body != nil {
		rd = bytes.NewReader(o.body)
	}
	req, _ := http.NewRequest(method, "https://"+e.addr+path, rd)
	if o.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.encoding != "" {
		req.Header.Set("Content-Encoding", o.encoding)
	}
	if o.ae != "" {
		req.Header.Set("Accept-Encoding", o.ae)
	}
	if !o.noJWT {
		j := o.jwt
		if j == "" {
			j = e.pki.PanelJWT()
		}
		req.Header.Set("Authorization", "Bearer "+j)
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

func TestEnvelopeStatusAndHeaders(t *testing.T) {
	e := start(t, false)
	resp, body, err := e.do(t, "POST", "/node/echo", reqOpts{body: []byte(`{"reset":true,"n":12345678901234567}`)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("POST answers 201 like Nest, got %d", resp.StatusCode)
	}
	if string(body) != `{"response":{"got":{"n":12345678901234567,"reset":true},"len":36}}` {
		t.Errorf("body = %s", body)
	}
	for k, want := range map[string]string{
		"Content-Type": "application/json; charset=utf-8", "X-Frame-Options": "SAMEORIGIN",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains", "X-Content-Type-Options": "nosniff",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(resp.Header.Get("ETag"), `W/"`) {
		t.Errorf("missing Express weak ETag: %q", resp.Header.Get("ETag"))
	}
	// Trailing slash and case-insensitive paths match like Express.
	if resp, _, err := e.do(t, "POST", "/NODE/Echo/", reqOpts{body: []byte(`{}`)}); err != nil || resp.StatusCode != 201 {
		t.Errorf("case/trailing slash: %v %v", resp, err)
	}
}

func TestGzipResponses(t *testing.T) {
	e := start(t, false)
	resp, body, err := e.do(t, "GET", "/node/big", reqOpts{ae: "gzip, compress, deflate, br"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("responses ≥ 1 KiB are gzipped for axios, got %q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.HasPrefix(plain, []byte(`{"response":"xxxx`)) {
		t.Errorf("decompressed = %.40s", plain)
	}
	if resp, _, _ := e.do(t, "GET", "/node/big", reqOpts{}); resp.Header.Get("Content-Encoding") != "" {
		t.Error("no gzip without Accept-Encoding")
	}
}

func TestAuthDropsLikeTheTSNode(t *testing.T) {
	e := start(t, false)
	expired := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"role": "API", "exp": time.Now().Add(-time.Minute).Unix()})
		s, _ := tok.SignedString(e.pki.JWTKey)
		return s
	}()
	hs256 := func() string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": "API"}).SignedString([]byte("x"))
		return s
	}()
	foreign := testpki.New().PanelJWT()
	for _, c := range []struct {
		name string
		o    reqOpts
		path string
	}{
		{"no token", reqOpts{noJWT: true, body: []byte(`{}`)}, "/node/echo"},
		{"expired token", reqOpts{jwt: expired, body: []byte(`{}`)}, "/node/echo"},
		{"HS256 token", reqOpts{jwt: hs256, body: []byte(`{}`)}, "/node/echo"},
		{"token from another panel", reqOpts{jwt: foreign, body: []byte(`{}`)}, "/node/echo"},
		{"unknown route", reqOpts{}, "/node/nope"},
		{"wrong method", reqOpts{}, "/node/echo"},
		{"internal route without token", reqOpts{}, "/internal/get-config"},
		{"internal route, wrong token", reqOpts{}, "/internal/get-config?token=nope"},
	} {
		t.Run(c.name, func(t *testing.T) {
			method := "POST"
			if c.o.body == nil {
				method = "GET"
			}
			if resp, _, err := e.do(t, method, c.path, c.o); err == nil {
				t.Fatalf("expected the connection to be dropped, got %d", resp.StatusCode)
			}
		})
	}
	// Health needs mTLS but no JWT; internal routes need only the token.
	if resp, _, err := e.do(t, "GET", "/node/health/live", reqOpts{noJWT: true}); err != nil || resp.StatusCode != 200 {
		t.Errorf("health: %v %v", resp, err)
	}
	if resp, _, err := e.do(t, "GET", "/internal/get-config?token=internal-token", reqOpts{noJWT: true}); err != nil || resp.StatusCode != 200 {
		t.Errorf("internal: %v %v", resp, err)
	}
}

func TestMTLSRules(t *testing.T) {
	e := start(t, false)
	noCert := e.pki.ClientTLS("")
	noCert.Certificates = nil
	otherCA := testpki.New().ClientTLS("")
	tls12 := e.pki.ClientTLS("")
	tls12.MinVersion, tls12.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	for name, cfg := range map[string]*tls.Config{"no client cert": noCert, "client cert from another CA": otherCA, "TLS 1.2": tls12} {
		t.Run(name, func(t *testing.T) {
			if resp, _, err := e.do(t, "GET", "/node/health/live", reqOpts{tls: cfg, noJWT: true}); err == nil {
				t.Fatalf("handshake should fail, got %d", resp.StatusCode)
			}
		})
	}
	// Without SNI_VERIFICATION any SNI (or none) is fine.
	if _, _, err := e.do(t, "GET", "/node/health/live", reqOpts{tls: e.pki.ClientTLS("whatever.example"), noJWT: true}); err != nil {
		t.Errorf("SNI must be ignored when verification is off: %v", err)
	}
}

func TestSNIVerification(t *testing.T) {
	e := start(t, true)
	if _, _, err := e.do(t, "GET", "/node/health/live", reqOpts{tls: e.pki.ClientTLS(e.mat.SNI), noJWT: true}); err != nil {
		t.Fatalf("derived SNI must pass: %v", err)
	}
	for name, sni := range map[string]string{"wrong SNI": "abc.def.com", "no SNI": ""} {
		if _, _, err := e.do(t, "GET", "/node/health/live", reqOpts{tls: e.pki.ClientTLS(sni), noJWT: true}); err == nil {
			t.Errorf("%s must fail the handshake", name)
		}
	}
}

// The panel compresses xray/start and bulk user bodies with zstd, windowLog
// 25 (32 MiB) and long-distance matching. A body whose repeats are further
// apart than the default 8 MiB window proves the decoder accepts it.
func TestPanelZstdBodies(t *testing.T) {
	e := start(t, false)
	dir := t.TempDir()
	chunk := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 300_000) // ~10.8 MB, repeated below
	users := []string{chunk, chunk, chunk}
	raw, _ := json.Marshal(map[string]any{"users": users})
	in, out := filepath.Join(dir, "body.json"), filepath.Join(dir, "body.zst")
	if err := os.WriteFile(in, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var info map[string]int
	nodefixture.Run(t, map[string]any{"op": "zstd", "in": in, "out": out}, &info)
	z, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the frame really needs more than an 8 MiB window.
	small, _ := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(8<<20))
	if _, err := small.DecodeAll(z, nil); err == nil {
		t.Fatal("test body does not exercise a >8 MiB window")
	}
	small.Close()
	resp, body, err := e.do(t, "POST", "/node/echo", reqOpts{body: z, encoding: "zstd"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 || !bytes.Contains(body, []byte(`"len":`+itoa(len(raw)))) {
		t.Fatalf("status %d, body %.120s (raw %d, zstd %d)", resp.StatusCode, body, info["raw"], info["compressed"])
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
