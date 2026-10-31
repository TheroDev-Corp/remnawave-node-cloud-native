package hashedset

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
)

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func TestJSHexFormatting(t *testing.T) {
	for v, want := range map[uint32]string{0: "00000000", 1: "00000001", 0x7fffffff: "7fffffff",
		0xffffffff: "000000-1", 0x80000000: "-80000000", 0xdbdae383: "-24251c7d"} {
		if got := jsHex(v); got != want {
			t.Errorf("jsHex(%#x) = %q, want %q", v, got, want)
		}
	}
}

func TestBasics(t *testing.T) {
	if New().Hash64String() != "0000000000000000" {
		t.Error("empty set hash")
	}
	a, b := New("x", "y", "z"), New("z", "x", "y", "x")
	if a.Hash64String() != b.Hash64String() || b.Len() != 3 {
		t.Error("hash must be order-independent and ignore duplicates")
	}
	b.Delete("y")
	b.Add("y")
	if a.Hash64String() != b.Hash64String() {
		t.Error("delete+add must restore the hash")
	}
}

// Bit-exact against @remnawave/hashed-set, the package the panel uses.
func TestMatchesRemnawaveHashedSet(t *testing.T) {
	sets := [][]string{{}, {"a"}, {"a", "b"}, {"ünïcødé", "🔑", "日本"}, {"x"}, {"m", "n", "o", "p"}}
	big := make([]string, 5000)
	for i := range big {
		big[i] = uuid()
	}
	sets = append(sets, big, big[:1], big[:1000])
	var want []string
	nodefixture.Run(t, map[string]any{"op": "hashedSet", "sets": sets}, &want)
	for i, s := range sets {
		if got := New(s...).Hash64String(); got != want[i] {
			t.Errorf("set %d (%d values): Go %s, TS %s", i, len(s), got, want[i])
		}
	}

	// Incremental add/delete, the way the node tracks users between pushes.
	var ops [][2]string
	for i := 0; i < 400; i++ {
		v := big[i%50]
		op := "add"
		if i%3 == 2 {
			op = "delete"
		}
		ops = append(ops, [2]string{op, v})
	}
	var steps []string
	nodefixture.Run(t, map[string]any{"op": "hashedSetOps", "ops": ops}, &steps)
	s := New()
	for i, op := range ops {
		if op[0] == "add" {
			s.Add(op[1])
		} else {
			s.Delete(op[1])
		}
		if s.Hash64String() != steps[i] {
			t.Fatalf("step %d (%s %s): Go %s, TS %s", i, op[0], op[1], s.Hash64String(), steps[i])
		}
	}
}
