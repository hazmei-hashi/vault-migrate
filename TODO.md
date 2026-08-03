# TODO

Active backlog only. Obsolete historical notes removed.

## Current Baseline (2026-08-03)

- 168 tests passing across 7 packages
- Coverage: 67.5% total (`kvv2`: 76.6%, `config`: 100%, `state`: 91.1%, `cmd`: 33.3%)
- Phases 1-4 complete (unit, integration, mock harness, E2E)
- Prompt desync bug fixed: shared `config.Prompt`/`PromptRequired` replaces
  `fmt.Scan`/`bufio.Scanner` mix in `client.go` and `kvv2/init.go`

## Active Items

### P1: CI Hardening
- [x] Add GitHub Actions workflow for `go test ./...`
- Add coverage gate (fail build if coverage drops below target)
- Add coverage artifact/report publishing
- Optional: add test matrix for multiple Go versions

### P2: Rollback Capability
- Add `-rollback` mode
- Read state file and delete destination secrets listed in state
- Add dry-run/confirmation behavior for rollback flow

### P3: Concurrency
- Add worker-pool based copy mode (configurable worker count)
- Preserve per-secret version order while parallelizing across secrets
- Add retry/rate-limit handling under concurrency

### P4: Placeholder Differentiation
- Distinguish placeholder reasons clearly:
  - `missing_in_metadata`
  - `source_version_unavailable`
  - `read_error`
  - `destroyed`
- Improve logs/state to reflect exact reason paths

### P5: Coverage Follow-up (Optional)
- Improve bootstrap coverage (`main`, `client`, CLI wiring)
- Add targeted failure-path tests where defects appear

### P6: Deferred Bugs (found during prompt-desync fix, out of scope this round)

- [ ] B6: version-replay pruning data loss — `copyOneSecret`/`copySecretFull`/
  `copyIncrementalVersions` (`kvv2.go:138-149, 307-319, 417-429`) write a
  placeholder for any version missing from `meta.Data.Versions` but never
  distinguish "never existed" from "pruned by `max_versions`/CAS on
  destination write" — potential silent data loss on tight `max_versions`
  destinations. Needs investigation before touching (explicitly out of
  scope for the prompt-desync fix).
- [ ] B8: configurable client timeout — `client/client.go:119` hardcodes
  `client.SetClientTimeout(3 * time.Second)`. Large secrets/slow networks can
  legitimately exceed 3s; make this a flag (`-clientTimeout`) with a sane
  default.
- [ ] B9: mount-existence preflight — `kvv2.Init` never checks that the
  source/destination KV v2 mounts actually exist before walking/copying;
  first real signal is an opaque error mid-migration. Add a preflight
  `Sys().MountConfig`/`ListMounts` check with a clear error.
- [ ] B10: dead srcAddr/dstAddr prompts — `client.BuildClients` still prompts
  for `srcAddr`/`dstAddr` when unset via flag, but `cmd.validateConfig`
  already fatals before `BuildClients` runs if either is empty, so those two
  prompt branches are unreachable in practice via the `cmd` entrypoint.
  Confirm intent (library-safe fallback vs dead code) and either document or
  remove.
- [ ] B11: non-atomic state write — `state.Save` (see `state/state.go`)
  writes the state file directly, not via temp-file + rename; a process
  kill mid-write can corrupt the state file used for incremental re-runs.
- [ ] B13: `log.Fatal`/`log.Fatalf` in library funcs — `client/client.go`'s
  `getClient` (health check, token lookup, `api.NewClient` failure) calls
  `log.Fatal*` instead of returning an error, making `client` unusable as a
  library and untestable without exiting the test process.
- [ ] B14: unused `-maxRetries` flag — validated as `>= 0` in
  `cmd.validateConfig` but no retry loop anywhere reads `c.MaxRetries`; either
  wire it into `copyOneSecretWithState`/`copyOneSecret` retry behavior or
  remove the flag. Documented as no-effect in README for now.
- [ ] B16: `trimSlashes` single-slash strip — `kvv2/helpers.go:30-35` only
  strips one leading/trailing `/` (`TrimPrefix`/`TrimSuffix`, not `Trim`), so
  a base path like `//app/` normalizes to `/app` instead of `app`. Out of
  scope this round per explicit instruction not to touch `helpers.go:30-35`.
- [ ] B17: `isNotFound` substring matching — `kvv2/helpers.go`'s `isNotFound`
  matches "404"/"not found" as substrings of the error message, so a genuine
  500 whose *path* happens to contain "404" (e.g. key `app/error-404`) is
  misread as not-found. PR #8 added `errMetadataNotFound` as a structural
  sentinel for the nil/nil "empty metadata" case (`errors.Is`), but that only
  hardens the one path that produces that sentinel — the substring matcher
  underneath is untouched and still has this gap for every other caller of
  `isNotFound`. Proper fix: `errors.As` on `*api.ResponseError` and check
  `StatusCode == 404` instead of string-matching the message.

## Completed Snapshot (Trimmed)

- State tracking + incremental migration complete
- UX improvements complete (progress, prompts, dry-run)
- Mock-based migration tests complete (`kvv2/kvv2_mock_test.go`)
- E2E test suite complete and passing (Vault 1.18.5)
- Prompt desync bug fixed (`fmt.Scan`/`bufio.Scanner` unified into
  `config.Prompt`/`PromptRequired`); regression tests added
  (`TestInit_EmptyMount_Rejected`, `config/prompt_test.go`)
- B1 (token logged in plaintext via `logger.Debug`) fixed
- B5 (namespace set after token lookup, fatal for namespace-scoped Enterprise
  tokens) fixed
- B7 (403/500/timeout on destination metadata read misread as "destination
  absent") fixed
- B12 (`ConfigureTLS` error ignored) fixed
- B15 (`%b` formatting of `time.Duration` TTL) fixed

