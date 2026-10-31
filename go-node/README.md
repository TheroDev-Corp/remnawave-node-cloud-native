# go-node: Go port of the Remnawave node

A drop-in replacement for the TS node in this repository. It exposes the same API on `NODE_PORT`, reads the same `SECRET_KEY` and env vars, and runs xray-core as a supervised child process. The stock panel shouldn't be able to tell it apart. The TS node in `../src` is untouched.

**Status: milestones 1–2 of 5.** All 25 contract routes are implemented. The 20 the panel calls are tested end to end against a real xray. The full parity checklist, resource numbers and Dockerfile come with milestone 5.

| Milestone | Scope | State |
|---|---|---|
| 1 | config/env parity, `SECRET_KEY` checks, mTLS TLS 1.3 + RS256 JWT + HKDF SNI, zstd bodies, xray supervisor, health, golden contract schemas | ✅ |
| 2 | all 20 panel endpoints + must-match items (numeric counters, semver `nodeVersion`, non-2xx `get-system-stats` when xray is down, bit-exact HashedSet, atomic reset) | ✅ |
| 3 | TS-vs-Go container parity harness (same `SECRET_KEY`, generated xray config diff, traffic counters) | ⏳ |
| 4 | HA layer (Lease election, pod labels, peer sync) with the fork's bugs fixed | ⏳ |
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
internal/ha             leader state (standalone for now)
internal/hashedset      bit-exact @remnawave/hashed-set (JS int32 hex quirks included)
internal/node           the Nest modules: xray start/stop/healthcheck + config generation + custom core,
                        handler (add/remove user(s)), stats, system info, network sampler, plugins
                        (sync, torrent blocker, suspension, pre/post-start), geocheck, internal routes
internal/testpki        panel-shaped PKI, SECRET_KEY and API JWT, for tests and the parity harness
internal/contract       embedded JSON Schemas of libs/contract and @remnawave/node-plugins (exported from
                        their zod schemas), used to validate requests/plugin configs; drift test
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
- `internal/node/e2e_test.go` plays the panel (mTLS, JWT, zstd) against a real xray with VLESS, Trojan, Shadowsocks and SS2022 inbounds. It calls every route the panel uses, and every 2xx body is validated by the TS contract's own zod schemas (31 responses). It also checks:
  - `get-system-stats` answers 500/A010 while xray is down.
  - `nodeVersion` is semver ≥ 2.7.0 per the `semver` package.
  - A push with identical hashes, or after incremental add/remove-user, does **not** restart xray (same pid).
  - Users really land in, and leave, each inbound, per xray's own API.
  - `reset:true` zeroes the counters.
  - The torrent blocker regenerates the config.
  - Suspension and restore work, with the fork's bugs fixed (below).

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
4. **Plugin sync wasn't replicated to standbys.** The Go node sends `pluginSync` through the replication hook. The HA transport lands in milestone 4.

The fourth listed fork bug (the SECRET_KEY header in peer sync) is about the HA transport and is handled in milestone 4.

## Known gaps

- **ASN lookups (`asList`) have no data source yet.** The TS node reads an lmdb-js database that Go can't read without cgo. Until a replacement lands, AS numbers resolve to nothing, as on a lookup miss in TS.
- **Hysteria add-user is implemented but not covered by the e2e test.** Its inbound needs TLS material.
- **`blockDuration: 0` restores immediately**, as in the TS fork (`setTimeout(…, 0)`). The schema text says "permanent", so the team should decide.

## Differences so far

- **No s6, no internal config socket.** xray gets its config on stdin, and Go supervises it. As with `s6-svc -o`, a crashed xray is not restarted automatically.
- **Internal names.** `INTERNAL_REST_TOKEN`, `INTERNAL_SOCKET_PATH` and `XTLS_API_SOCKET_PATH` are generated in-process when unset, in the same formats as `init-env.sh`.
- **TLS handshake timeout** follows the HTTP header timeout (61 s), not Node's 10 s `handshakeTimeout`.

License: AGPL-3.0-only, like upstream.
