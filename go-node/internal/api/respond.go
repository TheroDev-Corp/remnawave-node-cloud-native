package api

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
)

// Request is one routed request.
type Request struct {
	W    http.ResponseWriter
	R    *http.Request
	Body json.RawMessage
	log  *slog.Logger
}

// Decode unmarshals the body into v; a missing body decodes as {} (what
// Nest hands a handler when body-parser had nothing to parse).
func (r *Request) Decode(v any) error {
	if len(r.Body) == 0 {
		return json.Unmarshal([]byte("{}"), v)
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	return dec.Decode(v)
}

// OK writes {"response": v} with Nest's default status: 201 for POST, 200 otherwise.
func (r *Request) OK(v any) {
	status := http.StatusOK
	if r.R.Method == http.MethodPost {
		status = http.StatusCreated
	}
	r.JSON(status, map[string]any{"response": v})
}

// JSON writes v as-is (no envelope) with the given status.
func (r *Request) JSON(status int, v any) { writeJSONRaw(r.W, r.R, status, v) }

// Error writes HttpExceptionFilter's body: {timestamp, path, message, errorCode}.
func (r *Request) Error(status int, message, errorCode string) {
	if status == http.StatusForbidden {
		message = "Forbidden"
	}
	r.JSON(status, map[string]any{
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"path":      r.R.URL.RequestURI(),
		"message":   message,
		"errorCode": errorCode,
	})
}

// ValidationError is the nestjs-zod ZodValidationPipe answer (400).
func (r *Request) ValidationError(issues []map[string]any) {
	r.JSON(http.StatusBadRequest, map[string]any{
		"statusCode": 400,
		"message":    "Validation failed",
		"errors":     issues,
	})
}

// Empty writes a bodiless response (e.g. @HttpCode(200) on a void handler).
func (r *Request) Empty(status int) {
	r.W.Header().Set("Content-Length", "0")
	r.W.WriteHeader(status)
}

func (r *Request) Log() *slog.Logger { return r.log }

func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // JSON.stringify does not escape <, >, &
	if err := enc.Encode(v); err != nil {
		return []byte(`{}`)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// writeJSONRaw is res.status(s).json(v) behind compression() and Express'
// weak ETag.
func writeJSONRaw(w http.ResponseWriter, r *http.Request, status int, v any) {
	send(w, r, status, "application/json; charset=utf-8", marshal(v))
}

// compressionThreshold is compression()'s default 1kb threshold.
const compressionThreshold = 1024

func send(w http.ResponseWriter, r *http.Request, status int, ctype string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("ETag", weakETag(body))
	h.Add("Vary", "Accept-Encoding")
	// compression(): HEAD and short bodies stay plain; otherwise the
	// negotiated encoding, which is br for axios' "gzip, compress, deflate, br".
	if len(body) >= compressionThreshold && r.Method != http.MethodHead && status != http.StatusNoContent && r.Header.Get("Accept-Encoding") != "" {
		if enc := NegotiateEncoding(r.Header.Get("Accept-Encoding")); enc != "" && enc != "identity" {
			body = compress(enc, body)
			h.Set("Content-Encoding", enc)
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func compress(enc string, body []byte) []byte {
	var b bytes.Buffer
	var wc io.WriteCloser
	switch enc {
	case "br":
		wc = brotli.NewWriterLevel(&b, 4) // compression() sets BROTLI_PARAM_QUALITY 4
	case "gzip":
		wc, _ = gzip.NewWriterLevel(&b, gzip.DefaultCompression)
	case "deflate":
		wc, _ = zlib.NewWriterLevel(&b, zlib.DefaultCompression)
	default:
		return body
	}
	_, _ = wc.Write(body)
	_ = wc.Close()
	return b.Bytes()
}

// compression@1.8.1 negotiates among these, preferring br then gzip on ties.
var (
	supportedEncodings = []string{"br", "gzip", "deflate", "identity"}
	preferredEncodings = []string{"br", "gzip"}
	reEncoding         = regexp.MustCompile(`^\s*([^\s;]+)\s*(?:;(.*))?$`)
	reJSFloat          = regexp.MustCompile(`^[+-]?(?:Infinity|\d+\.?\d*(?:[eE][+-]?\d+)?|\.\d+(?:[eE][+-]?\d+)?)`)
)

// jsParseFloat is JavaScript's parseFloat: the longest numeric prefix, NaN otherwise.
func jsParseFloat(s string) float64 {
	m := reJSFloat.FindString(strings.TrimLeft(s, " \t\n\r\v\f"))
	if m == "" {
		return math.NaN()
	}
	if strings.HasSuffix(m, "Infinity") {
		if strings.HasPrefix(m, "-") {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

type encodingSpec struct {
	enc  string
	q    float64
	i, o int
	s    int
}

// NegotiateEncoding is negotiator@0.6.4's encoding(supported, preferred),
// the algorithm compression() uses. "" means nothing acceptable.
func NegotiateEncoding(accept string) string {
	var accepts []encodingSpec
	hasIdentity, minQ := false, 1.0
	parts := strings.Split(accept, ",")
	for i, p := range parts {
		m := reEncoding.FindStringSubmatch(strings.TrimSpace(p))
		if m == nil {
			continue
		}
		q := 1.0
		if m[2] != "" {
			for _, param := range strings.Split(m[2], ";") {
				kv := strings.Split(strings.TrimSpace(param), "=")
				if kv[0] == "q" {
					v := ""
					if len(kv) > 1 {
						v = kv[1]
					}
					q = jsParseFloat(v)
					break
				}
			}
		}
		spec := encodingSpec{enc: m[1], q: q, i: i}
		accepts = append(accepts, spec)
		if strings.EqualFold(spec.enc, "identity") || spec.enc == "*" {
			hasIdentity = true
		}
		qq := q
		if qq == 0 || math.IsNaN(qq) { // `encoding.q || 1`
			qq = 1
		}
		minQ = math.Min(minQ, qq)
	}
	if !hasIdentity {
		accepts = append(accepts, encodingSpec{enc: "identity", q: minQ, i: len(parts)})
	}
	var prios []encodingSpec
	for idx, enc := range supportedEncodings {
		best := encodingSpec{enc: enc, o: -1}
		for _, a := range accepts {
			s := 0
			if strings.EqualFold(a.enc, enc) {
				s = 1
			} else if a.enc != "*" {
				continue
			}
			spec := encodingSpec{enc: enc, i: idx, o: a.i, q: a.q, s: s}
			// (priority.s - spec.s || priority.q - spec.q || priority.o - spec.o) < 0,
			// where || skips 0 and NaN.
			d := float64(best.s - spec.s)
			if d == 0 {
				if dq := best.q - spec.q; dq != 0 && !math.IsNaN(dq) {
					d = dq
				} else {
					d = float64(best.o - spec.o)
				}
			}
			if d < 0 {
				best = spec
			}
		}
		if best.q > 0 {
			prios = append(prios, best)
		}
	}
	pref := func(e string) int {
		for i, p := range preferredEncodings {
			if p == e {
				return i
			}
		}
		return -1
	}
	sort.SliceStable(prios, func(x, y int) bool {
		a, b := prios[x], prios[y]
		if a.q != b.q {
			return a.q > b.q
		}
		ap, bp := pref(a.enc), pref(b.enc)
		switch {
		case ap == -1 && bp == -1:
			if a.s != b.s {
				return a.s > b.s
			}
			if a.o != b.o {
				return a.o < b.o
			}
			return a.i < b.i
		case ap != -1 && bp != -1:
			return ap < bp
		default:
			return ap != -1
		}
	})
	if len(prios) == 0 {
		return ""
	}
	return prios[0].enc
}

// weakETag is the `etag` package with weak=true:
// W/"<length hex>-<base64 sha1, 27 chars>".
func weakETag(body []byte) string {
	if len(body) == 0 {
		return `W/"0-2jmj7l5rSw0yVb/vlWAYkK/YBwk"`
	}
	sum := sha1.Sum(body)
	return fmt.Sprintf(`W/"%x-%s"`, len(body), base64.StdEncoding.EncodeToString(sum[:])[:27])
}

// setHelmetHeaders is helmet() with its v8 defaults (CSP included).
func setHelmetHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'self';base-uri 'self';font-src 'self' https: data:;form-action 'self';frame-ancestors 'self';img-src 'self' data:;object-src 'none';script-src 'self';script-src-attr 'none';style-src 'self' https: 'unsafe-inline';upgrade-insecure-requests")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Origin-Agent-Cluster", "?1")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Dns-Prefetch-Control", "off")
	h.Set("X-Download-Options", "noopen")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-Permitted-Cross-Domain-Policies", "none")
	h.Set("X-Xss-Protection", "0")
}
