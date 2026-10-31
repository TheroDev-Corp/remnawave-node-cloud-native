// Command node is a Go port of the Remnawave node (the TS app in ../src):
// same API, same SECRET_KEY, same behavior toward the panel, with xray-core
// as a supervised child process. License: AGPL-3.0-only, like upstream.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/api"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/asn"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/config"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/ha"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/health"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/node"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/secretkey"
	"github.com/TheroDev-Corp/remnawave-node-cloud-native/go-node/internal/xray"
)

func main() {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	mat, checks, err := secretkey.Load(cfg.SecretKey, time.Now())
	if checks != nil {
		if err != nil {
			log.Error("\n" + secretkey.Report(checks, ""))
		} else {
			log.Info("\n" + secretkey.Report(checks, mat.SNI))
		}
	}
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	proc := xray.NewProcess(cfg.XrayBinary, "/var/log/xray/current", log)
	listen := apiListen(cfg)
	xapi, err := xray.Dial(listen)
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
	stopNet := make(chan struct{})
	defer close(stopNet)
	// HA: Lease election when enabled (else always leader), peer sync when a
	// headless Service is configured.
	var state ha.State = ha.Standalone{ID: cfg.PodName}
	var elector *ha.Elector
	ns := ha.Namespace()
	if cfg.K8sLeaderElection {
		elector = ha.NewElector(log, ha.NewK8s(), cfg.K8sLeaseName, cfg.PodName, ns)
		state = elector
	} else {
		log.Info("K8s Leader Election disabled, running as standalone Leader.")
	}
	n := node.New(cfg, log, state, proc, xapi, listen, node.StartNetStats(stopNet))
	if _, err := os.Stat(asn.DefaultPath); err != nil {
		log.Warn(asn.DefaultPath + " not found — ASN lookup disabled")
	} else if db, err := asn.Open(asn.DefaultPath); err != nil {
		log.Error("Failed to open ASN LMDB database: " + err.Error())
	} else {
		defer db.Close()
		n.Plugins.ASN = db
		log.Info("ASN LMDB database opened successfully")
	}
	var peers *ha.PeerSync
	if cfg.PeerHeadlessService != "" {
		peerPort := cfg.HealthPort
		if peerPort == 0 {
			peerPort = 3000
		}
		peers = ha.NewPeerSync(log, n, elector, cfg.PeerHeadlessService, ns, peerPort,
			ha.NewPeerAuth(cfg.SecretKey), &ha.PeerTLS{Cert: mat.NodeCert, CA: mat.CAPool})
		n.Repl = peers
	} else {
		log.Info("PEER_HEADLESS_SERVICE is not configured. Standalone mode active.")
	}
	n.RefreshXrayVersion(ctx)
	xver := ""
	if v := n.XrayVersion(); v != nil {
		xver = *v
	}

	rt := api.NewRouter(log, api.NewJWTVerifier(mat.JWTKey), cfg.InternalRestToken)
	n.Register(rt)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.NodePort))
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
	srv := api.NewServer("", rt, api.TLSConfig(mat, cfg.SNIVerification), log)
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err.Error())
			os.Exit(1)
		}
	}()

	var extra []*http.Server
	if cfg.HealthPort != 0 {
		mux := health.PlainServer(n.HA, proc)
		hl, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", cfg.HealthPort))
		if err != nil {
			log.Error(err.Error())
			os.Exit(1)
		}
		if peers != nil {
			// Peers talk TLS (node certificate) on the probe port.
			peers.Register(mux)
			hl = (&ha.PeerTLS{Cert: mat.NodeCert, CA: mat.CAPool}).MuxListener(hl)
		}
		hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		extra = append(extra, hs)
		go func() { _ = hs.Serve(hl) }()
		log.Info(fmt.Sprintf("Plain HTTP Health check server listening on port %d", cfg.HealthPort))
	}
	// The internal socket is where xray posts routing webhooks (abstract
	// unix sockets are Linux-only).
	if runtime.GOOS == "linux" {
		irt := api.NewRouter(log, nil, cfg.InternalRestToken)
		n.RegisterInternal(irt)
		if il, err := net.Listen("unix", "@"+cfg.InternalSocketPath); err == nil {
			is := &http.Server{Handler: irt, ReadHeaderTimeout: 10 * time.Second}
			extra = append(extra, is)
			go func() { _ = is.Serve(il) }()
		} else {
			log.Error("internal socket: " + err.Error())
		}
		if !cfg.DisableInstanceLock && !acquireInstanceLock() {
			log.Error("Another Remnawave Node instance is already running on this host (abstract socket rwnode-lock is taken).")
		}
	}

	if elector != nil {
		elector.Start(ctx)
	}
	if peers != nil {
		peers.Start(ctx)
	}
	log.Info(fmt.Sprintf("Remnawave Node v%s (Go) · xray %s · listening on :%d", node.Version, xver, cfg.NodePort))

	<-ctx.Done()
	log.Info("Shutting down...")
	if peers != nil {
		peers.Shutdown()
	}
	if elector != nil {
		// Release the lease and keep serving 5 s so the new leader takes over.
		elector.Shutdown(context.Background())
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	for _, s := range extra {
		_ = s.Shutdown(shutdownCtx)
	}
	n.StopXray(false, false)
}

// apiListen is the xray api inbound address: the abstract socket the TS
// node uses, or a loopback port on hosts without abstract sockets.
var _ ha.Local = (*node.Node)(nil)

func apiListen(cfg *config.Config) string {
	if runtime.GOOS == "linux" {
		return "@" + cfg.XtlsAPISocketPath
	}
	return "127.0.0.1:61000"
}

// acquireInstanceLock holds the abstract socket "\0rwnode-lock" for the
// process lifetime, like acquire-instance-lock.ts.
func acquireInstanceLock() bool {
	l, err := net.Listen("unix", "@rwnode-lock")
	if err != nil {
		return !errors.Is(err, syscall.EADDRINUSE)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return true
}
