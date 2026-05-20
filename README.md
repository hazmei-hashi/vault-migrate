# vault-migrate

`vault-migrate` copies HashiCorp Vault KV v2 secrets between clusters or namespaces.

Core behavior:
- Walks source KV v2 tree recursively from a base path.
- Replays secret versions in order.
- Mirrors deleted and destroyed version state.
- Copies KV metadata settings (`cas_required`, `max_versions`, `delete_version_after`, `custom_metadata`).
- Supports local state file for incremental re-runs.

Memory model:
- Processes one secret at a time.
- Processes one version at a time.
- Reads version payload, writes payload, then drops local payload reference before next version.

No secret values are written to the state file.

## Requirements

- Go 1.25.5+
- Network reachability to source and destination Vault clusters
- Source and destination tokens with required policies
- KV v2 mounts on both source and destination

## Build

```bash
go build
```

Binary output: `./vault-migrate`.


## Releases

### Creating a Release

Releases are created via GitHub Actions:

1. Navigate to the **Actions** tab in GitHub
2. Select **"Create Release Tag"** workflow
3. Click **"Run workflow"**
4. Select the `main` branch and enter semantic version (e.g., `v1.0.0`)
5. Click **"Run workflow"** button

This triggers:
- Tag creation on `main` branch
- Validation that release tags point to commits reachable from `main`
- `go test ./...` before packaging
- GoReleaser binary builds for multiple platforms
- GitHub release creation and publishing after assets, SHA256 checksums, and artifact attestations are ready

### Available Binaries

Each release includes pre-built binaries for:
- **Linux**: amd64, arm64
- **macOS**: amd64 (Intel), arm64 (Apple Silicon)
- **Windows**: amd64

Binary naming: `vault-migrate-{tag}-{os}-{arch}{extension}`

Examples:
- `vault-migrate-v1.0.0-darwin-arm64`
- `vault-migrate-v1.0.0-windows-amd64.exe`

### Verifying Downloads

Each release includes a combined GoReleaser `checksums.txt`:

```bash
# Download binary and checksums
curl -LO https://github.com/hazmei-hashi/vault-migrate/releases/download/v1.0.0/vault-migrate-v1.0.0-linux-amd64
curl -LO https://github.com/hazmei-hashi/vault-migrate/releases/download/v1.0.0/checksums.txt

# Verify checksum
grep 'vault-migrate-v1.0.0-linux-amd64$' checksums.txt | sha256sum -c -
```

Release binaries also include GitHub artifact attestations. Verify provenance with GitHub CLI:

```bash
gh attestation verify vault-migrate-v1.0.0-linux-amd64 \
  --repo hazmei-hashi/vault-migrate
```

### Manual Tag Creation

Alternatively, create tags manually from a commit already on `main` to trigger builds:

```bash
git tag -a v1.0.0 -m "Release v1.0.0"
git push origin v1.0.0
```

Tags that are not reachable from `main` fail release validation.


## Test

Run unit/integration tests:

```bash
go test ./...
go test -v ./...
go test -cover ./...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

Run E2E tests (Docker required):

```bash
cd test/e2e
docker-compose up -d
E2E_TESTS=1 go test -v -timeout=5m .
docker-compose down -v
```

Helper script also exists:

```bash
cd test/e2e
./run-e2e.sh
```

## Token Policy Requirements

Paths below use your selected mount name in place of `<mount>`.

Source token capabilities:
- `list`, `read` on `<mount>/metadata/*`
- `read` on `<mount>/data/*`

Destination token capabilities:
- `create`, `update` on `<mount>/data/*`
- `create`, `update` on `<mount>/metadata/*`
- `update` on `<mount>/delete/*`
- `update` on `<mount>/destroy/*`
- `delete` on `<mount>/metadata/*` (used when recreating destination secret)

## Usage

### Quick Start

Minimal command:

```bash
./vault-migrate \
  -srcAddr https://vault-source.example.com:8200 \
  -dstAddr https://vault-dest.example.com:8200
```

`-srcAddr` and `-dstAddr` are required. Other missing values prompt interactively.

### Fully Non-Interactive Example

```bash
./vault-migrate \
  -srcAddr https://vault-source.example.com:8200 \
  -srcToken "$SRC_TOKEN" \
  -srcNamespace admin \
  -dstAddr https://vault-dest.example.com:8200 \
  -dstToken "$DST_TOKEN" \
  -dstNamespace admin \
  -tlsSkipVerify \
  -logLevel info
```

After startup, tool prompts for:
- Source KV v2 mount and base path
- Destination KV v2 mount and base path

Path rules:
- Paths are relative to mount.
- Do not include `data/`, `metadata/`, or mount name inside base path values.

### Flags

```bash
  -srcAddr string
        Source cluster API address (required)
  -srcToken string
        Source cluster token
  -srcNamespace string
        Source cluster namespace
  -dstAddr string
        Destination cluster API address (required)
  -dstToken string
        Destination cluster token
  -dstNamespace string
        Destination cluster namespace
  -tlsSkipVerify
        Skip TLS verification of Vault server certificates
  -logLevel string
        Log level: debug, info, warn, error (default "info")
  -mode string
        Mode of operation (default "kvv2")
  -stateFile string
        Path to migration state file (default ".vault-migrate-state.json")
  -noState
        Disable state tracking
  -forceRecopy
        Re-copy when state indicates hashes already match
  -maxRetries int
        Accepted and validated (>=0); currently not enforced as retry cap
  -continueOnError
        Continue migration after per-secret copy errors
  -dryRun
        Show what would be copied without writing destination
```

## Migration Behavior

### Default Mode (`-noState=false`)

State file stores:
- Per-secret status (`completed`, `failed`, `skipped`)
- Version hashes (SHA256)
- Version state (`active`, `deleted`, `destroyed`, `missing`, `read_error`)
- Metadata checksum
- Summary counters

Per-secret behavior:

1. Destination secret missing
- Full copy from version `1..N`.

2. Destination and source have same max version
- If existing state has version hashes and `-forceRecopy=false`, tool can skip.
- If no hash state, tool compares source/destination payload hashes.
- On mismatch, destination secret metadata path is deleted, then full copy runs.

3. Destination has fewer versions than source
- Tool copies only missing tail versions (`dstMax+1..srcMax`).

4. Destination has more versions than source
- Secret marked failed.
- Migration returns error for that secret.
- With `-continueOnError`, migration continues to next secret.

### Legacy Mode (`-noState=true`)

- Tool replays all source versions for every secret every run.
- Existing destination secrets get additional versions.
- Re-run is not idempotent on same destination path.

## Progress and Logging

- `info` level: progress bar on TTY, periodic progress logs on non-TTY.
- `debug` level: per-operation logs (`READ`, `WRITE`, `FULL COPY`, `INCREMENTAL COPY`, `SKIP`, `RECREATE`).

## Limitations

- KV v2 mode only (`-mode kvv2`).
- Source version timestamps are not preserved on destination.
- Destroyed source payloads are unrecoverable; tool writes placeholder then marks destination version destroyed.
- Token renewal is not handled; token TTL must exceed migration duration.
- State file is single-writer; do not share one `-stateFile` across parallel migrations.
- Vault client timeout is 3 seconds per request.

## Security Notes

- Avoid passing tokens directly in shell history; prefer environment variables.
- Interactive token prompts use hidden terminal input.
- State file stores hashes and metadata checksums, not secret values.
