// Package asn reads the ASN → prefixes database the TS node opens with
// lmdb-js (/usr/local/share/asn/asn-prefixes.lmdb, from remnawave/asn-index)
// without cgo: a read-only walk of the LMDB B+tree (lmdb-js' "master3" data
// format, version 2), ordered-binary number keys, and msgpackr values with
// inline record structures.
package asn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// DefaultPath is ASN_LMDB_PATH.
const DefaultPath = "/usr/local/share/asn/asn-prefixes.lmdb"

const (
	pageHdr   = 24 // pgno, txnid, pad, flags, lower/upper
	nodeHdr   = 8  // lo, hi, flags, ksize
	pBranch   = 0x01
	pLeaf     = 0x02
	pMeta     = 0x08
	fBigData  = 0x01
	metaMagic = 0xBEEFC0DE
	invalidPg = math.MaxUint64
)

type DB struct {
	data    []byte
	psize   uint64
	root    uint64
	depth   uint16
	entries uint64
	unmap   func() error
}

// Open maps the database read-only.
func Open(path string) (*DB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, unmap, err := mapFile(f)
	if err != nil {
		return nil, err
	}
	db := &DB{data: data, unmap: unmap}
	if err := db.readMeta(); err != nil {
		_ = unmap()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error {
	if db.unmap != nil {
		return db.unmap()
	}
	return nil
}

func (db *DB) Entries() uint64 { return db.entries }

// readMeta picks the newer of the two meta pages.
func (db *DB) readMeta() error {
	var best struct {
		ok          bool
		txn         uint64
		psize, root uint64
		depth       uint16
		entries     uint64
	}
	psize := uint64(4096)
	for i := uint64(0); i < 2; i++ {
		off := i * psize
		if off+pageHdr+160 > uint64(len(db.data)) {
			break
		}
		p := db.data[off:]
		if binary.LittleEndian.Uint16(p[18:])&pMeta == 0 {
			continue
		}
		m := p[pageHdr:]
		if binary.LittleEndian.Uint32(m[0:]) != metaMagic {
			continue
		}
		if v := binary.LittleEndian.Uint32(m[4:]); v != 2 {
			return fmt.Errorf("asn: unsupported LMDB data version %d", v)
		}
		ps := uint64(binary.LittleEndian.Uint32(m[24:])) // dbs[FREE].md_pad holds the page size
		main := m[24+48:]                                // dbs[MAIN]
		txn := binary.LittleEndian.Uint64(m[24+96+8:])
		if i == 0 && ps != 0 {
			psize = ps
		}
		if !best.ok || txn > best.txn {
			best.ok, best.txn, best.psize = true, txn, ps
			best.depth = binary.LittleEndian.Uint16(main[6:])
			best.entries = binary.LittleEndian.Uint64(main[32:])
			best.root = binary.LittleEndian.Uint64(main[40:])
		}
	}
	if !best.ok {
		return errors.New("asn: no valid LMDB meta page")
	}
	db.psize, db.root, db.depth, db.entries = best.psize, best.root, best.depth, best.entries
	return nil
}

func (db *DB) page(pg uint64) ([]byte, error) {
	off := pg * db.psize
	if pg == invalidPg || off+db.psize > uint64(len(db.data)) {
		return nil, fmt.Errorf("asn: page %d out of range", pg)
	}
	return db.data[off : off+db.psize], nil
}

type node struct {
	key   []byte
	lo    uint32 // data size (leaf) / pgno low bits (branch)
	flags uint16
	data  []byte // inline data (leaf)
}

func nodeAt(p []byte, i int) (node, error) {
	ptr := int(binary.LittleEndian.Uint16(p[pageHdr+2*i:])) + pageHdr
	if ptr+nodeHdr > len(p) {
		return node{}, errors.New("asn: node out of page")
	}
	n := p[ptr:]
	lo := uint32(binary.LittleEndian.Uint16(n[0:])) | uint32(binary.LittleEndian.Uint16(n[2:]))<<16
	flags := binary.LittleEndian.Uint16(n[4:])
	ks := int(binary.LittleEndian.Uint16(n[6:]))
	if nodeHdr+ks > len(n) {
		return node{}, errors.New("asn: key out of page")
	}
	return node{key: n[nodeHdr : nodeHdr+ks], lo: lo, flags: flags, data: n[nodeHdr+ks:]}, nil
}

func numKeys(p []byte) int { return int(binary.LittleEndian.Uint16(p[20:])) >> 1 }

// get returns the raw value for key (memcmp order, LMDB's default).
func (db *DB) get(key []byte) ([]byte, bool, error) {
	if db.root == invalidPg {
		return nil, false, nil
	}
	pg := db.root
	for depth := 0; depth < 64; depth++ {
		p, err := db.page(pg)
		if err != nil {
			return nil, false, err
		}
		flags := binary.LittleEndian.Uint16(p[18:])
		n := numKeys(p)
		switch {
		case flags&pBranch != 0:
			// The last child whose key <= key (child 0's key is implicit).
			lo, hi := 1, n-1
			child := 0
			for lo <= hi {
				mid := (lo + hi) / 2
				nd, err := nodeAt(p, mid)
				if err != nil {
					return nil, false, err
				}
				if bytes.Compare(nd.key, key) <= 0 {
					child, lo = mid, mid+1
				} else {
					hi = mid - 1
				}
			}
			nd, err := nodeAt(p, child)
			if err != nil {
				return nil, false, err
			}
			pg = uint64(nd.lo) | uint64(nd.flags)<<32
		case flags&pLeaf != 0:
			lo, hi := 0, n-1
			for lo <= hi {
				mid := (lo + hi) / 2
				nd, err := nodeAt(p, mid)
				if err != nil {
					return nil, false, err
				}
				switch c := bytes.Compare(nd.key, key); {
				case c < 0:
					lo = mid + 1
				case c > 0:
					hi = mid - 1
				default:
					size := int(nd.lo)
					if nd.flags&fBigData != 0 {
						// The value lives on overflow pages starting at this pgno.
						if len(nd.data) < 8 {
							return nil, false, errors.New("asn: bad overflow node")
						}
						start := binary.LittleEndian.Uint64(nd.data) * db.psize
						if start+pageHdr+uint64(size) > uint64(len(db.data)) {
							return nil, false, errors.New("asn: overflow data out of range")
						}
						return db.data[start+pageHdr : start+pageHdr+uint64(size)], true, nil
					}
					if size > len(nd.data) {
						return nil, false, errors.New("asn: data out of page")
					}
					return nd.data[:size], true, nil
				}
			}
			return nil, false, nil
		default:
			return nil, false, fmt.Errorf("asn: unexpected page flags %#x", flags)
		}
	}
	return nil, false, errors.New("asn: tree too deep")
}

// NumberKey is ordered-binary's encoding of a non-negative number key.
func NumberKey(n float64) []byte {
	bits := math.Float64bits(n)
	high, low := uint32(bits>>32), uint32(bits)
	b := make([]byte, 9)
	binary.BigEndian.PutUint32(b[0:], high>>4|0x10000000)
	binary.BigEndian.PutUint32(b[4:], low>>4|high<<28)
	b[8] = byte(low&0xf) << 4
	switch {
	case low&0xf != 0:
		return b[:9]
	case low&0xfffff != 0:
		return b[:8]
	case low != 0 || high&0xf != 0:
		return b[:6]
	}
	return b[:4]
}

// Prefixes implements the node's ASNResolver: getIpv4ByAsn + getIpv6ByAsn.
func (db *DB) Prefixes(asn uint32) (v4, v6 []string, ok bool) {
	raw, found, err := db.get(NumberKey(float64(asn)))
	if err != nil || !found {
		return nil, nil, false
	}
	v, err := decodeMsgpackr(raw)
	if err != nil {
		return nil, nil, false
	}
	m, _ := v.(map[string]any)
	return strings(m["ipv4"]), strings(m["ipv6"]), m != nil
}

func strings(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, e := range a {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
