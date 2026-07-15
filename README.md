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
  --enable-flx \
  --internal-api-secret local-secret
```

Then start Go:

```sh
BARQ_DATA_PLANE_URL=http://127.0.0.1:9090 \
BARQ_DATA_PLANE_SECRET=local-secret \
BARQ_API_KEYS='dev-key:dev:default:*' \
go run ./cmd/barq-server
```

The public API listens on `127.0.0.1:8080`. `BARQ_API_KEYS` is used only on the
first start. Barq stores each SHA-256 key digest in `control.barq`; raw keys are
never stored. Later changes to the environment do not recreate a rotated or
revoked key. For local-only work, `BARQ_DEV_MODE=true` bootstraps `dev-key` as a
global development key.

The control console is at `/control/`; embedded Swagger UI is at `/docs/`.
Both ship inside the server binary and need no CDN. A global admin can register
tenants, add logical databases, create scoped service keys, rotate keys, and
revoke keys from the control console. The Sync rules page reads the live Barq
schema, plans normal Barq predicates, applies them without disconnecting
devices, tests one stored row, and restores old revisions. New key values are
shown once. Disabling a tenant stops its scoped keys and webhook polling
without deleting Barq data.

## Self-hosted deployment

`barqctl` hides Docker Compose and creates all local secrets. A release build
downloads `release.json` and uses matching Core and control-plane images pinned
to exact SHA-256 digests:

```sh
curl -fsSL https://github.com/BarqDB/barq-control-plane/releases/download/v1.2.3/install.sh | sh -s -- v1.2.3
```

On Windows PowerShell, download `install.ps1` from the same release and run it
with `-Version v1.2.3`. Both installers check the release archive checksum and
put `barqctl`, Restic, and Cosign in one private user-owned bin directory.

```sh
barqctl init --domain db.example.com
barqctl up
barqctl status
barqctl open
```

Day-to-day maintenance also stays behind `barqctl`:

```sh
barqctl doctor                 # containers, disk, backups, queues, dead letters
barqctl logs --tail 200
barqctl access set             # paste a new admin key after rotating it
barqctl backup
barqctl upgrade --release v1.2.0
barqctl rollback
```

`backup`, `upgrade`, and `rollback` briefly stop Barq so the database files are
copied as one consistent snapshot. Every upgrade and rollback first makes a
local safety backup. If the new containers do not become healthy, `barqctl`
restores both the old image digests and the full pre-upgrade data snapshot.
Before any downtime, it downloads the target images and checks the internal
protocol, Core data format, and control-database migration path. Unsupported
releases are rejected without touching the running stack.

Backups are stored under `~/.barq/backups` by default. They contain the Barq
data volume, deployment settings, keys, SHA-256 checksums, and release history.
Restore checks every file before replacing data and makes one more safety
backup first:

```sh
barqctl restore --backup ~/.barq/backups/20260713T010203Z --yes
```

For encrypted off-site backups, use any S3-compatible service. The release
bundle includes a checked Restic binary, so there is nothing else to install.
Credentials are read from the environment once and then stored in a private
`0600` file:

```sh
export BARQ_BACKUP_ACCESS_KEY='...'
export BARQ_BACKUP_SECRET_KEY='...'
barqctl backup configure \
  --repository s3:https://s3.example.com/my-bucket/barq/client-a \
  --region us-east-1
unset BARQ_BACKUP_ACCESS_KEY BARQ_BACKUP_SECRET_KEY

barqctl backup --remote
barqctl backup check --restore-test
barqctl restore --snapshot latest --yes
```

`backup configure` creates a random encryption key unless
`BARQ_BACKUP_PASSWORD` is set. Copy the shown recovery-key file into a separate
password manager. Losing both the server and that key makes the remote backup
unreadable.

On a Linux server with systemd, one command enables daily encrypted backups and
a weekly full download-and-restore test:

```sh
barqctl backup schedule --daily-at 03:00
```

Remote retention keeps 7 daily, 4 weekly, and 12 monthly snapshots. Three
verified local copies are kept for fast recovery. `barqctl doctor` warns when
an upload is older than 26 hours or a restore test is older than 8 days.

The default deployment directory is `~/.barq`. Set `BARQ_HOME` or pass
`--dir` to use another location. `init` creates:

- a private internal Docker network;
- one Barq data volume shared by Core and the control plane;
- an automatic HTTPS edge using Caddy;
- a random internal API secret and control API key;
- a 3072-bit RSA key pair for signed device sync tokens.

Only ports 80 and 443 are published. The Core internal API is not exposed.
Generated Docker deployments enable FLX sync by default. Standalone Core keeps
FLX off unless `--enable-flx` is passed. `barqctl doctor` checks both the Core
capability and the live rule API.
The generated `.env` and JWT private key use file mode `0600`.
Each domain gets stable Compose project, volume, and network names. The default
bundle owns public ports 80 and 443, so it expects one public client stack per
host. The later SaaS shape is one small isolated host per client, with several
tenants inside that client's stack. The key created by `barqctl init` is the
global admin key for that stack. The `default/default` tenant and database are
registered on first start; more tenants can be added in the control console.

For source builds without a published release, provide both images explicitly:

```sh
go run ./cmd/barqctl init --domain db.example.com --release main \
  --control-image ghcr.io/barqdb/barq-control-plane:main \
  --core-image ghcr.io/barqdb/barq-core:main
```

Tagged releases publish signed GHCR images, SBOM and provenance attestations,
fixed image digests in `release.json`, and `barqctl` binaries for Linux, macOS,
and Windows. Both server images support Linux AMD64 and ARM64. The release
bundle includes Cosign. `barqctl init` and `upgrade`
reject an image unless its digest was signed by the tagged Barq release
workflow. Rollback uses a fixed digest that was verified when first installed,
so emergency recovery does not need the signing service to be online.

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

CI checks out this repository as `server/`, plus the pinned `barq-go` and Core
commits. It runs race tests, vet, native builds, both Docker builds, and a
two-container FLX apply-and-restart test on every pull request and push to
`main`.

## Test

```sh
go test ./...
```

The private data-plane contract is in `contracts/openapi.yaml`. Both Go and C++
use the fixtures under `contracts/fixtures`.

## Current Core integration

The isolated C++ branch implements `health`, additive schema plan/apply, object
create/read/patch/delete, and live FLX rule read/plan/apply/test. Rules are
stored inside each Barq database, use immutable revision snapshots while writes
run, and re-filter connected devices without a reconnect. API writes use server
sync history, so synced clients can download them. Control state belongs to Go
and is opened locally with `barq-go`.

`query`, `batch`, device-origin `changes`, and event `materialize` are still the
next Core slice. Until `changes` is implemented, Core does not advertise that
capability and Go does not start webhook polling. This avoids pretending that
device writes can trigger production webhooks yet.

## Development boundary

This repository does not write to the main Core checkout. Live FLX integration
lives only in `../worktrees/core-flx-rules` on `feature/live-flx-rules`. The old
`core-server` worktree stays untouched. There is no fake runtime data plane;
`BARQ_DATA_PLANE_URL` is required.

Go opens `data/_system/control.barq` directly with
[`github.com/BarqDB/barq-go`](https://github.com/BarqDB/barq-go). Webhook
registrations, immutable revisions, materializations, deliveries, cursors, and
dead letters are versioned BarqDB records. Set `BARQ_CONTROL_PATH` to override
the file location.

Webhook transforms run as QuickJS inside WebAssembly. They receive only the
materialized event context and cannot make network or database calls. Related
data must be declared in the webhook registration.
