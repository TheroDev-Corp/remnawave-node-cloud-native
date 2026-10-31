# Agent guide: go-node

Read this before changing anything in `go-node/`. It is written for coding agents (Claude Code and others) and for people who drive them.

## What this is

`go-node/` is a Go port of the Remnawave node that lives in `../src` (NestJS, TypeScript). It must stay a drop-in replacement: the stock Remnawave panel must not be able to tell the two apart. Same API on `NODE_PORT` (mTLS + RS256 JWT, zstd bodies), same `SECRET_KEY`, same env vars, same xray config generation, same answers. On top of that it runs the fork's HA layer (k8s Lease election, peer sync) with the fork's bugs fixed.

The TS node is the reference. **Do not modify `../src`.** When behaviour is unclear, read the TS source and match it; when the TS behaviour is a bug, fix it in Go and document it in `README.md` under "Fork bugs fixed by design".

`README.md` has the full picture: layout, test matrix, parity checklist, HA design, fixed fork bugs, resource numbers. This file is the short operational version.

## Layout

```
main.go              wiring (TLS server, HEALTH_PORT, METRICS_PORT, internal socket, HA, shutdown)
internal/api         mTLS server, Nest-compatible router, bodies, envelope, helmet/ETag, compression
internal/contract    JSON Schemas exported from libs/contract + zod-faithful request validator
internal/node        the Nest modules: xray start/stop, handler, stats, plugins, geocheck, peer adapter;
                     metrics.go + traffic.go: the Go-only METRICS_PORT /metrics
internal/xray        xray child process + gRPC client
internal/ha          Lease election, pod role labels, peer sync (TLS + HMAC), hand-off
internal/asn         pure-Go reader of asn-prefixes.lmdb (lmdb-js format)
internal/secretkey   SECRET_KEY decode + checks, HKDF SNI
internal/hashedset   bit-exact @remnawave/hashed-set
internal/parity      TS-vs-Go container harness (Docker)
internal/kindtest    Helm chart + Go node in kind (HA under traffic)
testdata/node        fixture.mjs: runs the TS node's own code for cross-checks
Dockerfile           scratch image: Go binary + xray-core + geocheck + ASN db
```

## Commands

Run from `go-node/`.

```bash
go build ./... && go vet ./...
go test ./...                                   # unit + TLS end-to-end, ~10 s
XRAY_BINARY=/path/to/xray go test ./...         # + real-xray e2e (internal/node, internal/xray)
ASN_DB=/path/to/asn-prefixes.lmdb go test ./internal/asn/   # vs lmdb-js, all entries
```

Cross-checks against the TS code (`internal/contract`, `internal/api`, `internal/hashedset`, …) need `node` and `npm ci` in the repo root; they skip otherwise. If `~/.npm` isn't writable, use `npm ci --cache <tmpdir>`.

Heavy suites, one at a time (they load the machine hard; stop colima afterwards):

```bash
# Container parity vs the published TS image (needs Docker; amd64 TS image)
PARITY_TS_IMAGE=ghcr.io/therodev-corp/remnawave-node-cloud-native:3.4.5-HA \
PARITY_GO_IMAGE=<go image> PARITY_REPORT=/tmp/parity.md go test -v -run TestParity ./internal/parity/

# HA in kind (image and busybox:1.36.1 loaded with `kind load`)
KIND_CONTEXT=kind-rwnode KIND_IMAGE=<go image> go test -v -timeout 30m ./internal/kindtest/
```

Must pass before any push: `go vet ./...` and `go test ./...` (with `XRAY_BINARY` when touching `internal/node` or `internal/xray`). The parity harness must stay at 0 diffs when touching anything the panel sees.

## Environment notes (this Mac)

- Docker runs in colima. For amd64 images (the TS image is amd64 only) use the Rosetta profile: `colima start -p rwparity --vm-type vz --vz-rosetta`. Under plain qemu, xray crashes at random.
- The colima VM has no direct internet; the host goes out through `http://127.0.0.1:2080`. So image builds can't download inside the VM. Prepare artifacts on the host and override the download stages:
  `docker-buildx build --build-context build=… --build-context xray=… --build-context geocheck=… --build-context asn=… go-node` (the folder layout each stage produces is in the Dockerfile).
- GitHub CI has internet and builds the Dockerfile as is.
- Stop colima when done (`colima stop -p rwparity`, `kind delete cluster --name rwnode`); the tests heat the laptop.

## Git and release

- Work branch: `feat/go-node`. Remote branch `dev` builds `ghcr.io/therodev-corp/remnawave-node-cloud-native:dev` from `go-node/Dockerfile` (`.github/workflows/build-dev.yml`). Pushing to `dev` deploys the dev image.
- Push with fast-forwards only. Never force-push, never delete remote branches, never merge PRs.
- Commit messages: conventional (`feat(go-node): …`, `fix(go-node): …`), body says what and why.
- Keep commits clean: one logical change per commit, tests green.

## Conventions

- Parity first. A change in an answer the panel sees needs a TS reference (file + function) or a documented fork fix.
- Log lines mirror the TS node's wording where they exist.
- Tests next to the code; cross-language checks go through `testdata/node/fixture.mjs`.
- No cgo. The binary is static and runs on `scratch`.
- Metrics (`METRICS_PORT`, README "Metrics") are Go-only and opt-in. With the port unset, nothing the panel sees may change, including the generated xray config. Stats reads go through `Node.queryTraffic`, never `XAPI.QueryTraffic` directly, so the panel's `reset:true` doesn't break the exported counters. No per-user labels.

## Running an agent from the console

`go-node/scripts/agent.sh` starts Claude Code on this repo without GUI approval prompts. Permissions come from `go-node/scripts/agent-settings.json`: allowed are file edits and the dev tools above; denied are force-push, branch deletion, PR merge, `sudo` and hard resets. Anything not allowed is refused instead of waiting for a click.

```bash
go-node/scripts/agent.sh "run the unit tests and fix what fails"   # one-shot, prints the result
go-node/scripts/agent.sh -c "now push it to dev"                     # continue the last run
go-node/scripts/agent.sh                                             # interactive
```
