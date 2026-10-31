# go-node: Go port of the Remnawave node

A drop-in replacement for the TS node in this repository. It exposes the same API on `NODE_PORT`, reads the same `SECRET_KEY` and env vars, and runs xray-core as a supervised child process. The stock panel shouldn't be able to tell it apart. The TS node in `../src` is untouched.

**Status: all 5 milestones done.** All 25 contract routes are implemented. The 20 the panel calls are tested end to end against a real xray, a side-by-side run against the published TS image (`3.4.5-HA`) shows 0 differences the panel could observe, and the Helm chart runs the Go node with HA in a kind cluster through failover and rolling updates without dropping a user's traffic.

| Milestone | Scope | State |
|---|---|---|
| 1 | config/env parity, `SECRET_KEY` checks, mTLS TLS 1.3 + RS256 JWT + HKDF SNI, zstd bodies, xray supervisor, health, golden contract schemas | ✅ |
| 2 | all 20 panel endpoints + must-match items (numeric counters, semver `nodeVersion`, non-2xx `get-system-stats` when xray is down, bit-exact HashedSet, atomic reset) | ✅ |
| 3 | TS-vs-Go container parity harness (same `SECRET_KEY`, generated xray config diff, traffic counters) | ✅ |
| 4 | HA layer (Lease election, pod labels, peer sync) with the fork's bugs fixed, tested in kind | ✅ |
| 5 | Dockerfile, resource numbers, parity checklist | ✅ |

## Layout

```
main.go                 wiring: TLS server, HEALTH_PORT, METRICS_PORT, internal socket, instance lock, shutdown
internal/config         env vars of config.schema.ts + the values init-env.sh generates
internal/secretkey      SECRET_KEY decode, the 7 integrity checks, HKDF SNI
internal/api            mTLS server, Nest-compatible router (drop on unknown route / bad JWT),
                        zstd/gzip/br bodies, {"response":…} envelope, helmet, ETag, gzip responses
internal/xray           xray child process (no s6), gRPC client (xray-core's own stubs), readiness
internal/health         /node/health/* and HEALTH_PORT /health/*
internal/ha             Lease election + pod role labels (k8s API client), peer sync (replication,
                        snapshot + journal catch-up, hand-off), signed peer requests over TLS
internal/asn            pure-Go reader for the TS node's asn-prefixes.lmdb (LMDB + msgpackr)
internal/hashedset      bit-exact @remnawave/hashed-set (JS int32 hex quirks included)
internal/node           the Nest modules: xray start/stop/healthcheck + config generation + custom core,
                        handler (add/remove user(s)), stats, system info, network sampler, plugins
                        (sync, torrent blocker, suspension, pre/post-start), geocheck, internal routes;
                        METRICS_PORT /metrics (Go-only)
internal/testpki        panel-shaped PKI, SECRET_KEY and API JWT, for tests and the parity harness
internal/contract       embedded JSON Schemas of libs/contract and @remnawave/node-plugins (exported from
                        their zod schemas); a zod-faithful request validator (same issues, same order,
                        unknown keys stripped); drift test
internal/parity         TS-vs-Go container parity harness (milestone 3)
internal/kindtest       the Helm chart with the Go node in kind: election, failover, rollout (milestone 4)
testdata/node           fixture.mjs: runs the TS node's own code/packages for cross-checks
```

## Tests

```bash
go test ./...                              # unit + TLS end-to-end
XRAY_BINARY=/path/to/xray go test ./...    # + supervisor tests against a real xray v26.7.28 (the pinned version)
```

