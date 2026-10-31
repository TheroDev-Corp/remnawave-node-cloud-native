package asn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// decodeMsgpackr decodes the msgpack lmdb-js writes, including msgpackr's
// records: ext 0x72 ("r") defines a structure id (0x40–0x7f) with its key
// list, and a byte in 0x40–0x7f starts a record of that structure.
func decodeMsgpackr(b []byte) (any, error) {
	d := &mpDecoder{b: b, structs: map[byte][]string{}}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	return v, nil
}

type mpDecoder struct {
	b       []byte
	i       int
	structs map[byte][]string
}

var errShort = errors.New("asn: truncated msgpack")

func (d *mpDecoder) take(n int) ([]byte, error) {
	if n < 0 || d.i+n > len(d.b) {
		return nil, errShort
	}
	s := d.b[d.i : d.i+n]
	d.i += n
	return s, nil
}

func (d *mpDecoder) uint(n int) (uint64, error) {
	s, err := d.take(n)
	if err != nil {
		return 0, err
	}
	switch n {
	case 1:
		return uint64(s[0]), nil
	case 2:
		return uint64(binary.BigEndian.Uint16(s)), nil
	case 4:
		return uint64(binary.BigEndian.Uint32(s)), nil
	}
	return binary.BigEndian.Uint64(s), nil
}

func (d *mpDecoder) str(n int) (any, error) {
	s, err := d.take(n)
	return string(s), err
}

func (d *mpDecoder) array(n int) (any, error) {
	out := make([]any, 0, n)
	for j := 0; j < n; j++ {
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (d *mpDecoder) mapN(n int) (any, error) {
	out := make(map[string]any, n)
	for j := 0; j < n; j++ {
		k, err := d.value()
		if err != nil {
			return nil, err
		}
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		out[fmt.Sprint(k)] = v
	}
	return out, nil
}

func (d *mpDecoder) record(id byte) (any, error) {
	keys, ok := d.structs[id]
	if !ok {
		return nil, fmt.Errorf("asn: msgpackr record %#x without a structure", id)
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func (d *mpDecoder) value() (any, error) {
	s, err := d.take(1)
	if err != nil {
		return nil, err
	}
	c := s[0]
	switch {
	case c <= 0x3f: // positive fixint (0x40–0x7f are record ids in msgpackr)
		return float64(c), nil
	case c <= 0x7f:
		return d.record(c)
	case c <= 0x8f:
		return d.mapN(int(c & 0x0f))
	case c <= 0x9f:
		return d.array(int(c & 0x0f))
	case c <= 0xbf:
		return d.str(int(c & 0x1f))
	case c >= 0xe0:
		return float64(int8(c)), nil
	}
	switch c {
	case 0xc0:
		return nil, nil
	case 0xc2:
		return false, nil
	case 0xc3:
		return true, nil
	case 0xc4, 0xc5, 0xc6: // bin
		n, err := d.uint(1 << (c - 0xc4))
		if err != nil {
			return nil, err
		}
		b, err := d.take(int(n))
		return append([]byte(nil), b...), err
	case 0xca:
		u, err := d.uint(4)
		return float64(math.Float32frombits(uint32(u))), err
	case 0xcb:
		u, err := d.uint(8)
		return math.Float64frombits(u), err
	case 0xcc, 0xcd, 0xce, 0xcf:
		u, err := d.uint(1 << (c - 0xcc))
		return float64(u), err
	case 0xd0:
		u, err := d.uint(1)
		return float64(int8(u)), err
	case 0xd1:
		u, err := d.uint(2)
		return float64(int16(u)), err
	case 0xd2:
		u, err := d.uint(4)
		return float64(int32(u)), err
	case 0xd3:
		u, err := d.uint(8)
		return float64(int64(u)), err
	case 0xd9, 0xda, 0xdb:
		n, err := d.uint(1 << (c - 0xd9))
		if err != nil {
			return nil, err
		}
		return d.str(int(n))
	case 0xdc, 0xdd:
		n, err := d.uint(2 << (c - 0xdc))
		if err != nil {
			return nil, err
		}
		return d.array(int(n))
	case 0xde, 0xdf:
		n, err := d.uint(2 << (c - 0xde))
		if err != nil {
			return nil, err
		}
		return d.mapN(int(n))
	case 0xd4: // fixext 1: msgpackr's record definition is ext 0x72 + id, keys, then the record
		t, err := d.take(2)
		if err != nil {
			return nil, err
		}
		if t[0] != 0x72 {
			return nil, fmt.Errorf("asn: unsupported msgpack ext %#x", t[0])
		}
		keysV, err := d.value()
		if err != nil {
			return nil, err
		}
		ka, ok := keysV.([]any)
		if !ok {
			return nil, errors.New("asn: bad record structure")
		}
		keys := make([]string, len(ka))
		for j, k := range ka {
			keys[j] = fmt.Sprint(k)
		}
		d.structs[t[1]] = keys
		return d.record(t[1])
	}
	return nil, fmt.Errorf("asn: unsupported msgpack byte %#x", c)
}
