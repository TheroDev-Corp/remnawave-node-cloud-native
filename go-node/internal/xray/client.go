package xray

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	statscmd "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// API is the gRPC client to xray's HandlerService and StatsService, using
// xray-core's own generated stubs (pinned to the same commit as the binary).
type API struct {
	conn    *grpc.ClientConn
	Stats   statscmd.StatsServiceClient
	Handler handlercmd.HandlerServiceClient
}

// Target turns the api inbound's listen value into a gRPC target: "@name"
// is an abstract unix socket (what the TS node uses), anything else is
// dialled as host:port (local development on non-Linux hosts).
func Target(listen string) string {
	if name, ok := strings.CutPrefix(listen, "@"); ok {
		return "unix-abstract:" + name
	}
	return "passthrough:///" + listen
}

// Dial creates a lazy client (no connection until the first call), with the
// same 100 MB receive limit as app.module.ts.
func Dial(listen string) (*API, error) {
	conn, err := grpc.NewClient(Target(listen),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(100<<20)),
	)
	if err != nil {
		return nil, err
	}
	return &API{conn: conn, Stats: statscmd.NewStatsServiceClient(conn), Handler: handlercmd.NewHandlerServiceClient(conn)}, nil
}

func (a *API) Close() error { return a.conn.Close() }

// SysStats is getSysStats(); a short timeout keeps health checks snappy.
func (a *API) SysStats(ctx context.Context) (*statscmd.SysStatsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return a.Stats.GetSysStats(ctx, &statscmd.SysStatsRequest{})
}

// ErrProcessDown means xray died (or was replaced) while we waited for it.
var ErrProcessDown = errors.New("xray process down")

// WaitReady is getXrayInternalStatus(): p-retry with 30 retries, 100 ms
// growing ×1.5 up to 2 s, polling GetSysStats and giving up early if the
// process exits or its pid changes.
func WaitReady(ctx context.Context, api *API, p *Process) error {
	startPID := p.Status().PID
	delay := 100 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= 30; attempt++ {
		if _, err := api.SysStats(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		st := p.Status()
		if !st.Up || (startPID != 0 && st.PID != startPID) {
			return fmt.Errorf("%w: Xray Core process is not running anymore (%s)", ErrProcessDown, p.StatusLine())
		}
		if attempt == 30 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(time.Duration(float64(delay)*1.5), 2*time.Second)
	}
	return fmt.Errorf("Xray Core did not become ready in time: %v", lastErr)
}
