package asn

import (
	"encoding/hex"
	"os"
	"reflect"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
)

func TestNumberKeyMatchesOrderedBinary(t *testing.T) {
	// Raw keys of ASN 1, 2 and 3 as lmdb-js stored them (keyEncoding: binary).
	for n, want := range map[float64]string{1: "13ff0000", 2: "14000000", 3: "14008000"} {
		if got := hex.EncodeToString(NumberKey(n)); got != want {
			t.Errorf("NumberKey(%v) = %s, want %s", n, got, want)
		}
	}
}

// TestAgainstLmdbJs reads every entry with lmdb-js and with this package.
// ASN_DB points at asn-prefixes.lmdb (copy it out of the TS image).
func TestAgainstLmdbJs(t *testing.T) {
	path := os.Getenv("ASN_DB")
	if path == "" {
		t.Skip("set ASN_DB to an asn-prefixes.lmdb to cross-check with lmdb-js")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var all [][3]any
	nodefixture.Run(t, map[string]any{"op": "asnDump", "path": path}, &all)
	if uint64(len(all)) != db.Entries() {
		t.Fatalf("lmdb-js sees %d entries, meta says %d", len(all), db.Entries())
	}
	for _, e := range all {
		asn := uint32(e[0].(float64))
		v4, v6, ok := db.Prefixes(asn)
		if !ok {
			t.Fatalf("ASN %d not found", asn)
		}
		if !reflect.DeepEqual(v4, strs(e[1])) || !reflect.DeepEqual(v6, strs(e[2])) {
			t.Fatalf("ASN %d differs:\n go %v %v\n js %v %v", asn, v4, v6, e[1], e[2])
		}
	}
	for _, missing := range []uint32{0, 4294967295, 999999999} {
		if _, _, ok := db.Prefixes(missing); ok {
			t.Errorf("ASN %d should be absent", missing)
		}
	}
	t.Logf("%d ASNs identical to lmdb-js", len(all))
}

func strs(v any) []string {
	out := []string{}
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
}
