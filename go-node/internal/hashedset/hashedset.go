// Package hashedset is a bit-exact port of @remnawave/hashed-set: a string
// set with an order-independent 64-bit hash (two djb2 variants XORed over
// the members). The panel sends this hash per inbound with xray/start, and
// the node compares it with its own to decide whether xray must restart.
package hashedset

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

type Set struct {
	m      map[string]struct{}
	hi, lo uint32
}

func New(values ...string) *Set {
	s := &Set{m: map[string]struct{}{}}
	for _, v := range values {
		s.Add(v)
	}
	return s
}

// djb2Dual iterates UTF-16 code units, like String.prototype.charCodeAt.
// The `| 0` int32 wrap in JS is plain uint32 overflow here.
func djb2Dual(s string) (uint32, uint32) {
	hi, lo := uint32(5381), uint32(5387)
	for _, c := range utf16.Encode([]rune(s)) {
		hi = (hi << 5) + hi + uint32(c)
		lo = (lo << 6) + lo + uint32(c)*37
	}
	return hi, lo
}

func (s *Set) Add(v string) {
	if _, ok := s.m[v]; ok {
		return
	}
	h, l := djb2Dual(v)
	s.hi ^= h
	s.lo ^= l
	s.m[v] = struct{}{}
}

func (s *Set) Delete(v string) {
	if _, ok := s.m[v]; !ok {
		return
	}
	h, l := djb2Dual(v)
	s.hi ^= h
	s.lo ^= l
	delete(s.m, v)
}

func (s *Set) Has(v string) bool { _, ok := s.m[v]; return ok }
func (s *Set) Len() int          { return len(s.m) }

// Hash64String is hashHigh.toString(16).padStart(8,'0') + hashLow (same).
// In JS the XOR accumulator is a signed int32, so a half with the top bit
// set prints as a negative hex number ("-24251c7d"), and padStart pads the
// whole string including the sign ("-1" → "000000-1"). The panel computes
// the same strings, so they are reproduced exactly.
func (s *Set) Hash64String() string { return jsHex(s.hi) + jsHex(s.lo) }

func jsHex(v uint32) string {
	x := int64(int32(v))
	var h string
	if x < 0 {
		h = "-" + strconv.FormatInt(-x, 16)
	} else {
		h = strconv.FormatInt(x, 16)
	}
	if len(h) < 8 {
		h = strings.Repeat("0", 8-len(h)) + h
	}
	return h
}
