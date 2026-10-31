# go-node: Go port of the Remnawave node

A drop-in replacement for the TS node in this repository. It exposes the same API on `NODE_PORT`, reads the same `SECRET_KEY` and env vars, and runs xray-core as a supervised child process. The stock panel shouldn't be able to tell it apart. The TS node in `../src` is untouched.

**Status: milestone 1 of 5 (skeleton).** The full parity checklist, resource numbers and Dockerfile come with milestone 5.

| Milestone | Scope | State |
|---|---|---|
| 1 | config/env parity, `SECRET_KEY` checks, mTLS TLS 1.3 + RS256 JWT + HKDF SNI, zstd bodies, xray supervisor, health, golden contract schemas | ✅ |
| 2 | all 20 panel endpoints + must-match items (numeric counters, semver `nodeVersion`, non-2xx `get-system-stats` when xray is down, bit-exact HashedSet, atomic reset) | ⏳ |
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
internal/node           route registration
internal/testpki        panel-shaped PKI, SECRET_KEY and API JWT, for tests and the parity harness
internal/contract       golden JSON Schemas of libs/contract + drift test
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

## Differences so far

- **No s6, no internal config socket.** xray gets its config on stdin, and Go supervises it. As with `s6-svc -o`, a crashed xray is not restarted automatically.
- **Internal names.** `INTERNAL_REST_TOKEN`, `INTERNAL_SOCKET_PATH` and `XTLS_API_SOCKET_PATH` are generated in-process when unset, in the same formats as `init-env.sh`.
- **TLS handshake timeout** follows the HTTP header timeout (61 s), not Node's 10 s `handshakeTimeout`.

License: AGPL-3.0-only, like upstream.
