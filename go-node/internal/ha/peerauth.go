package ha

import (
	"bufio"
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Peer requests are authenticated without ever sending SECRET_KEY.
//
// The TS fork puts the whole SECRET_KEY (node private key, CA, JWT key) in an
// X-Secret-Key header over plain HTTP. Here every request carries
//
//	X-Rw-Peer: v1 ts=<unix seconds> sig=<hex HMAC-SHA256>
//
// with the HMAC over "v1\n<method>\n<path>\n<ts>\n<hex sha256(body)>" under a
// key derived from SECRET_KEY with HKDF ("rw-peer-v1"). Requests older or
// newer than PeerAuthWindow, or replayed within it, are refused. On top of
// that the peer port speaks TLS with the node certificate from SECRET_KEY,
// so replicated user credentials are not readable on the pod network.

const peerHeader = "X-Rw-Peer"

var PeerAuthWindow = 30 * time.Second

type PeerAuth struct {
	key []byte
	now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// NewPeerAuth derives the HMAC key from SECRET_KEY. The TS fork's
// X-Secret-Key header is never sent nor accepted.
func NewPeerAuth(secretKey string) *PeerAuth {
	key, err := hkdf.Key(sha256.New, []byte(secretKey), []byte("remnawave-node-peer"), "rw-peer-v1", 32)
	if err != nil {
		panic(err)
	}
	return &PeerAuth{key: key, now: time.Now, seen: map[string]time.Time{}}
}

func (a *PeerAuth) sign(method, path string, ts int64, body []byte) string {
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, a.key)
	fmt.Fprintf(m, "v1\n%s\n%s\n%d\n%s", method, path, ts, hex.EncodeToString(sum[:]))
	return hex.EncodeToString(m.Sum(nil))
}

// Sign sets the auth header on an outgoing peer request.
func (a *PeerAuth) Sign(req *http.Request, body []byte) {
	ts := a.now().Unix()
	req.Header.Set(peerHeader, fmt.Sprintf("v1 ts=%d sig=%s", ts, a.sign(req.Method, req.URL.RequestURI(), ts, body)))
}

// Verify checks an incoming peer request and returns its body.
func (a *PeerAuth) Verify(r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // express.json({limit:'50mb'})
	if err != nil {
		return nil, false
	}
	var ts int64
	var sig string
	for _, f := range strings.Fields(strings.TrimPrefix(r.Header.Get(peerHeader), "v1 ")) {
		if v, ok := strings.CutPrefix(f, "ts="); ok {
			ts, _ = strconv.ParseInt(v, 10, 64)
		} else if v, ok := strings.CutPrefix(f, "sig="); ok {
			sig = v
		}
	}
	if !strings.HasPrefix(r.Header.Get(peerHeader), "v1 ") || ts == 0 || sig == "" {
		return nil, false
	}
	now := a.now()
	if d := now.Sub(time.Unix(ts, 0)); d > PeerAuthWindow || d < -PeerAuthWindow {
		return nil, false
	}
	want := a.sign(r.Method, r.URL.RequestURI(), ts, body)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, t := range a.seen {
		if now.Sub(t) > 2*PeerAuthWindow {
			delete(a.seen, k)
		}
	}
	if _, dup := a.seen[sig]; dup {
		return nil, false
	}
	a.seen[sig] = now
	return body, true
}

// PeerTLS holds the node certificate (server side) and the CA pool peers
// check it against (client side). Every pod has the same SECRET_KEY, so a
// peer proves it holds the node key by completing the handshake.
type PeerTLS struct {
	Cert tls.Certificate
	CA   *x509.CertPool
}

func (p *PeerTLS) server() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{p.Cert}, MinVersion: tls.VersionTLS13}
}

func (p *PeerTLS) client() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // the chain is checked below; pod IPs aren't in the cert
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return fmt.Errorf("peer sent no certificate")
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = cert.Verify(x509.VerifyOptions{Roots: p.CA, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
			return err
		},
	}
}

// MuxListener serves TLS (peers) and plain HTTP (kubelet probes) on one
// port, telling them apart by the first byte (0x16 = TLS handshake).
func (p *PeerTLS) MuxListener(ln net.Listener) net.Listener {
	return &muxListener{Listener: ln, cfg: p.server()}
}

type muxListener struct {
	net.Listener
	cfg *tls.Config
}

func (m *muxListener) Accept() (net.Conn, error) {
	c, err := m.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc := &peekConn{Conn: c, r: bufio.NewReader(c)}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := pc.r.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return pc, nil // let the HTTP server deal with it
	}
	if b[0] == 0x16 {
		return tls.Server(pc, m.cfg), nil
	}
	return pc, nil
}

type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// peerClient sends signed requests over TLS.
type peerClient struct {
	auth *PeerAuth
	http *http.Client
}

func newPeerClient(auth *PeerAuth, tlsCfg *PeerTLS) *peerClient {
	tr := &http.Transport{TLSClientConfig: tlsCfg.client(), MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	return &peerClient{auth: auth, http: &http.Client{Transport: tr}}
}

func (c *peerClient) do(req *http.Request, body []byte) (*http.Response, error) {
	c.auth.Sign(req, body)
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}
