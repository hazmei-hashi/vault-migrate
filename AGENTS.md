# AGENTS.md

Agent instructions for `vault-migrate` - a Go CLI tool for migrating HashiCorp Vault KV v2 secrets.

Terse like caveman. Technical substance exact. Only fluff die.

Drop: articles, filler (just/really/basically), pleasantries, hedging.

Fragments OK. Short synonyms. Code unchanged.

Pattern: [thing] [action] [reason]. [next step].

ACTIVE EVERY RESPONSE. No revert after many turns. No filler drift.

Code/commits/PRs: normal. Off: "stop caveman" / "normal mode".

ALWAYS use rtk command to proxy CLI commands. For example: `rtk ls -l` to list all the files.

ALWAYS update the README.md at the end of any changes.

Mark TODO as done when it is completed.

Ensure there's sufficient testing for potential use case.

DO NOT use directories outside of here. For temporary files, use .tmp/ directory in this repository.

Fork: origin = hazmei-hashi/vault-migrate, upstream parent = czembower/vault-migrate. ALWAYS PR against origin main, NEVER upstream. `gh pr create` defaults base to upstream on forks — pass explicit: `gh pr create --repo hazmei-hashi/vault-migrate --base main`. Verify after: `isCrossRepository` must be `false`.

You may use the directory /Users/hazmei/Documents/Obsidian/localvault/ to access the persistent Obsidian knowledge vault.

## Build & Run

```bash
go build                 # produces vault-migrate binary
go run main.go          # run directly
./vault-migrate -h      # show available flags
```

## Interactive Prompts

The tool prompts for any missing flag values at runtime:
- Tokens are read securely via `term.ReadPassword` (no echo)
- Source/destination addresses, namespaces, mount paths, and base paths
- Can provide all values via flags to avoid prompts

Example with all flags:
```bash
./vault-migrate \
  -srcAddr https://vault-src:8200 \
  -srcToken $SRC_TOKEN \
  -srcNamespace admin \
  -dstAddr https://vault-dst:8200 \
  -dstToken $DST_TOKEN \
  -dstNamespace admin \
  -tlsSkipVerify \
  -logLevel debug
```

## Security

**CRITICAL**: Never log, print, or commit Vault tokens. The codebase uses `term.ReadPassword` for secure input and debug logs explicitly avoid token values.

## Package Structure

- `main.go` → `cmd/cmd.go` → orchestrates client setup and mode selection
- `client/client.go` - Vault API client builder with health checks
- `config/config.go` - Config structs and logger setup
- `config/prompt.go` - Shared stdin reader for all line-based prompts (`Prompt`, `PromptRequired`)
- `kvv2/` - KV v2 migration logic (init, walk, copy)

Entry flow: `main.Init()` → `client.BuildClients()` → `kvv2.Init()` → `Migrator.Run()`

## Testing

Conventions:
- Table-driven tests (`tests := []struct{...}` + `t.Run` subtests) throughout.
- `kvv2/kvv2_mock_test.go` provides `newFakeVault`, an `httptest.Server`-backed
  fake KV v2 backend used for all `kvv2` package unit tests — no live Vault
  required. It serves real Vault's 404-with-data shape for reads against
  deleted/destroyed versions (not a bare error) and enforces per-secret and
  mount-level `max_versions` sliding-window pruning on write, so pruning- and
  soft-delete-related bugs are reachable by the mock instead of masked by it.
- `client/client_test.go` provides a matching `httptest.Server`-backed fake
  for `sys/health` and `auth/token/lookup-self`, covering every `getClient`
  error path plus regression locks for namespace-before-lookup, max retries,
  and client timeout. First test file for `client/` (0% -> ~52.6% coverage).
- Stdin-driven prompts are tested via a `useStdinInput(t, input)` helper
  (defined in `kvv2/kvv2_mock_test.go` and mirrored in `config/prompt_test.go`)
  that swaps `os.Stdin` for a pipe pre-loaded with input and restores it on
  cleanup.
- E2E tests (`test/e2e/e2e_test.go`) exercise the real binary against a live
  Vault cluster (Docker Compose) and are gated behind `E2E_TESTS=1`; they
  no-op otherwise.

Commands:
```bash
go test ./...
go test -v ./...
go test -cover ./...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# E2E (Docker required)
cd test/e2e && docker-compose up -d
E2E_TESTS=1 go test -v -timeout=5m .
docker-compose down -v
```

## Notes

- Only `kvv2` mode is implemented (source/destination KV v2 secrets engines)
- Tokens are not renewed; TTL must exceed migration duration
- Client timeout is configurable via `-clientTimeout` flag, default 60s (see `cmd/cmd.go:30`, wired through `client/client.go`'s `getClient`/`SetClientTimeout`)
- Go module version: 1.25.5 (runs on 1.26+)
