// Package contract holds the golden JSON Schemas of the panel↔node contract
// (testdata/contract/schemas.json, generated from libs/contract's zod
// schemas) and checks them against the current TS sources.
package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/nodefixture"
)

// TestGoldenSchemasAreCurrent fails when libs/contract changed (e.g. after
// an upstream sync) without regenerating the golden file:
//
//	echo '{"op":"schemas"}' | node --import ./testdata/node/ts-hooks.mjs testdata/node/fixture.mjs > testdata/contract/schemas.json
func TestGoldenSchemasAreCurrent(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join(nodefixture.Root(), "testdata", "contract", "schemas.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fresh any
	nodefixture.Run(t, map[string]any{"op": "schemas"}, &fresh)
	got, _ := json.Marshal(fresh)
	var want any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}
	wantB, _ := json.Marshal(want)
	if !bytes.Equal(got, wantB) {
		t.Fatal("libs/contract changed: regenerate testdata/contract/schemas.json and review the Go structs")
	}
}

// TestGoldenCoversTheContract pins the command list: 25 commands, of which
// the stock panel calls 20.
func TestGoldenCoversTheContract(t *testing.T) {
	golden, _ := os.ReadFile(filepath.Join(nodefixture.Root(), "testdata", "contract", "schemas.json"))
	var m map[string]struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(golden, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 25 {
		t.Fatalf("want 25 commands, got %d", len(m))
	}
	for _, name := range []string{"StartXrayCommand", "GetSystemStatsCommand", "GetUsersStatsCommand", "AddUsersCommand", "SyncCommand"} {
		if m[name].URL == "" {
			t.Errorf("%s missing", name)
		}
	}
}
