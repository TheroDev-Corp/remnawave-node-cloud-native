package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
)

// fakeCore is a shell script that answers `version` like xray does.
func fakeCore(version string) []byte {
	return []byte("#!/bin/sh\necho 'Xray " + version + " (Xray, Penetrates Everything.) test'\n")
}

// coreTestNode lays out the image's core paths under a temp dir: a stock
// core, /var/lib/rw-core with its "current" link, and an assets dir. Nothing
// here needs root, which is the point: the node runs as nobody.
func coreTestNode(t *testing.T) (*Node, string) {
	t.Helper()
	dir := t.TempDir()
	stock := filepath.Join(dir, "usr/local/bin/xray")
	if err := os.MkdirAll(filepath.Dir(stock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stock, fakeCore("26.7.28"), 0o755); err != nil {
		t.Fatal(err)
	}
	n := New(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), ha.Standalone{}, nil, nil, "", nil)
	n.CoreStock = stock
	n.CoreLink = filepath.Join(dir, "var/lib/rw-core/current")
	n.CoreCustom = filepath.Join(dir, "var/lib/rw-core/xray-custom")
	n.AssetsDir = filepath.Join(dir, "usr/local/share/xray")
	if err := os.MkdirAll(n.AssetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return n, dir
}

// tlsFiles serves path -> body over HTTPS and makes downloadFile trust it.
func tlsFiles(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	prev := http.DefaultClient
	http.DefaultClient = srv.Client()
	t.Cleanup(func() { http.DefaultClient = prev })
	return srv
}

func linkTarget(t *testing.T, path string) string {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return got
}

func TestEnsureCoreLink(t *testing.T) {
	n, _ := coreTestNode(t)

	// Fresh /var/lib/rw-core (emptyDir or first start): link to the stock core.
	n.EnsureCoreLink()
	if got := linkTarget(t, n.CoreLink); got != n.CoreStock {
		t.Fatalf("current -> %q, want stock %q", got, n.CoreStock)
	}

	// A working custom core is left alone.
	if err := os.WriteFile(n.CoreCustom, fakeCore("26.10.21"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := n.pointCoreLinkAt(n.CoreCustom); err != nil {
		t.Fatal(err)
	}
	n.EnsureCoreLink()
	if got := linkTarget(t, n.CoreLink); got != n.CoreCustom {
		t.Fatalf("current -> %q, want custom %q", got, n.CoreCustom)
	}

	// The custom core vanished (emptyDir wiped): back to stock, no dangling link.
	if err := os.Remove(n.CoreCustom); err != nil {
		t.Fatal(err)
	}
	n.EnsureCoreLink()
	if got := linkTarget(t, n.CoreLink); got != n.CoreStock {
		t.Fatalf("after losing the custom core: current -> %q, want stock", got)
	}
}

func TestPrepareCoreInstallReuseRollback(t *testing.T) {
	n, _ := coreTestNode(t)
	n.EnsureCoreLink()
	custom := fakeCore("26.10.21")
	sum := sha256.Sum256(custom)
	srv := tlsFiles(t, map[string][]byte{"/xray": custom})
	core := map[string]any{"core": map[string]any{"url": srv.URL + "/xray", "sha256": hex.EncodeToString(sum[:])}}
	ctx := context.Background()

	n.prepareCore(ctx, core)
	if got := linkTarget(t, n.CoreLink); got != n.CoreCustom {
		t.Fatalf("after install: current -> %q, want custom", got)
	}
	marker := filepath.Join(filepath.Dir(n.CoreCustom), ".rw-core.json")
	st1, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	// Same url and sha256 again: reused, not downloaded a second time.
	srv.Close()
	n.prepareCore(ctx, core)
	if st2, _ := os.Stat(marker); !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("custom core was reinstalled although it was already in place")
	}

	// No "core" section: roll back to the bundled core and drop the custom one.
	n.prepareCore(ctx, map[string]any{})
	if got := linkTarget(t, n.CoreLink); got != n.CoreStock {
		t.Fatalf("after rollback: current -> %q, want stock", got)
	}
	if _, err := os.Stat(n.CoreCustom); !os.IsNotExist(err) {
		t.Fatal("custom core left behind after rollback")
	}
}

func TestPrepareCoreBadChecksumKeepsStock(t *testing.T) {
	n, _ := coreTestNode(t)
	n.EnsureCoreLink()
	srv := tlsFiles(t, map[string][]byte{"/xray": fakeCore("26.10.21")})
	wrong := sha256.Sum256([]byte("something else"))
	n.prepareCore(context.Background(), map[string]any{"core": map[string]any{"url": srv.URL + "/xray", "sha256": hex.EncodeToString(wrong[:])}})
	if got := linkTarget(t, n.CoreLink); got != n.CoreStock {
		t.Fatalf("current -> %q, want stock after a checksum mismatch", got)
	}
	if _, err := os.Stat(n.CoreCustom + ".staged"); !os.IsNotExist(err) {
		t.Fatal("staged download left behind")
	}
}

func TestPrepareAssets(t *testing.T) {
	n, _ := coreTestNode(t)
	srv := tlsFiles(t, map[string][]byte{"/geosite_custom.dat": []byte("geosite-bytes")})
	if err := os.WriteFile(filepath.Join(n.AssetsDir, "present.dat"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.prepareAssets(context.Background(), map[string]any{"assets": []any{
		map[string]any{"url": srv.URL + "/geosite_custom.dat", "file": "geosite_custom.dat"},
		map[string]any{"url": srv.URL + "/missing.dat", "file": "missing.dat"},
		map[string]any{"url": srv.URL + "/geosite_custom.dat", "file": "present.dat"},
	}})
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(n.AssetsDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(b)
	}
	if got := read("geosite_custom.dat"); got != "geosite-bytes" {
		t.Fatalf("downloaded asset = %q", got)
	}
	if got := read("missing.dat"); got != "" {
		t.Fatalf("failed download should leave an empty stub, got %q", got)
	}
	if got := read("present.dat"); got != "keep" {
		t.Fatalf("existing asset was overwritten: %q", got)
	}
}

func TestAssetsDirFollowsXrayLocationAsset(t *testing.T) {
	t.Setenv("XRAY_LOCATION_ASSET", "")
	if got := assetsDir(); got != "/usr/local/share/xray" {
		t.Fatalf("default assets dir = %q", got)
	}
	t.Setenv("XRAY_LOCATION_ASSET", "/data")
	if got := assetsDir(); got != "/data" {
		t.Fatalf("assets dir with XRAY_LOCATION_ASSET=/data = %q", got)
	}
	n := New(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), ha.Standalone{}, nil, nil, "", nil)
	if n.AssetsDir != "/data" {
		t.Fatalf("Node.AssetsDir = %q, want /data (geodata.service.ts ASSETS_DIR)", n.AssetsDir)
	}
}