The cross-checks run when `node` is installed and `npm ci` has been run in the repo root:
- `normalizePem`, `deriveSni` and the contract schemas come from the TS sources.
- The panel's exact zstd settings (level 1, long-distance matching, `windowLog` 25) come from Node's zlib. The test asserts the frame really needs more than an 8 MiB window.
- HashedSet is checked against `@remnawave/hashed-set`: 5000-member sets, Unicode, 400-step add/delete sequences.
- `internal/asn` reads `asn-prefixes.lmdb` (the file the TS image ships, from remnawave/asn-index) without cgo. With `ASN_DB` set it is compared with lmdb-js on every entry: 87,414 ASNs, identical. `asList` ignore lists therefore resolve like in the TS node.
- `internal/node/e2e_test.go` plays the panel (mTLS, JWT, zstd) against a real xray with VLESS, Trojan, Shadowsocks, SS2022 and Hysteria 2 inbounds. The Hysteria inbound runs TLS on a self-signed certificate (`testpki.SelfSigned`), which the test client pins by sha256. It calls every route the panel uses, and every 2xx body is validated by the TS contract's own zod schemas (31 responses). It also checks:
  - `get-system-stats` answers 500/A010 while xray is down.
  - `nodeVersion` is semver ≥ 2.7.0 per the `semver` package.
  - A push with identical hashes, or after incremental add/remove-user, does **not** restart xray (same pid).
  - Users really land in, and leave, each inbound, per xray's own API.
  - Real traffic: a second xray plays the client and downloads 512 KiB through Hysteria 2 and through VLESS. The users' and the inbound's counters, and the online status, must reflect it.
  - `reset:true` zeroes the counters.
  - The torrent blocker regenerates the config.
  - Suspension and restore work, with the fork's bugs fixed (below).

## Container parity (milestone 3)

`internal/parity` starts the TS image and the Go image side by side on one Docker network with the same `SECRET_KEY`, plays the panel against both with identical requests, and diffs everything the panel can observe:

- status codes, header names and values (helmet, ETag, `Vary`, `Content-Encoding`), and bodies (volatile values such as timestamps, host stats and counters are compared by JSON type only);
- the generated xray config, read from each node's `/internal/get-config` over its abstract socket, after start, after incremental user changes, and with the torrent blocker on;
- real traffic: an xray client container downloads 512 KiB as three users over VLESS, Trojan and Hysteria 2 (self-signed cert, pinned) through each node; the user counters must match (they do, to the byte), and while a connection is held both nodes must list the client container's IP;
- the torrent blocker end to end: a real BitTorrent handshake through VLESS, the xray webhook into the node, one report collected per node, user 1 cut off and user 2 unaffected on both;
- error paths: bad JWT and unknown route (socket dropped), nine kinds of invalid bodies (identical zod issues), xray refusing a config, xray down, geocheck failing.

```bash
# images: the published TS image and a Go image (see Dockerfile)
PARITY_TS_IMAGE=ghcr.io/therodev-corp/remnawave-node-cloud-native:3.4.5-HA \
PARITY_GO_IMAGE=remnawave-node-go:amd64 PARITY_REPORT=parity.md \
  go test -v -run TestParity ./internal/parity/
```

Latest run (both amd64, colima with Rosetta): **54 steps, 0 diffs.** On Apple Silicon use a colima VM with `--vm-type vz --vz-rosetta`: under qemu user-mode emulation xray (Go) crashes at random in the runtime, on both nodes alike.

What the harness found and the Go node now does like the TS node:
- **Validation errors** are zod v4 issues (`invalid_type`, `invalid_value`, `invalid_format`, `too_small`, `invalid_union` with the discriminator note, …) in zod's order and wording, and **unknown keys are stripped, not rejected**, like plain `z.object`. A newer panel adding a field would otherwise have got 400s from the Go node. `TestZodParity` checks 36 bodies against the TS contract's real zod schemas.
- **Responses ≥ 1 KiB are Brotli-compressed for axios** (`Accept-Encoding: gzip, compress, deflate, br`), as `compression@1.8.1` negotiates. `TestNegotiateEncoding` checks the port against the `negotiator` package.
- **xray start failures** read `Xray Core process is not running anymore (s6: down (exitcode 23) 0 seconds, ready 0 seconds) · <last xray log line>`, in s6-svstat's format.
- **geocheck failures** carry Node's `execFile` message (`Command failed: … \n<stderr>`).

Differences the harness tolerates, by design:
- When xray rejects a config, xray's own message names its config source: the TS node's internal socket URL, **including the internal REST token**, versus `stdin:` for the Go node. The Go node doesn't leak the token into the panel. (The longer TS text can also push that response over the 1 KiB compression threshold.)
- `system.interface` is `null` until a node's 1 s network sampler has a first rate, on both nodes; the harness waits 3 s before the first start.
- Whether geocheck fails fast or hits its 45 s limit depends on the network, so only the error code and prefix are compared.

