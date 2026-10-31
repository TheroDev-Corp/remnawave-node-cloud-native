# go-node: Go port of the Remnawave node

A drop-in replacement for the TS node in this repository. It exposes the same API on `NODE_PORT`, reads the same `SECRET_KEY` and env vars, and runs xray-core as a supervised child process. The stock panel shouldn't be able to tell it apart. The TS node in `../src` is untouched.

**Status: milestones 1–4 of 5.** All 25 contract routes are implemented. The 20 the panel calls are tested end to end against a real xray, a side-by-side run against the published TS image (`3.4.5-HA`) shows 0 differences the panel could observe, and the Helm chart runs the Go node with HA in a kind cluster through failover and rolling updates without dropping a user's traffic.

| Milestone | Scope | State |
|---|---|---|
| 1 | config/env parity, `SECRET_KEY` checks, mTLS TLS 1.3 + RS256 JWT + HKDF SNI, zstd bodies, xray supervisor, health, golden contract schemas | ✅ |
| 2 | all 20 panel endpoints + must-match items (numeric counters, semver `nodeVersion`, non-2xx `get-system-stats` when xray is down, bit-exact HashedSet, atomic reset) | ✅ |
| 3 | TS-vs-Go container parity harness (same `SECRET_KEY`, generated xray config diff, traffic counters) | ✅ |
| 4 | HA layer (Lease election, pod labels, peer sync) with the fork's bugs fixed, tested in kind | ✅ |
| 5 | Dockerfile, resource numbers, parity checklist | ⏳ |

## Layout

```
main.go                 wiring: TLS server, HEALTH_PORT, internal socket, instance lock, shutdown
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
                        (sync, torrent blocker, suspension, pre/post-start), geocheck, internal routes
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
XRAY_BINARY=/path/to/xray go test ./...    # + supervisor tests against a real xray v26.10.21
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

Configuration is the fork's: `K8S_LEADER_ELECTION_ENABLED`, `K8S_LEASE_NAME`, `PEER_HEADLESS_SERVICE`, `HEALTH_PORT` (peer port), `POD_NAME`, `POD_NAMESPACE`. One addition: `PEER_ACCEPT_LEGACY_SECRET_HEADER=true` lets Go pods accept a TS pod's old `X-Secret-Key` during a migration (see fix 5 below). Go pods never send it.

`internal/ha` unit tests run two and three electors against an in-memory API server (failover after a release and after expiry, stale-label cleanup, every yield rule, the health gate) and two peer-sync instances over real TLS sockets (catch-up with plugin and journal, 41 actions replicated in order, unsigned requests refused).

### In kind

`internal/kindtest` installs `charts/remnanode` with two Go replicas in a kind cluster, plays the panel through a port-forward to the leader, and runs an xray client pod that downloads through the leader-only Service as a user added with `add-user` (not in the original config). It probes every 200 ms through each event:

KIND_RESULTS_PLACEHOLDER

```bash
kind create cluster --name rwnode
kind load docker-image --name rwnode remnawave-node-go:dev   # + busybox:1.36.1
KIND_CONTEXT=kind-rwnode KIND_IMAGE=remnawave-node-go:dev go test -v -timeout 30m ./internal/kindtest/
```

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

## Known gaps

- **`blockDuration: 0` restores immediately**, as in the TS fork (`setTimeout(…, 0)`). The schema text says "permanent", so the team should decide.

## Other differences

- **No s6, no internal config socket.** xray gets its config on stdin, and Go supervises it. As with `s6-svc -o`, a crashed xray is not restarted automatically.
- **Internal names.** `INTERNAL_REST_TOKEN`, `INTERNAL_SOCKET_PATH` and `XTLS_API_SOCKET_PATH` are generated in-process when unset, in the same formats as `init-env.sh`.
- **TLS handshake timeout** follows the HTTP header timeout (61 s), not Node's 10 s `handshakeTimeout`.

License: AGPL-3.0-only, like upstream.
