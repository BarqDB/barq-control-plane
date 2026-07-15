# Barq Server

Barq Server is the public API and webhook service for BarqDB. `barqctl` runs it
on your own machine: it wraps Docker Compose, creates every secret locally, and
pins Core and control-plane images to signed SHA-256 digests.

- [Self-hosted deployment](#self-hosted-deployment) — install and run Barq.
- [Development](#development) — build and test this repository from source.

The control console is at `/control/` and the Swagger UI is at `/docs/`. Both
ship inside the server binary and need no CDN.

## Self-hosted deployment

### Requirements

- **Docker Engine** with the **Compose v2** plugin (`docker compose version`).
- **A hostname already pointing at this server.** Barq requests its own
  certificate on first start, which only works once the DNS record resolves
  here.
- **Ports 80 and 443 free and reachable**, both for the certificate and for
  traffic. If something else already owns them, see
  [Behind an existing reverse proxy](#behind-an-existing-reverse-proxy).
- Linux, macOS, or Windows. Scheduled backups need Linux with systemd.

### 1. Install barqctl

Pick the newest version from the
[releases page](https://github.com/BarqDB/barq-control-plane/releases) and use
it in both places:

```sh
curl -fsSL https://github.com/BarqDB/barq-control-plane/releases/download/v0.1.1/install.sh | sh -s -- v0.1.1
```

On Windows PowerShell, download `install.ps1` from the same release and run it
with `-Version v0.1.1`.

Both installers verify the release archive checksum and put `barqctl`, Restic,
and Cosign in one private user-owned bin directory, `~/.local/bin` by default.
If `barqctl` is not found afterwards, that directory is not on your `PATH`:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

### 2. Create the deployment

```sh
barqctl init --domain db.example.com
```

`init` downloads `release.json`, checks that both images were signed by the
tagged Barq release workflow, and refuses anything unsigned. It then creates:

- a private internal Docker network;
- one Barq data volume shared by Core and the control plane;
- an automatic HTTPS edge using Caddy;
- a random internal API secret and control API key;
- a 3072-bit RSA key pair for signed device sync tokens.

**`init` prints a global admin API key once.** Save it now; it also lives in the
private `.env` file. It is the global admin key for this stack, and you sign in
to the control console with it.

`--release` defaults to the version of `barqctl` you installed, so it matches by
default. Use `--dir` or `BARQ_HOME` to deploy somewhere other than `~/.barq`.

### 3. Start it

```sh
barqctl up
barqctl status
barqctl doctor
```

Barq is now at `https://db.example.com/control/`. On a desktop, `barqctl open`
launches it; on a headless server, browse to the URL yourself.

The `default/default` tenant and database are registered on first start. Add
more tenants from the control console.

### Behind an existing reverse proxy

A host that already terminates TLS for other sites cannot give ports 80 and 443
to the bundle. Point three `.env` settings at a loopback port instead, and let
the existing proxy forward to it:

```sh
BARQ_SITE_ADDRESS=:80
BARQ_HTTP_BIND=127.0.0.1:8090
BARQ_HTTPS_BIND=127.0.0.1:8443
```

Run `barqctl up` again afterwards. `BARQ_SITE_ADDRESS` replaces the
`https://<domain>` site address, so the edge serves plain HTTP and never
requests a certificate. The outer proxy owns TLS and must forward `/barq-sync`
as well, because device sync uses that path. With Caddy, one line covers both:

```
db.example.com {
	reverse_proxy 127.0.0.1:8090
}
```

`barqctl upgrade` rewrites `compose.yaml` and the `Caddyfile` from the release
bundle, so keep this configuration in `.env`, which upgrades preserve.

### Encrypted off-site backups

Local backups need no setup and are written under `~/.barq/backups`:

```sh
barqctl backup
```

For off-site copies, use any S3-compatible service — AWS S3, MinIO, or a hosted
equivalent. The release bundle includes a checked Restic binary, so there is
nothing else to install. Restic encrypts before upload, so the storage provider
only ever holds ciphertext. Credentials are read from the environment once and
then stored in a private `0600` file:

```sh
export BARQ_BACKUP_ACCESS_KEY='...'
export BARQ_BACKUP_SECRET_KEY='...'
barqctl backup configure \
  --repository s3:https://s3.example.com/my-bucket/barq/client-a \
  --region us-east-1
unset BARQ_BACKUP_ACCESS_KEY BARQ_BACKUP_SECRET_KEY

barqctl backup --remote
barqctl backup check --restore-test
```

Give the credentials access to that one bucket rather than the whole account.
Against a self-hosted MinIO the repository looks the same:

```sh
barqctl backup configure --repository s3:https://minio.example.com/barq-backups/client-a
```

**`backup configure` creates a random encryption key** unless
`BARQ_BACKUP_PASSWORD` is set, and prints where it saved it. Copy that
recovery-key file into a separate password manager. Losing both the server and
that key makes the remote backup unreadable.

Storing backups on the same machine as the deployment protects against a bad
upgrade, not against losing the machine. For real disaster recovery, point the
repository at storage somewhere else.

On a Linux server with systemd, one command enables daily encrypted backups and
a weekly full download-and-restore test:

```sh
sudo barqctl backup schedule --daily-at 03:00
```

This writes system timers to `/etc/systemd/system`, so it needs root. System
timers keep running after logout and start again after a reboot; the units run
as the account that owns the deployment directory.

Remote retention keeps 7 daily, 4 weekly, and 12 monthly snapshots. Three
verified local copies are kept for fast recovery. `barqctl doctor` warns when an
upload is older than 26 hours or a restore test is older than 8 days.

### Restore

Restore checks every file before replacing data and makes one more safety backup
first:

```sh
barqctl restore --backup ~/.barq/backups/20260713T010203Z --yes   # local
barqctl restore --snapshot latest --yes                           # off-site
```

### Upgrades and rollback

```sh
barqctl upgrade --release v0.1.1
barqctl rollback
```

Every upgrade and rollback first makes a local safety backup. If the new
containers do not become healthy, `barqctl` restores both the old image digests
and the full pre-upgrade data snapshot. Before any downtime it downloads the
target images and checks the internal protocol, Core data format, and
control-database migration path; unsupported releases are rejected without
touching the running stack. Rollback uses a fixed digest that was verified when
first installed, so emergency recovery does not need the signing service to be
online.

`backup`, `upgrade`, and `rollback` briefly stop Barq so the database files are
copied as one consistent snapshot.

### Day-to-day

```sh
barqctl doctor                 # containers, disk, backups, queues, dead letters
barqctl logs --tail 200
barqctl status
barqctl access set             # paste a new admin key after rotating it
```

Start with `barqctl doctor`. It checks file permissions, image pins, disk,
container health, the public endpoint, FLX sync, webhook queues, and backup
freshness, and names whatever is wrong. `barqctl logs --tail 200` shows the rest.

### What you get

The default deployment directory is `~/.barq`; set `BARQ_HOME` or pass `--dir`
to use another location. It holds `compose.yaml`, the `Caddyfile`, deployment
settings, secrets, and backups. The generated `.env` and JWT private key use
file mode `0600`.

Only ports 80 and 443 are published, and the Core internal API is never exposed.
Docker deployments enable FLX sync by default. Each domain gets stable Compose
project, volume, and network names, so several stacks can share a host as long
as only one owns the public ports. The later SaaS shape is one small isolated
host per client, with several tenants inside that client's stack.

Backups contain the Barq data volume, deployment settings, keys, SHA-256
checksums, and release history.

### Using the control console

A global admin can register tenants, add logical databases, create scoped
service keys, rotate keys, and revoke keys. New key values are shown once.
Disabling a tenant stops its scoped keys and webhook polling without deleting
Barq data.

The Sync rules page reads the live Barq schema, plans normal Barq predicates,
applies them without disconnecting devices, tests one stored row, and restores
old revisions.

Barq stores each SHA-256 API key digest in `control.barq`; raw keys are never
stored. `BARQ_API_KEYS` is read only on the first start, so later changes to the
environment do not recreate a rotated or revoked key.

## Development

This repository is kept separate from the active Barq Core checkout so work here
does not change it. The Go process talks to the C++ Barq sync process through
`/internal/v1`. Production control records are stored as versioned Barq rows in
`data/_system/control.barq`. There is no SQLite database.

### Run from source

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

The public API listens on `127.0.0.1:8080`. For local-only work,
`BARQ_DEV_MODE=true` bootstraps `dev-key` as a global development key.
Standalone Core keeps FLX off unless `--enable-flx` is passed; `barqctl doctor`
checks both the Core capability and the live rule API.

### Build

`go.mod` pins [`BarqDB/barq-go`](https://github.com/BarqDB/barq-go) and uses the
workspace copy at `../client/barq-go`. Build its native library once before
building the server:

```sh
make -C ../client/barq-go native
```

Build the container from the Barq workspace root so both repositories are in the
Docker context:

```sh
docker build -f server/Dockerfile -t barq-server .
```

For a deployment without a published release, provide both images explicitly:

```sh
go run ./cmd/barqctl init --domain db.example.com --release main \
  --control-image ghcr.io/barqdb/barq-control-plane:main \
  --core-image ghcr.io/barqdb/barq-core:main
```

### Test

```sh
go test ./...
```

The private data-plane contract is in `contracts/openapi.yaml`. Both Go and C++
use the fixtures under `contracts/fixtures`.

CI checks out this repository as `server/`, plus the pinned `barq-go` and Core
commits. It runs race tests, vet, native builds, both Docker builds, and a
two-container FLX apply-and-restart test on every pull request and push to
`main`.

### Releases

Tagged releases publish signed GHCR images, SBOM and provenance attestations,
fixed image digests in `release.json`, and `barqctl` binaries for Linux, macOS,
and Windows. Both server images support Linux AMD64 and ARM64. The release
bundle includes Cosign. `barqctl init` and `upgrade` reject an image unless its
digest was signed by the tagged Barq release workflow.

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
