// Package api is the node's HTTPS API on NODE_PORT: the Express/Nest stack of
// src/main.ts (mTLS, body parsing, helmet, compression, the /node prefix,
// JWT guard, socket drop on unknown routes) without the framework.
package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
)

// Auth is what a route requires on top of mTLS.
type Auth int

const (
	AuthNone  Auth = iota // e.g. /node/health/*
	AuthJWT               // JwtDefaultGuard: RS256 bearer, drop on failure
	AuthToken             // TokenAuthMiddleware: ?token=INTERNAL_REST_TOKEN, drop on failure
)

// Handler gets the decoded request body (nil for bodiless requests) and
// writes the response through the Request helpers.
type Handler func(r *Request)

type route struct {
	auth Auth
	h    Handler
}

// Router maps "METHOD /path" to handlers. Express matching is
// case-insensitive and tolerates one trailing slash; anything unmatched hits
// NotFoundExceptionFilter, which destroys the socket.
type Router struct {
	routes map[string]route
	log    *slog.Logger
	jwt    *JWTVerifier
	token  string
}

func NewRouter(log *slog.Logger, jwt *JWTVerifier, internalToken string) *Router {
	return &Router{routes: map[string]route{}, log: log, jwt: jwt, token: internalToken}
}

func routeKey(method, path string) string {
	p := strings.ToLower(path)
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return method + " " + p
}

func (rt *Router) Handle(method, path string, auth Auth, h Handler) {
	rt.routes[routeKey(method, path)] = route{auth: auth, h: h}
	if method == http.MethodGet { // Express app.get also answers HEAD
		rt.routes[routeKey(http.MethodHead, path)] = route{auth: auth, h: h}
	}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setHelmetHeaders(w.Header())

	// body-parser runs before routing, like app.use(bodyParser.json(...)).
	body, err := readBody(r)
	if err != nil {
		var be *bodyError
		if errors.As(err, &be) {
			writeJSONRaw(w, r, be.status, map[string]any{"message": be.msg, "error": http.StatusText(be.status), "statusCode": be.status})
			return
		}
		Drop(w)
		return
	}

	rt2, ok := rt.routes[routeKey(r.Method, r.URL.Path)]
	if !ok {
		Drop(w)
		return
	}
	switch rt2.auth {
	case AuthJWT:
		if err := rt.jwt.Verify(r.Header.Get("Authorization")); err != nil {
			rt.log.Error(fmt.Sprintf("Incorrect SECRET_KEY or JWT! Request dropped. URL: %s, IP: %s", r.URL.RequestURI(), remoteIP(r)))
			Drop(w)
			return
		}
	case AuthToken:
		if t := r.URL.Query().Get("token"); t == "" || rt.token == "" || t != rt.token {
			Drop(w)
			return
		}
	}
	rt2.h(&Request{W: w, R: r, Body: body, log: rt.log})
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return "::ffff:" + h
}

// Drop is res.socket.destroy(): close the connection without a response.
func Drop(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
		return
	}
	panic(http.ErrAbortHandler)
}

// TLSConfig is the httpsOptions from src/main.ts: TLS 1.3 only, client cert
// required and verified against the panel CA, and with SNI_VERIFICATION the
// SNI must equal the HKDF-derived name (handshake fails otherwise).
func TLSConfig(m *secretkey.Material, sniVerification bool) *tls.Config {
	c := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.NodeCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    m.CAPool,
		NextProtos:   []string{"http/1.1"}, // the Node https server has no HTTP/2
	}
	if sniVerification {
		c.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			if !m.SNIMatches(h.ServerName) {
				return nil, errors.New("unknown sni")
			}
			return nil, nil
		}
	}
	return c
}

// NewServer wires the router into an HTTPS server with the TS node's
// keep-alive (60 s) and header (61 s) timeouts.
func NewServer(addr string, h http.Handler, tlsCfg *tls.Config, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsCfg,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // no h2
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 61 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
}
