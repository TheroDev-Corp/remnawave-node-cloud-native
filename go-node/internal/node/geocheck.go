package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

const (
	geocheckBin     = "/usr/local/bin/geocheck"
	geocheckTimeout = 45 * time.Second
)

var geocheckRunning atomic.Bool

// geocheck is GeocheckService.getGeocheck: one run at a time, bound to an
// IP or interface when given, 45 s timeout, the report passed through.
func (n *Node) geocheck(ctx context.Context, ip, iface string) (map[string]any, error) {
	ip, iface = strings.TrimSpace(ip), strings.TrimSpace(iface)
	bindTo := ""
	if ip != "" {
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("Geocheck: %q is not a valid IP address.", ip)
		}
		bindTo = ip
	} else if iface != "" {
		bindTo = iface
	}
	if !geocheckRunning.CompareAndSwap(false, true) {
		return nil, errors.New("Geocheck: a run is already in progress.")
	}
	defer geocheckRunning.Store(false)

	target := bindTo
	if target == "" {
		target = "default route"
	}
	args := []string{}
	if bindTo != "" {
		args = append(args, "--interface", bindTo)
	}
	args = append(args, "--json", "--svg-base64", "--quiet")
	ctx, cancel := context.WithTimeout(ctx, geocheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, geocheckBin, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("Geocheck via %s exceeded %dms and was killed.", target, geocheckTimeout.Milliseconds())
	}
	if err != nil {
		return nil, fmt.Errorf("Geocheck via %s failed: %v", target, err)
	}
	var report map[string]any
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("Geocheck via %s failed: %v", target, err)
	}
	img, _ := report["image"].(map[string]any)
	if img == nil || img["data"] == nil || img["data"] == "" {
		return nil, fmt.Errorf("Geocheck via %s failed: geocheck report carries no image", target)
	}
	return report, nil
}
