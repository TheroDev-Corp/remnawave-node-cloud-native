package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	if len(body) >= compressionThreshold && acceptsGzip(r.Header.Get("Accept-Encoding")) && status != http.StatusNoContent {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.DefaultCompression)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = b.Bytes()
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		fields := strings.Split(part, ";")
		enc := strings.ToLower(strings.TrimSpace(fields[0]))
		if enc != "gzip" && enc != "x-gzip" && enc != "*" {
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.TrimSpace(k) == "q" {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		if q > 0 {
			return true
		}
	}
	return false
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
