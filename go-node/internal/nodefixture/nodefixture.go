// Package nodefixture runs testdata/node/fixture.mjs, which loads the TS
// node's own sources and npm packages, so tests can compare Go output with
// the original implementation. Tests skip when node or the repo's
// node_modules are missing.
package nodefixture

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Root is the go-node directory.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func Run(t testing.TB, req, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping cross-check with the TS node")
	}
	if _, err := os.Stat(filepath.Join(Root(), "..", "node_modules", "zod")); err != nil {
		t.Skip("run `npm ci` in the repo root to enable cross-checks with the TS node")
	}
	in, _ := json.Marshal(req)
	dir := filepath.Join(Root(), "testdata", "node")
	cmd := exec.Command(node, "--no-warnings", "--import", filepath.Join(dir, "ts-hooks.mjs"), filepath.Join(dir, "fixture.mjs"))
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("node fixture: %v\n%s", err, stderr.String())
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		t.Fatalf("node fixture output: %v\n%.500s", err, stdout)
	}
}
