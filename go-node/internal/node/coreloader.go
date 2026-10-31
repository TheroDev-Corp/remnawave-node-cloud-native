package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

// downloadFile is common/utils/download-file.ts: https only (also after
// redirects), 5 s idle / 15 s total timeout, 128 MiB cap, optional sha256
// check, written to <path>.download then renamed.
func downloadFile(ctx context.Context, rawURL, path, wantSHA256 string) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, fmt.Errorf("unexpected response %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if resp.Request.URL.Scheme != "https" {
		return "", 0, fmt.Errorf("redirected to a non-https url %q", resp.Request.URL.String())
	}
	const maxSize = 128 << 20
	if resp.ContentLength > maxSize {
		return "", 0, fmt.Errorf("content-length exceeds %d bytes", maxSize)
	}
	tmp := path + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	idle := time.AfterFunc(5*time.Second, cancel)
	n, err := io.Copy(io.MultiWriter(f, h), &idleReader{r: io.LimitReader(resp.Body, maxSize+1), t: idle})
	idle.Stop()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxSize {
		err = fmt.Errorf("download exceeds %d bytes", maxSize)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err == nil && wantSHA256 != "" && sum != wantSHA256 {
		err = fmt.Errorf("sha256 mismatch: expected %s, got %s", wantSHA256, sum)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	return sum, n, os.Rename(tmp, path)
}

type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	i.t.Reset(5 * time.Second)
	return n, err
}

type coreMarker struct {
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	MtimeMs     int64  `json:"mtimeMs"`
	Version     string `json:"version"`
	InstalledAt string `json:"installedAt"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// prepareCore is CoreLoaderService.prepare: install the custom core from
// xrayConfig.geodata.core {url, sha256}, or roll back to the bundled core.
func (n *Node) prepareCore(ctx context.Context, geodata any) {
	g, _ := geodata.(map[string]any)
	rawCore, has := g["core"]
	if !has || rawCore == nil {
		n.restoreStockCore()
		return
	}
	core, _ := rawCore.(map[string]any)
	u, _ := core["url"].(string)
	sum, _ := core["sha256"].(string)
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || !sha256Hex.MatchString(sum) {
		n.Log.Warn(`Invalid "core" section, skipped`)
		return
	}
	sum = strings.ToLower(sum)
	marker := n.CoreCustom[:strings.LastIndex(n.CoreCustom, "/")+1] + ".rw-core.json"
	if n.customCoreInstalled(marker, u, sum) {
		return
	}
	staged := n.CoreCustom + ".staged"
	got, size, err := downloadFile(ctx, u, staged, sum)
	if err == nil {
		err = os.Chmod(staged, 0o755)
	}
	var version string
	if err == nil {
		version, _, err = xray.Version(ctx, staged)
	}
	if err == nil {
		err = os.Rename(staged, n.CoreCustom)
	}
	if err == nil {
		err = n.pointCoreLinkAt(n.CoreCustom)
	}
	if err != nil {
		n.Log.Error("Failed to install custom core: " + err.Error())
		_ = os.Remove(staged)
		return
	}
	st, _ := os.Stat(n.CoreCustom)
	b, _ := json.MarshalIndent(coreMarker{u, got, st.Size(), st.ModTime().UnixMilli(), version, time.Now().UTC().Format(time.RFC3339Nano)}, "", "  ")
	_ = os.WriteFile(marker, b, 0o644)
	n.Log.Info(fmt.Sprintf("Installed %q (%d bytes)", version, size))
}

func (n *Node) customCoreInstalled(markerPath, u, sum string) bool {
	b, err := os.ReadFile(markerPath)
	if err != nil {
		return false
	}
	var m coreMarker
	if json.Unmarshal(b, &m) != nil || m.URL != u || m.SHA256 != sum {
		return false
	}
	st, err := os.Stat(n.CoreCustom)
	if err != nil || st.Size() != m.Size || st.ModTime().UnixMilli() != m.MtimeMs {
		return false
	}
	target, err := os.Readlink(n.CoreLink)
	return err == nil && target == n.CoreCustom
}

func (n *Node) restoreStockCore() {
	cur, err := os.Readlink(n.CoreLink)
	if err != nil || cur == n.CoreStock {
		return
	}
	if err := n.pointCoreLinkAt(n.CoreStock); err != nil {
		n.Log.Error("Failed to restore the bundled core: " + err.Error())
		return
	}
	_ = os.Remove(filepath.Join(filepath.Dir(n.CoreCustom), ".rw-core.json"))
	_ = os.Remove(n.CoreCustom)
	n.Log.Warn("No core configured, rolled back to the bundled one.")
}

func (n *Node) pointCoreLinkAt(target string) error {
	tmp := n.CoreLink + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, n.CoreLink)
}

var assetFileName = regexp.MustCompile(`^[\w.-]+$`)

// prepareAssets is GeodataService.prepare: download geodata.assets[] into
// XRAY_LOCATION_ASSET unless present; on failure create an empty stub so
// xray can still start.
func (n *Node) prepareAssets(ctx context.Context, geodata any) {
	g, ok := geodata.(map[string]any)
	if !ok {
		return
	}
	list, _ := g["assets"].([]any)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, raw := range list {
		a, _ := raw.(map[string]any)
		u, _ := a["url"].(string)
		file, _ := a["file"].(string)
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme != "https" || !assetFileName.MatchString(file) || file == "." || file == ".." {
			n.Log.Warn(`Invalid "assets" section, skipped`)
			return
		}
		path := filepath.Join(n.AssetsDir, file)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if _, _, err := downloadFile(ctx, u, path, ""); err != nil {
				n.Log.Error(fmt.Sprintf("Failed to download %q: %v", u, err))
				if f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
					_ = f.Close()
				} else if !errors.Is(err, os.ErrExist) {
					n.Log.Error("Failed to create stub: " + err.Error())
				}
			}
		}()
	}
	wg.Wait()
}
