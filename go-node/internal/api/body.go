package api

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// BodyLimit is bodyParser.json({ limit: '1000mb' }), applied to the decoded body.
const BodyLimit = 1000 << 20

// zstdMaxWindow covers the panel's windowLog 25 (32 MiB) with room to spare.
const zstdMaxWindow = 1 << 28

type bodyError struct {
	status int
	msg    string
}

func (e *bodyError) Error() string { return e.msg }

// readBody is @kastov/body-parser-with-zstd's json parser: decode
// Content-Encoding (identity, gzip, deflate, br, zstd), enforce the limit,
// parse JSON when Content-Type is JSON. It returns the raw JSON bytes (or nil
// when there is no JSON body); handlers unmarshal into their own types.
func readBody(r *http.Request) (json.RawMessage, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	isJSON := ct == "application/json" || strings.HasSuffix(ct, "+json")
	if !isJSON {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, BodyLimit))
		return nil, nil
	}

	var rd io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, &bodyError{400, "invalid gzip body"}
		}
		defer zr.Close()
		rd = zr
	case "deflate":
		buf, _ := io.ReadAll(io.LimitReader(r.Body, BodyLimit+1))
		if zr, err := zlib.NewReader(bytes.NewReader(buf)); err == nil {
			rd = zr
		} else {
			rd = flate.NewReader(bytes.NewReader(buf))
		}
	case "br":
		rd = brotli.NewReader(r.Body)
	case "zstd":
		zr, err := zstd.NewReader(r.Body, zstd.WithDecoderMaxWindow(zstdMaxWindow), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, &bodyError{400, "invalid zstd body"}
		}
		defer zr.Close()
		rd = zr
	default:
		return nil, &bodyError{415, `unsupported content encoding "` + enc + `"`}
	}

	data, err := io.ReadAll(io.LimitReader(rd, BodyLimit+1))
	if err != nil {
		return nil, &bodyError{400, "request body could not be decoded: " + err.Error()}
	}
	if len(data) > BodyLimit {
		return nil, &bodyError{413, "request entity too large"}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	if !json.Valid(data) {
		return nil, &bodyError{400, "Unexpected token in JSON"}
	}
	// body-parser's strict mode only accepts objects and arrays.
	if t := bytes.TrimSpace(data)[0]; t != '{' && t != '[' {
		return nil, &bodyError{400, "Unexpected token in JSON at position 0"}
	}
	return data, nil
}
