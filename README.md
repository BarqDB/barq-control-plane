# Barq Server

Barq Server is the public API and webhook service for BarqDB. It is kept in a
separate repository so work here does not change the active Barq Core checkout.

The Go process talks to the C++ Barq sync process through `/internal/v1`.
Production control records are stored as versioned Barq rows in
`data/_system/control.barq`. There is no SQLite database.

## Run

Start the C++ process from the isolated Core worktree:

```sh
barq-server --root-dir ./data --allow-unsigned-tokens \
  --internal-api-secret local-secret
```

Then start Go:

```sh
BARQ_DATA_PLANE_URL=http://127.0.0.1:9090 \
BARQ_DATA_PLANE_SECRET=local-secret \
BARQ_API_KEYS='dev-key:dev:default:*' \
go run ./cmd/barq-server
```

The public API listens on `127.0.0.1:8080`. Set `BARQ_API_KEYS` to a
comma-separated list of `key:tenant:database:actions` entries. For local-only
work, `BARQ_DEV_MODE=true` enables `dev-key` for tenant `dev`, database
`default`.

The control console is at `/control/`; embedded Swagger UI is at `/docs/`.
Both ship inside the server binary and need no CDN.

`go.mod` pins [`BarqDB/barq-go`](https://github.com/BarqDB/barq-go) and uses the
workspace copy at `../client/barq-go`. Build its native library once before
building the server:

```sh
make -C ../client/barq-go native
```

Build the container from the Barq workspace root so both repositories are in
the Docker context:

```sh
docker build -f server/Dockerfile -t barq-server .
```

CI checks out this repository as `server/` and the pinned `barq-go` commit as
`client/barq-go/`. It runs race tests, vet, a native server build, and a full
Docker build on every pull request and push to `main`.

## Test

```sh
go test ./...
```

The private data-plane contract is in `contracts/openapi.yaml`. Both Go and C++
use the fixtures under `contracts/fixtures`.

## Current Core integration

The isolated C++ branch currently implements `health`, additive schema
plan/apply, and object create/read/patch/delete. API writes use server sync
history, so synced clients can download them. Control state belongs to Go and
is opened locally with `barq-go`.

`query`, `batch`, device-origin `changes`, and event `materialize` are still the
next Core slice. Until `changes` is implemented, Core does not advertise that
capability and Go does not start webhook polling. This avoids pretending that
device writes can trigger production webhooks yet.

## Development boundary

This repository does not write to the main Core checkout. Core integration
lives only in `../worktrees/core-server` on `feature/server-data-plane`. There
is no fake runtime data plane; `BARQ_DATA_PLANE_URL` is required.

Go opens `data/_system/control.barq` directly with
[`github.com/BarqDB/barq-go`](https://github.com/BarqDB/barq-go). Webhook
registrations, immutable revisions, materializations, deliveries, cursors, and
dead letters are versioned BarqDB records. Set `BARQ_CONTROL_PATH` to override
the file location.

Webhook transforms run as QuickJS inside WebAssembly. They receive only the
materialized event context and cannot make network or database calls. Related
data must be declared in the webhook registration.