## HA (milestone 4)

The HA layer follows the fork's design, ported decision by decision (`leader-election.service.ts`, `peer-sync.service.ts`, `health.service.ts`):

- **Election:** a `coordination.k8s.io` Lease (10 s duration, renewed every 2 s, optimistic concurrency via `resourceVersion`). The holder labels its pod `role=leader`, every other pod `role=standby`, and the chart's Services select `role: leader`, so only the leader gets panel and user traffic. A new leader strips the stale label of a holder that died without releasing.
- **Warm standbys:** the leader replicates `xray/start`, user changes, suspensions and plugin syncs to the pods behind the headless Service. A new pod catches up from a peer snapshot, starts xray, and is Ready only once xray is up, so a rollout waits for a real hot standby.
- **Hand-off:** a newer, hot pod (rolling update) asks the leader to yield. The leader releases the lease and keeps serving for a 10 s drain overlap. A restarted container (`restartCount > 0`) never asks, so a crash-looping pod can't take leadership.
- **xray health gate:** a leader whose xray died (OOM) while a standby is healthy steps down and wakes the standby. A pod with a warm peer won't take the lease until its own xray is up; on a genuine cold start it takes it at once so the panel can connect.
- **Shutdown:** the leader releases the lease and drains 5 s before exit.

Configuration is the fork's: `K8S_LEADER_ELECTION_ENABLED`, `K8S_LEASE_NAME`, `PEER_HEADLESS_SERVICE`, `HEALTH_PORT` (peer port), `POD_NAME`, `POD_NAMESPACE`. Go and TS pods don't peer with each other (Go never sends or accepts the TS `X-Secret-Key`, see fix 5), so migrate with a full rollout.

`internal/ha` unit tests run two and three electors against an in-memory API server (failover after a release and after expiry, stale-label cleanup, every yield rule, the health gate) and two peer-sync instances over real TLS sockets (catch-up with plugin and journal, 41 actions replicated in order, unsigned requests refused).

### In kind

`internal/kindtest` installs `charts/remnanode` with two Go replicas in a kind cluster, plays the panel through a port-forward to the leader, and runs an xray client pod that downloads through the leader-only Service as a user added with `add-user` (not in the original config). It probes every 200 ms through each event:

| Event | Leader change | Downloads during the event |
|---|---|---|
| Cold start | one leader 1 s after install, standby hot 1.3 s after the panel's push | — |
| Leader pod deleted (graceful) | 0.1 s | 218 ok, 0 failed |
| Leader pod force-deleted | 1.3 s | 241 ok, 0 failed |
| xray SIGKILLed on the leader (OOM-style) | 2 s (leader steps down) | 179 ok, 1 failed (2.2 s) |
| Node process SIGKILLed on the leader (lease not released) | 1.7 s (restarted container releases its old lease) | 230 ok, 1 failed (2.2 s) |
| Rolling update (`rollout restart`) | hand-off to a new pod | 578 ok, 0 failed |

