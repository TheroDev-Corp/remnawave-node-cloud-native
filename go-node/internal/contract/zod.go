package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The request validator walks the exported JSON Schema the way zod parses:
// object keys in shape order, every key checked (issues are collected, not
// short-circuited), unknown keys stripped rather than rejected (plain
// z.object), and issues shaped like zod v4's (code, path array, message in
// zod's English locale). ZodValidationPipe puts exactly these into the 400
// body, so the panel sees the same errors from both nodes.

// omap is a JSON object that remembers its key order.
type omap struct {
	keys []string
	m    map[string]any
}

func (o *omap) get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.m[k]
	return v, ok
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &omap{m: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := o.m[k]; !dup {
					o.keys = append(o.keys, k)
				}
				o.m[k] = v
			}
			_, err := dec.Token()
			return o, err
		case '[':
			var a []any
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			_, err := dec.Token()
			if a == nil {
				a = []any{}
			}
			return a, err
		}
	}
	return tok, nil
}

// undefined stands for a missing key.
type undefinedT struct{}

var undefined = undefinedT{}

type Issue = map[string]any

func receivedType(v any) string {
	switch x := v.(type) {
	case undefinedT:
		return "undefined"
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		if f, err := x.Float64(); err == nil && f != f {
			return "nan"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// stringifyPrimitive is zod's util.stringifyPrimitive.
func stringifyPrimitive(v any) string {
	switch x := v.(type) {
	case string:
		b, _ := json.Marshal(x)
		return string(b)
	case nil:
		return "null"
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// formatNames is zod v4's English FormatDictionary.
var formatNames = map[string]string{
	"regex": "input", "email": "email address", "url": "URL", "emoji": "emoji", "uuid": "UUID",
	"uuidv4": "UUIDv4", "uuidv6": "UUIDv6", "nanoid": "nanoid", "guid": "GUID", "cuid": "cuid",
	"cuid2": "cuid2", "ulid": "ULID", "xid": "XID", "ksuid": "KSUID", "datetime": "ISO datetime",
	"date": "ISO date", "time": "ISO time", "duration": "ISO duration", "ipv4": "IPv4 address",
	"ipv6": "IPv6 address", "cidrv4": "IPv4 range", "cidrv6": "IPv6 range",
	"base64": "base64-encoded string", "base64url": "base64url-encoded string",
	"json_string": "JSON string", "e164": "E.164 number", "jwt": "JWT", "template_literal": "input",
}

var (
	reMu    sync.Mutex
	reCache = map[string]*regexp.Regexp{}
)

func compiled(p string) *regexp.Regexp {
	reMu.Lock()
	defer reMu.Unlock()
	if re, ok := reCache[p]; ok {
		return re
	}
	re, err := regexp.Compile(p)
	if err != nil {
		re = nil
	}
	reCache[p] = re
	return re
}

func withPath(path []any, seg any) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, seg)
}

func issue(path []any, fields Issue) Issue {
	if path == nil {
		path = []any{}
	}
	fields["path"] = path
	return fields
}

func invalidType(path []any, expected string, v any) Issue {
	return issue(path, Issue{"code": "invalid_type", "expected": expected,
		"message": "Invalid input: expected " + expected + ", received " + receivedType(v)})
}

func numberOf(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func equalJSON(a, b any) bool {
	fa, oka := numberOf(a)
	fb, okb := numberOf(b)
	if oka && okb {
		return fa == fb
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && receivedType(a) == receivedType(b)
}

// zodCheck validates v against schema s at path.
func zodCheck(s *omap, v any, path []any) []Issue {
	if s == nil || len(s.keys) == 0 {
		return nil // z.unknown() / z.any()
	}
	if raw, ok := s.get("anyOf"); ok {
		return checkUnion(raw.([]any), v, path)
	}
	if raw, ok := s.get("oneOf"); ok {
		opts := raw.([]any)
		if disc, values := discriminator(opts); disc != "" {
			obj, ok := v.(map[string]any)
			if !ok {
				return []Issue{invalidType(path, "object", v)}
			}
			for i, opt := range opts {
				if equalJSON(obj[disc], values[i]) {
					return zodCheck(opt.(*omap), v, path)
				}
			}
			quoted := make([]string, len(values))
			for i, val := range values {
				quoted[i] = "'" + fmt.Sprint(val) + "'"
			}
			return []Issue{issue(withPath(path, disc), Issue{"code": "invalid_union", "errors": []any{},
				"note": "No matching discriminator", "discriminator": disc, "options": values,
				"message": "Invalid discriminator value. Expected " + strings.Join(quoted, " | ")})}
		}
		return checkUnion(opts, v, path)
	}
	if c, ok := s.get("const"); ok {
		if equalJSON(v, c) {
			return nil
		}
		return []Issue{issue(path, Issue{"code": "invalid_value", "values": []any{c},
			"message": "Invalid input: expected " + stringifyPrimitive(c)})}
	}
	if raw, ok := s.get("enum"); ok {
		vals := raw.([]any)
		for _, e := range vals {
			if equalJSON(v, e) {
				return nil
			}
		}
		parts := make([]string, len(vals))
		for i, e := range vals {
			parts[i] = stringifyPrimitive(e)
		}
		return []Issue{issue(path, Issue{"code": "invalid_value", "values": vals,
			"message": "Invalid option: expected one of " + strings.Join(parts, "|")})}
	}

	typ, _ := s.get("type")
	if list, ok := typ.([]any); ok { // ["string","null"]: z.nullable
		var other string
		for _, t := range list {
			if t == "null" {
				if v == nil {
					return nil
				}
			} else {
				other = t.(string)
			}
		}
		typ = other
	}
	switch typ {
	case "object":
		return checkObject(s, v, path)
	case "array":
		a, ok := v.([]any)
		if !ok {
			return []Issue{invalidType(path, "array", v)}
		}
		var out []Issue
		if m, ok := s.get("minItems"); ok {
			if n, _ := numberOf(m); float64(len(a)) < n {
				out = append(out, issue(path, Issue{"origin": "array", "code": "too_small", "minimum": m, "inclusive": true,
					"message": "Too small: expected array to have >=" + stringifyPrimitive(m) + " items"}))
			}
		}
		if m, ok := s.get("maxItems"); ok {
			if n, _ := numberOf(m); float64(len(a)) > n {
				out = append(out, issue(path, Issue{"origin": "array", "code": "too_big", "maximum": m, "inclusive": true,
					"message": "Too big: expected array to have <=" + stringifyPrimitive(m) + " items"}))
			}
		}
		if items, ok := s.get("items"); ok {
			for i, e := range a {
				out = append(out, zodCheck(items.(*omap), e, withPath(path, i))...)
			}
		}
		return out
	case "string":
		str, ok := v.(string)
		if !ok {
			return []Issue{invalidType(path, "string", v)}
		}
		var out []Issue
		if m, ok := s.get("minLength"); ok {
			if n, _ := numberOf(m); float64(len([]rune(str))) < n {
				out = append(out, issue(path, Issue{"origin": "string", "code": "too_small", "minimum": m, "inclusive": true,
					"message": "Too small: expected string to have >=" + stringifyPrimitive(m) + " characters"}))
			}
		}
		if m, ok := s.get("maxLength"); ok {
			if n, _ := numberOf(m); float64(len([]rune(str))) > n {
				out = append(out, issue(path, Issue{"origin": "string", "code": "too_big", "maximum": m, "inclusive": true,
					"message": "Too big: expected string to have <=" + stringifyPrimitive(m) + " characters"}))
			}
		}
		if f, _ := s.get("format"); f == "ipv6" {
			// zod checks IPv6 with the URL parser, not a regex: no origin/pattern.
			if a, err := netip.ParseAddr(str); err != nil || !a.Is6() || a.Zone() != "" {
				out = append(out, issue(path, Issue{"code": "invalid_format", "format": "ipv6", "message": "Invalid IPv6 address"}))
			}
			return out
		}
		if p, ok := s.get("pattern"); ok {
			if re := compiled(p.(string)); re != nil && !re.MatchString(str) {
				format, _ := s.get("format")
				f, _ := format.(string)
				msg := "Invalid string: must match pattern /" + p.(string) + "/"
				if f == "" {
					f = "regex"
				} else if name, ok := formatNames[f]; ok {
					msg = "Invalid " + name
				} else {
					msg = "Invalid " + f
				}
				out = append(out, issue(path, Issue{"origin": "string", "code": "invalid_format", "format": f,
					"pattern": "/" + p.(string) + "/", "message": msg}))
			}
		}
		return out
	case "number", "integer":
		f, ok := numberOf(v)
		expected := "number"
		if typ == "integer" {
			expected = "int"
		}
		if !ok || (typ == "integer" && f != float64(int64(f))) {
			return []Issue{invalidType(path, expected, v)}
		}
		var out []Issue
		if m, ok := s.get("minimum"); ok {
			if n, _ := numberOf(m); f < n {
				out = append(out, issue(path, Issue{"origin": "number", "code": "too_small", "minimum": m, "inclusive": true,
					"message": "Too small: expected number to be >=" + stringifyPrimitive(m)}))
			}
		}
		if m, ok := s.get("maximum"); ok {
			if n, _ := numberOf(m); f > n {
				out = append(out, issue(path, Issue{"origin": "number", "code": "too_big", "maximum": m, "inclusive": true,
					"message": "Too big: expected number to be <=" + stringifyPrimitive(m)}))
			}
		}
		return out
	case "boolean":
		if _, ok := v.(bool); !ok {
			return []Issue{invalidType(path, "boolean", v)}
		}
	case "null":
		if v != nil {
			return []Issue{invalidType(path, "null", v)}
		}
	}
	return nil
}

func checkObject(s *omap, v any, path []any) []Issue {
	obj, ok := v.(map[string]any)
	if !ok {
		expected := "object"
		if _, isRecord := s.get("propertyNames"); isRecord {
			expected = "record"
		}
		return []Issue{invalidType(path, expected, v)}
	}
	var out []Issue
	required := map[string]bool{}
	if r, ok := s.get("required"); ok {
		for _, k := range r.([]any) {
			required[k.(string)] = true
		}
	}
	propsRaw, _ := s.get("properties")
	props, _ := propsRaw.(*omap)
	known := map[string]bool{}
	if props != nil {
		for _, k := range props.keys {
			known[k] = true
			ps := props.m[k].(*omap)
			val, present := obj[k]
			if !present {
				if _, hasDefault := ps.get("default"); hasDefault || !required[k] {
					continue
				}
				val = undefined
			}
			out = append(out, zodCheck(ps, val, withPath(path, k))...)
		}
	}
	// z.record: additionalProperties is a schema. additionalProperties:false
	// (plain z.object) strips unknown keys, it doesn't reject them.
	if ap, ok := s.get("additionalProperties"); ok {
		if aps, ok := ap.(*omap); ok {
			for k, val := range obj {
				if !known[k] {
					out = append(out, zodCheck(aps, val, withPath(path, k))...)
				}
			}
		}
	}
	return out
}

// checkUnion is z.union (and z.nullable as anyOf [X, null]).
func checkUnion(opts []any, v any, path []any) []Issue {
	if len(opts) == 2 {
		for i, o := range opts {
			if t, _ := o.(*omap).get("type"); t == "null" {
				if v == nil {
					return nil
				}
				return zodCheck(opts[1-i].(*omap), v, path)
			}
		}
	}
	var all []any
	for _, o := range opts {
		is := zodCheck(o.(*omap), v, nil)
		if len(is) == 0 {
			return nil
		}
		all = append(all, is)
	}
	return []Issue{issue(path, Issue{"code": "invalid_union", "errors": all, "message": "Invalid input"})}
}

// discriminator finds z.discriminatedUnion's key: a property every option
// pins with const.
func discriminator(opts []any) (string, []any) {
	first, ok := opts[0].(*omap)
	if !ok {
		return "", nil
	}
	pr, _ := first.get("properties")
	props, _ := pr.(*omap)
	if props == nil {
		return "", nil
	}
	for _, key := range props.keys {
		var values []any
		for _, o := range opts {
			op, _ := o.(*omap).get("properties")
			opm, _ := op.(*omap)
			ks, _ := opm.get(key)
			kso, _ := ks.(*omap)
			c, ok := kso.get("const")
			if !ok {
				values = nil
				break
			}
			values = append(values, c)
		}
		if values != nil {
			return key, values
		}
	}
	return "", nil
}

var (
	zodOnce    sync.Once
	zodSchemas map[string]*omap
	zodErr     error
)

func loadZod() {
	dec := json.NewDecoder(bytes.NewReader(schemasJSON))
	dec.UseNumber()
	root, err := decodeOrdered(dec)
	if err != nil {
		zodErr = err
		return
	}
	zodSchemas = map[string]*omap{}
	for _, name := range root.(*omap).keys {
		cmd := root.(*omap).m[name].(*omap)
		if req, ok := cmd.get("request"); ok {
			if rs, ok := req.(*omap); ok {
				zodSchemas[name] = rs
			}
		}
	}
}

// ValidateRequest checks a request body against command's request schema
// and returns zod-shaped issues. Commands without a request schema accept
// anything.
func ValidateRequest(command string, body []byte) []Issue {
	zodOnce.Do(loadZod)
	if zodErr != nil {
		return []Issue{issue(nil, Issue{"code": "custom", "message": "contract schemas failed to load: " + zodErr.Error()})}
	}
	s, ok := zodSchemas[command]
	if !ok {
		return nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []Issue{issue(nil, Issue{"code": "custom", "message": err.Error()})}
	}
	if _, err := dec.Token(); err != io.EOF {
		return []Issue{issue(nil, Issue{"code": "custom", "message": "unexpected data after the JSON body"})}
	}
	return zodCheck(s, v, nil)
}
