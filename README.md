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

- Go 1.25.5+ (module declares `go 1.25.5`; CI builds/tests on 1.26.x)
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

Test conventions:
- Table-driven tests (`tests := []struct{...}` + `t.Run` subtests) throughout.
- `kvv2/kvv2_mock_test.go` provides `newFakeVault` — an `httptest.Server`-backed
  fake KV v2 backend used by all `kvv2` package unit tests instead of a real
  Vault cluster. It models real Vault's 404-with-data response shape for
  reads against deleted/destroyed versions, per-secret and mount-level
  `max_versions` sliding-window pruning, per-secret and mount-level
  `cas_required` enforcement on data writes (rejecting a write with no
  `options.cas` as `400 check-and-set parameter required for this call`,
  and rejecting a wrong `options.cas` VALUE — not merely its presence — as
  `400 check-and-set parameter did not match the current version`,
  mirroring `path_data.go:283-288`'s unconditional value check), and
  injectable LIST errors, plus a `deleteCalls()` counter used to assert
  idempotency (no destination delete+recopy) across repeated migration
  runs.
- `client/client_test.go` provides a matching `httptest.Server`-backed fake for
  `sys/health` and `auth/token/lookup-self`, covering every `getClient` error
  path (unreachable address, health 5xx, uninitialized, sealed, lookup
  failure) plus regression locks for namespace-before-lookup, max retries, and
  client timeout.
- Stdin-driven prompts (`Init`, `config.Prompt`/`PromptRequired`) are tested via
  a `useStdinInput(t, input)` helper that swaps `os.Stdin` for a pipe pre-loaded
  with the given input and restores it on cleanup.
- True end-to-end tests against a live Vault cluster live in `test/e2e/` and
  are gated behind `E2E_TESTS=1` (skipped otherwise).

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
- `create`, `update`, `read` on `<mount>/metadata/*`
- `read` on `<mount>/data/*` (required to verify existing destination secrets before deciding skip/recreate/incremental)
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

`-srcAddr` and `-dstAddr` are required; `cmd.validateConfig` fatals immediately if either is missing or malformed, before any prompt runs. Every other value not passed as a flag prompts interactively.

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

### Prompt Order

`-srcAddr` and `-dstAddr` are **not actually prompted** despite `client.BuildClients`
containing prompt branches for both: `cmd.validateConfig` fatals before
`BuildClients` ever runs if either is empty, so those two branches are dead
code on the `cmd` entrypoint (kept as a library-safe fallback for callers
that skip `validateConfig` — see `client/client.go`). Pass both as flags.

Any other value not supplied via flag is prompted for, in this exact order
(skipping entries already set by flag):

1. Source Vault token (hidden input)
2. Source namespace (empty = root namespace)
3. Destination Vault token (hidden input)
4. Destination namespace (empty = root namespace)
5. Skip TLS verification? (y/n)
6. Source KV-V2 mount (required; re-prompts on empty or slash-only input)
7. Source KV-V2 base path (empty = root path, legal)
8. Destination KV-V2 mount (required; re-prompts on empty or slash-only input)
9. Destination KV-V2 base path (empty = root path, legal)

Tokens are read via `term.ReadPassword` and require a real TTY (no echo, no
paste-safe fallback). In CI or any non-interactive environment, pass
`-srcToken`/`-dstToken` as flags or via environment-sourced flag values —
piping a token through stdin does not work for the token prompts.

Path rules:
- Paths are relative to mount.
- Do not include `data/`, `metadata/`, or mount name inside base path values.

### Flags

The CLI supports these flags:

| Flag | Default | Purpose |
|---|---|---|
| `-srcAddr` | required | Source cluster API address |
| `-srcToken` | prompt (TTY only) | Source cluster token |
| `-srcNamespace` | prompt | Source cluster namespace |
| `-dstAddr` | required | Destination cluster API address |
| `-dstToken` | prompt (TTY only) | Destination cluster token |
| `-dstNamespace` | prompt | Destination cluster namespace |
| `-tlsSkipVerify` | `false` | Skip TLS verification of Vault server certificates |
| `-mode` | `kvv2` | Mode of operation. Currently only `kvv2` is supported |
| `-logLevel` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `-stateFile` | `.vault-migrate-state.json` | Path to migration state file |
| `-noState` | `false` | Disable state tracking |
| `-forceRecopy` | `false` | Re-copy when state indicates hashes already match |
| `-maxRetries` | `3` | Validated as `>= 0` at startup; wired into the Vault API client's HTTP-level retry policy (`SetMaxRetries` + `retryablehttp.RateLimitLinearJitterBackoff`), so failed/idempotent requests are retried up to this many times, honoring a server's `Retry-After` header on `429`/`503` responses. No app-level per-secret retry loop exists — retries happen only at the HTTP transport layer, below `copyOneSecret` |
| `-clientTimeout` | `60s` | Validated as `> 0` at startup; HTTP client timeout for Vault API requests (`SetClientTimeout`) |
| `-continueOnError` | `false` | Continue migration after per-secret copy errors |
| `-dryRun` | `false` | Show what would be copied without writing to the destination |

If a flag is omitted, the tool prompts for the missing value at runtime, in the order listed under [Prompt Order](#prompt-order). Empty input at the mount prompts is rejected and re-prompted; empty input at the base path or namespace prompts is accepted and means root.

## Migration Behavior

### Default Mode (`-noState=false`)

State file stores:
- Per-secret status (`completed`, `recreated`, `failed`, `skipped`)
- Version hashes (SHA256)
- Version state (`active`, `deleted`, `destroyed`, `missing`, `read_error`) — `deleted` is set when the source version is confirmed genuinely soft-deleted by an actual failed read (Vault's 404-with-data response for that version), never merely because a `deletion_time` field is present: a future-dated `deletion_time` from `delete_version_after` is non-empty while the version is still fully readable, and that case reads real data and is labeled `active`.
- Metadata checksum
- Source and destination version counts — the destination count is **measured** from an actual destination metadata read after copy, not assumed equal to the source count, so it stays accurate even when destination `max_versions` retention prunes below what was written. If that measurement read itself fails, the count falls back to the assumed (source) value and the failure is logged at debug — this bookkeeping never aborts the migration.
- Summary counters

Per-secret behavior:

1. Destination secret missing
- Full copy from version `1..N`.

2. Destination and source have same max version
- If existing state has version hashes and `-forceRecopy=false`, tool can skip.
- If no hash state, tool compares source/destination payload hashes.
- A source version already pruned from the destination's own metadata (by destination `max_versions`) is skipped during comparison, not treated as a mismatch — this avoids a destructive delete-and-recopy loop that would otherwise repeat on every run.
- On genuine mismatch, destination secret metadata path is deleted, then full copy runs.

3. Destination has fewer versions than source
- Tool copies only missing tail versions (`dstMax+1..srcMax`).
- Destination retention governs history depth: if destination `max_versions` is lower than the source version count, only the newest N versions persist there regardless of how many were copied — this is the destination's own KV v2 engine honoring its configured retention, not a migration bug.

4. Destination has more versions than source
- Secret marked failed.
- Migration returns error for that secret.
- With `-continueOnError`, migration continues to next secret.

After any full or incremental copy (cases 1, 2, and 3), if the destination's measured version count ends up lower than the source count just copied, a `Logger.Warn` names the secret key and both the source and measured destination version counts. This is a warning only — the migration does not fail and does not retry: the latest value is written last and always survives destination retention, so only older history is affected.

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
- Soft-deleted source payloads are also unrecoverable at read time (Vault returns no data for them); the tool writes the configured placeholder then marks the destination version deleted to match, instead of writing an empty/null payload.
- Token renewal is not handled; token TTL must exceed migration duration.
- State file is single-writer; do not share one `-stateFile` across parallel migrations.
- State file writes are atomic against a killed process (temp file in the same directory + `os.Rename`), but NOT against power loss — there is no `fsync` before the rename, so a hard crash at the wrong instant can still leave a lost (not corrupted) write on some filesystems.
- Vault client timeout is configurable via `-clientTimeout` (default 60s per request).
- A source mount/base path that resolves to zero secrets (missing mount, KV v1 mount, typo'd mount or base path) makes the migration fail rather than report success; the SDK cannot distinguish those cases from a genuinely empty mount.
- Migrating into a `cas_required` destination is supported via a reactive check-and-set retry: `kv2WriteData` sends the same request as always (no `options.cas`) on the first attempt. If — and only if — that write is rejected with `400 check-and-set parameter required for this call` (destination mount tunable OR destination secret's own `cas_required`), it reads the destination's current version via `<mount>/metadata/*` and retries exactly once with `options.cas` set to that version. On every other destination (the overwhelming majority of real runs) this is byte-identical to the pre-existing wire format — no extra request, no `options` key on the write at all. The one-time extra round trip only happens on a `cas_required` destination, and only after the first write already failed. A genuine concurrent-writer CAS mismatch on the retry is not looped — it propagates as a loud failure, same as any other write error. Requires no new token permission: destination `read` on `<mount>/metadata/*` was already required (see Token Policy Requirements above). Verified against a real Vault cluster (`test/e2e/e2e_test.go`'s `TestE2E_CASRequiredMountDestination`, `TestE2E_CASRequiredPerSecretDestination`, `TestE2E_SourceCASRequiredIncremental`), in addition to mock coverage.

## Security Notes

- Avoid passing tokens directly in shell history; prefer environment variables.
- Interactive token prompts use hidden terminal input (`term.ReadPassword`) and require a TTY; use `-srcToken`/`-dstToken` flags or environment-sourced values in CI or any non-interactive context.
- State file stores hashes and metadata checksums, not secret values.
- Empty mount input is rejected and re-prompted (never silently accepted); empty base path or namespace input is accepted and means root.