The probe runs in the cluster (busybox `wget` through the client's HTTP proxy, every 200 ms), not through `kubectl port-forward`, which dies on the first reset connection.

```bash
kind create cluster --name rwnode
kind load docker-image --name rwnode remnawave-node-go:dev   # + busybox:1.36.1
KIND_CONTEXT=kind-rwnode KIND_IMAGE=remnawave-node-go:dev go test -v -timeout 30m ./internal/kindtest/
```

## Image and resources (milestone 5)

`Dockerfile` builds a static Go binary (`CGO_ENABLED=0`) and copies it onto `scratch` with what the TS image ships next to the node: xray-core v26.7.28 (`/usr/local/bin/xray`, `rw-core` symlink, geoip/geosite), geocheck 0.3.0, `asn-prefixes.lmdb`, and CA certificates. Downloads are checksum-verified like in `docker/Dockerfile`. No shell, no s6, no Node.js.

```bash
docker buildx build -t remnawave-node-go go-node
```

Offline builds can replace the download stages with local folders: `--build-context build=… --build-context xray=… --build-context geocheck=… --build-context asn=…` (each folder holds what the stage would produce). The harness images were built that way, with the final stage unchanged.

Measured by the parity harness, both images amd64 on the same colima VM (Rosetta), whole container (node + xray), same config and traffic:

| | TS `3.4.5-HA` | Go |
|---|---|---|
| Image, on disk / compressed | 469 MB / 115 MB | 117 MB / 31 MB (incl. the 12.6 MB ASN database) |
| Memory, idle with xray running | 190 MiB (6 GiB VM), 253 MiB (8 GiB VM) | 98–99 MiB on both |
| Memory after 3 × 512 KiB per user | 200 / 261 MiB | 103–104 MiB |
| CPU, idle | 0.11–0.25 % | 0.05–0.09 % |
| Processes/threads (PIDs) | 51–53 | 19–22 |

xray-core is the same binary in both, so the difference is the node itself: Node.js + V8 (whose heap grows with available memory) and s6 versus one Go process.

## Runs as nobody

The image runs as `nobody` (`USER 65534:65534`); no part of the node needs root. Audit:
- **No root needed:**
  - the panel API, HEALTH_PORT and METRICS_PORT;
  - the abstract unix sockets;
  - the Lease, pod labels and peer sync;
  - the xray child, the `/proc` stats, the ASN mmap and geocheck;
  - the torrent block (a routing rule over gRPC).
- **No-ops:** nftables/sockdestroy are no-ops in the fork.
- **Writable for nobody in the image:**

| Path | Used for |
|---|---|
| `/var/lib/rw-core` | `current`, the core link; `xray-custom` and its marker for a custom core (`geodata.core`) |
| `/usr/local/share/xray` | panel `geodata.assets`, unless `XRAY_LOCATION_ASSET` points elsewhere |
| `/var/log/xray` | `current`, the xray log copy |
| `/tmp` | 1777 |

Binaries and the ASN db stay root-owned.

How the core path works:
- `/usr/local/bin/rw-core` (the exec path and process name, as in the TS node) is a fixed link to `/var/lib/rw-core/current`, which points at the bundled `/usr/local/bin/xray` or at a custom core.
- The TS node keeps these files in `/usr/local/bin`; they moved here so the switch works without root.
- At startup the node points a missing or dangling `current` back at the bundled core, for example when `/var/lib/rw-core` is an emptyDir.
- Panel `geodata.assets` go to `XRAY_LOCATION_ASSET` when it is set, as in the TS node (geodata.service.ts). Earlier Go builds always wrote to `/usr/local/share/xray`.

What xray needs:
- Inbounds below 1024 need the sysctl `net.ipv4.ip_unprivileged_port_start`. Docker sets it to 0 for containers, and the chart sets it in the pod. With `hostNetwork` it can't be set, so such ports need `podSecurity.runAsUser: 0`.
- Features that need `CAP_NET_ADMIN` (sockopt `mark`/`tproxy`, `tun` inbounds) don't work as nobody. None of the gitops profiles use them.

What the chart (`charts/remnanode`) sets:
- **Every pod:**
  - `fsGroup: 65534`, so a TLS secret with `defaultMode` 0400 becomes `0440 root:65534`, and the geodata PVC is readable;
  - the port sysctl, unless hostNetwork.
  
  Both are harmless for root pods (the TS image).
- `podSecurity.runAsUser`/`runAsGroup` override the image's user (`0` runs as root).
- `podSecurity.runAsNonRoot` adds hardening: runAsNonRoot, drop ALL capabilities, no privilege escalation, RuntimeDefault seccomp. It is off while TS nodes remain.
- `geodata.files` sets the URLs the geodata downloader fetches into `/data`.

Tested in kind (local-path storage, as in k3s), image built from this Dockerfile:

**HA suite** (`internal/kindtest`, `-kind-nonroot`):
- passes under traffic: graceful and hard failover with 0 failed downloads, xray crash 5 (3 s), node SIGKILL 1 (2.2 s), rolling update 1 (5.2 s);
- the same image as root (`-kind-root`) gives identical numbers.

**With panel 3.4.7 and chart defaults:**
- node and xray at uid 65534 with CapEff 0;
- Shadowsocks on 443 and Trojan TLS on 8443 (cert from a 0400 secret);
- the geodata PVC: files left by an earlier root run are read, and the sidecar replaces them;
- a routing rule on `ext:zapret.dat:…` served from `/data`;
- panel `geodata.assets` written to `/data` as 65534.

## Metrics (Go-only, opt-in)

`METRICS_PORT=<port>` makes the node serve Prometheus text format at `GET /metrics`, plain HTTP on its own port. It is off by default, it is not on `NODE_PORT`, and the panel sees nothing new. With it unset the node behaves exactly as described above (no tracker, same generated xray config). The Helm chart wires it as `metrics.enabled` / `metrics.port` (default 9464). Every pod, leader and standbys, gets a container port named `metrics` and `prometheus.io/scrape`, `/port`, `/path` annotations (`metrics.podAnnotations`, on by default), so an annotation-based scraper such as vmagent's `kubernetes-pods` job picks up each pod without extra config. The headless Service also lists a `metrics` port but carries no scrape annotation, so pods aren't scraped twice. The chart's other Services select only the leader.

There are no per-user labels. User traffic is one sum and online users a count, so the series grow with inbounds, outbounds and balancers, not with users.

| Metric | Type | Labels | Source |
|---|---|---|---|
| `remnanode_info` | gauge | `version`, `xray_version` | node build, `xray version` |
| `remnanode_ha_leader`, `remnanode_ha_draining` | gauge | | Lease election (standalone: always leader) |
| `remnanode_xray_up` | gauge | | `GetSysStats` answers |
| `remnanode_xray_process_up`, `remnanode_xray_online` | gauge | | supervisor; node's "started and ready" flag |
| `remnanode_xray_starts_total`, `remnanode_xray_crashes_total` | counter | | supervisor: launches, and exits without a stop request (crash, OOM kill) |
| `remnanode_xray_uptime_seconds`, `_goroutines`, `_memory_alloc_bytes`, `_memory_sys_bytes`, `_gc_total` | gauge/counter | | `GetSysStats` |
| `remnanode_inbound_users` | gauge | `tag` | users the node tracks per inbound |
| `remnanode_xray_online_users` | gauge | | `GetAllOnlineUsers` (users with a live IP; xray never records 127.0.0.1) |
| `remnanode_xray_inbound_traffic_bytes_total`, `remnanode_xray_outbound_traffic_bytes_total` | counter | `tag`, `direction` | `QueryStats` |
| `remnanode_xray_user_traffic_bytes_total` | counter | `direction` | `QueryStats`, summed over users |
| `remnanode_xray_observatory_alive`, `_delay_seconds`, `_last_seen_timestamp_seconds`, `_last_try_timestamp_seconds` | gauge | `outbound` | `ObservatoryService.GetOutboundStatus` |
| `remnanode_xray_observatory_window_probes`, `_window_failures`, `_rtt_seconds{stat}` | gauge | `outbound` (+ `stat`) | burstObservatory's sampling window (what leastLoad decides on) |
| `remnanode_xray_balancer_info` | gauge | `balancer`, `strategy`, `fallback` | config |
| `remnanode_xray_balancer_candidates` | gauge | `balancer` | outbounds matching the selector (prefix match, like xray) |
| `remnanode_xray_balancer_targets`, `remnanode_xray_balancer_target` | gauge | `balancer` (+ `outbound`) | `RoutingService.GetBalancerInfo`: what the strategy picks from now; 0 targets means the fallback |
| `remnanode_xray_balancer_override` | gauge | `balancer`, `outbound` | manual `OverrideBalancerTarget` |
| `remnanode_metrics_collector_success` | gauge | `collector` | 0 when a part of the scrape failed |

How it stays out of the panel's way:
- **Counters the panel resets.** The panel polls stats with `reset:true`, so xray's counters keep dropping to zero. With metrics on, every `QueryStats` (the panel's and the scrape's) goes through one tracker: a reset read moves the drained values into a running total, a scrape reads without reset. The exported value is that total plus the live counter, so it only goes up and the panel still gets every byte. A counter that shrinks or vanishes (xray restart, user removed) has its last value folded in first. The tracker lock spans the gRPC call, so a scrape and a panel reset can't interleave. The totals start at zero when the node process starts, which Prometheus treats as a counter reset.
- **Observatory.** xray only answers `GetOutboundStatus` when the api lists `ObservatoryService`, and refuses to start with that service but no observatory. So the node adds the service to the generated config only when `METRICS_PORT` is set **and** the panel's config has `observatory` or `burstObservatory`. Without `METRICS_PORT` the config stays byte-identical to the TS node's (the parity harness runs that way).
- Scrapes are serialized and time out after 10 s. Each one costs a handful of gRPC calls: `GetSysStats`, three `QueryStats`, `GetAllOnlineUsers`, `GetOutboundStatus`, and one `GetBalancerInfo` per balancer.

`internal/node/metrics_e2e_test.go` checks all of this against a real xray: burstObservatory with two live relays and a dead one behind leastLoad and leastPing balancers, real downloads with panel resets between scrapes, online users (the client leaves from the host's LAN address), and an xray SIGKILL. Every scrape is parsed by a strict reader of the text format.

## Parity checklist

| Area | Item | Checked by |
|---|---|---|
| Transport | mTLS TLS 1.3, client cert against the panel CA, h2 off | api tests, e2e, parity |
| | HKDF SNI (`SNI_VERIFICATION`) | secretkey test vs TS `deriveSni` |
| | RS256 JWT, socket dropped on bad JWT or unknown route | api tests, parity |
| | Case-insensitive, trailing-slash-tolerant routes, HEAD for GET | api tests, parity |
| Bodies | zstd (panel settings, 32 MiB window), gzip, deflate, br requests | api tests vs Node zlib |
| | `{"response":…}` envelope, 201 for POST, Nest error body | parity |
| | zod issues on 400, unknown keys stripped | `TestZodParity` (36 bodies), parity |
| | helmet headers, weak ETag, Vary, br/gzip/deflate negotiation ≥ 1 KiB | `TestNegotiateEncoding`, parity |
| SECRET_KEY | 7 integrity checks, PEM normalisation | secretkey tests vs TS |
| xray | Generated config (API inbound/rules, policy, torrent rules, webhook) | parity (3 states, identical) |
| | No restart on identical hashes, HashedSet bit-exact | e2e, hashedset vs `@remnawave/hashed-set` |
| | Start failure text (s6-svstat format, last log line) | parity |
| | Custom core loader, geodata assets | unit tests |
| Users | add/remove user(s) per protocol incl. SS2022 and Hysteria 2 | e2e against xray, parity |
| Stats | all stats routes, numbers not strings, reset semantics | e2e (zod), parity (byte-equal counters) |
| | online status, user IP lists | parity (client container IP) |
| | `get-system-stats` 500/A010 while xray is down | e2e, parity |
| Plugins | torrent blocker via real BitTorrent handshake, reports, suspension/restore | e2e, parity |
| | ipList and asList ignore lists (ASN database) | e2e, unit, `internal/asn` vs lmdb-js |
| | nftables / drop routes as no-ops (fork) | parity |
| Health | `/node/health/*`, `HEALTH_PORT` `/health/live`, `/health/ready` | unit, kind (probes) |
| HA | Lease election, role labels, failover, hand-off, xray health gate | `internal/ha`, kind |
| | Peer sync: replication, snapshot + journal, plugin | `internal/ha`, kind |

## Fork bugs fixed by design

These are fixed in the Go node. Each one can be patched in the TS fork separately.

1. **SS2022 restore used the wrong key.**
   - TS: `add-users` sends xray `base64(ssPassword)` with cipher 0, but caches the raw password with cipher 6. A restored user gets the wrong key.
   - Go: a stored user definition is the list of xray operations actually applied, so a restore replays the real values. Tested against xray.
2. **Users from the initial config couldn't be restored.**
   - TS: a user that came with `xray/start`, rather than `add-user`, has no definition, so after a torrent-blocker suspension they never come back.
   - Go: the definition is derived from the live xray config (vless, trojan, shadowsocks incl. per-client method, ss2022, hysteria). Tested.
3. **`asList` / CIDR ignore lists never matched.**
   - TS: ignored IPs go through an exact `Set.has`, so ranges from `sharedLists` (`ipList` CIDRs, `asList` prefixes) never match a client IP.
   - Go: prefix matching. `DEFAULT_IGNORED_IPS`' literal `0.0.0.0/0` is deliberately *not* read as a CIDR, since that would exempt every IPv4 client.
4. **Plugin sync wasn't replicated to standbys, nor handed to a catching-up pod.**
   - TS: `/node/plugin/sync` never reaches peers, and the snapshot has no plugin. After a failover the new leader runs without the torrent blocker until the panel syncs again.
   - Go: `pluginSync` is replicated, and the snapshot carries the last plugin sync, applied before xray starts.
5. **Peer sync sent the whole SECRET_KEY in a header, over plain HTTP.**
   - TS: every peer request carries `X-Secret-Key: <SECRET_KEY>`, i.e. the node private key, the CA and the JWT key, readable by anything that sees pod traffic. Replicated user credentials travel in clear too.
   - Go: requests are signed (`X-Rw-Peer: v1 ts=… sig=…`, HMAC-SHA256 under an HKDF-derived key over method, path, timestamp and body hash; ±30 s window, replays refused), and peers speak TLS with the node certificate from SECRET_KEY, verified against the panel CA. Kubelet probes stay plain HTTP on the same port (the first byte tells them apart).
6. **Users added after the last full start were lost on hand-off.** Found by the kind test.
   - TS: the snapshot a new pod catches up from holds only the last `xray/start` request. Users added or removed since (`add-user`, `remove-users`, …) are missing on the new pod, and once it takes over (failover or rollout), those users are cut off until the panel next restarts xray. A replicated user change that reaches a standby whose xray isn't up is also dropped.
   - Go: each node keeps a journal of user operations since its last full start; the snapshot carries it and the catching-up pod replays it after starting xray.
7. **Replication could reorder operations.** TS fires each replication with `void fetch(…)`, so a quick `add-user` + `remove-user` can arrive reversed. Go sends through one ordered queue.
8. **`blockDuration: 0` restored the user at once** (`setTimeout(…, 0)`), although the plugin schema documents 0 as permanent. Go blocks permanently (until a node restart or the panel re-adds the user), on standbys too. The usual value, the panel's 60-minute default, is unaffected.
9. **A torrent-suspended user kept torrenting through their open session.** Found on dev-dedicated.
   - TS: suspension only removes the user from the inbounds (`RemoveUser`). That stops new sessions, but a client holding a mux or XHTTP session keeps sending new requests through it without re-authenticating, so torrenting continues and the detection fires every 5 s (the webhook's deduplication window). The original Remnawave cut such sessions with nftables, which the fork removed.
   - Go: suspension also puts `{"ruleTag": "RW_TB_SUSPENDED_USERS", "user": [<suspended ids>], "outboundTag": "RW_TB_OUTBOUND_BLOCK"}` right after the API rule, through RoutingService (no xray restart, other users untouched); restore removes it, and xray restarts rebuild it. Routing runs per request, so new requests inside an open session are dropped at once. Peer flows that were already established inside the session run until they end; xray offers no way to close them. The e2e test proves it with a mux client, and fails with the rule disabled.

## Known gaps


## Other differences

- **No s6, no internal config socket.** xray gets its config on stdin, and Go supervises it. As with `s6-svc -o`, a crashed xray is not restarted automatically.
- **Internal names.** `INTERNAL_REST_TOKEN`, `INTERNAL_SOCKET_PATH` and `XTLS_API_SOCKET_PATH` are generated in-process when unset, in the same formats as `init-env.sh`.
- **`METRICS_PORT`** (Go-only, off by default) serves Prometheus metrics; see "Metrics".
- **TLS handshake timeout** follows the HTTP header timeout (61 s), not Node's 10 s `handshakeTimeout`.

License: AGPL-3.0-only, like upstream.
