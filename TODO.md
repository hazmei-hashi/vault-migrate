# TODO

Active backlog only. Obsolete historical notes removed.

## Current Baseline (2026-08-04)

- 258 tests passing across 7 packages (E2E gated behind `E2E_TESTS=1`, 6
  scenarios verified against a real Vault 1.18.5 cluster this session,
  not counted in the 258)
- Coverage: `client` 52.6%, `cmd` 35.3%, `config` 100.0%, `kvv2` 80.6%, `state` 85.5%
- Phases 1-4 complete (unit, integration, mock harness, E2E)
- Prompt desync bug fixed: shared `config.Prompt`/`PromptRequired` replaces
  `fmt.Scan`/`bufio.Scanner` mix in `client.go` and `kvv2/init.go`
- Harness realism lesson: `kvv2/kvv2_mock_test.go` used to be more forgiving
  than real Vault (bare errors instead of 404-with-data for soft-deleted/
  destroyed version reads, no `max_versions` pruning enforcement on write, no
  `cas_required` enforcement on data writes AT ALL, then only presence- not
  value-checked once added), so silent-failure bugs (B17's subtree skip,
  B18, B19) were invisible by construction and the pre-fix `kvv2` coverage
  number overstated confidence in exactly the paths that mattered most.
  Mock fidelity is now fixed for 404-with-data, pruning, AND check-and-set
  enforcement (mount- and secret-level, VALUE-checked against
  `CurrentVersion` per `path_data.go:283-288`, not just presence-checked);
  treat harness realism as a prerequisite before trusting future coverage
  deltas in this package.

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
- `oldest_version` from the KV v2 metadata response is not currently parsed
  by `kv2MetadataResp` (`kvv2.go` ~106-118). Adding that one field lets a
  missing version be classified as "pruned at source" (`v < oldest_version`)
  vs. "never existed", which this taxonomy needs.
- B18 (done): its fix already produces a clean `deleted` (genuinely
  soft-deleted, confirmed by the read failing) vs `read_error` (a real read
  failure — network, 5xx, etc.) split in `VersionStates`, as a side effect
  of the `errVersionDataUnavailable` sentinel. That distinction did NOT
  need the full reason taxonomy above and was not built out further — P4
  remains open for the placeholder `_reason` differentiation
  (`missing_in_metadata`/`source_version_unavailable`/`read_error`/
  `destroyed`) on the destination payload itself, which is a separate,
  larger piece of work.

### P5: Coverage Follow-up (Optional)
- Improve bootstrap coverage (`main`, `cmd` CLI wiring)
- Add targeted failure-path tests where defects appear

### P6: Deferred Bugs

- [x] B6 (REFRAMED — was "version-replay pruning data loss"): NOT silent
  data loss from placeholders. Placeholder triggers
  (`copyOneSecret`/`copySecretFull`/`copyIncrementalVersions`, `kvv2.go`
  lines 144-155 / 316-328 / 426-438 — prior line refs 138-149/307-319/
  417-429 were stale) read only `srcMeta.Data.Versions` and never consult
  destination state, so destination-side pruning cannot cause a placeholder
  write. The latest value is never lost (written last, always survives
  destination retention). Losing *old* versions is the destination honoring
  its own configured `max_versions` — expected behavior, not a bug. The
  original CAS claim is dropped entirely: `kv2WriteData` sends no `cas`
  option, so a `cas_required` destination fails loudly on write, it does not
  silently drop data. What actually remains as real gaps:
  - [x] (i) `DestVersionCount` in state is now measured from an actual
    destination metadata read (`measureDestVersionCount`, `kvv2.go`) after
    both `copySecretFull` and `copyIncrementalVersions` write, instead of
    always asserted equal to the source count. A measurement read failure
    falls back to the assumed value and logs at debug — bookkeeping never
    aborts the migration. Covered by
    `TestCopySecretFull_DestVersionCountMeasuredFromDestination` and
    `TestCopyIncrementalVersions_DestVersionCountMeasuredFromDestination`.
  - [x] (ii) `warnDestTruncated` (`kvv2.go`) now emits a `Logger.Warn`
    naming the secret key, source version count, and measured destination
    version count whenever destination `max_versions` truncated history.
    Warning only — never fails, never retries (latest value always
    survives, written last). Covered by the same two tests as (i).
  - [x] (iii) REJECTED — reordering `kv2WriteMetadataSettings` to run
    BEFORE the version-write loop in `copySecretFull` (`kvv2.go:381`, loop
    321-379) and `copyIncrementalVersions` (`kvv2.go:491`, loop 431-489).
    Do NOT retry this. Rationale:
    - The retention premise WAS correct and stays verified:
      `vault-plugin-secrets-kv@v0.26.2/path_data.go:750-756` uses
      `max(k.MaxVersions, configMaxVersions)`, so per-secret `max_versions`
      can only RAISE effective retention, never lower it — applying it
      before the loop would have been a legitimate easy win in isolation.
    - REJECTED because the metadata payload is not just `max_versions`.
      `kv2WriteMetadataSettings` (`kvv2.go:764-768`) sends `cas_required`
      UNCONDITIONALLY from source metadata, and `kv2WriteData`
      (`kvv2.go:720-730`) never sends `options.cas`. Moving the metadata
      write before the loop would set `cas_required=true` on the
      destination up front whenever the SOURCE secret has it set, then
      EVERY subsequent version write in that same loop 400s
      ("check-and-set parameter required for this call",
      `path_data.go:278-288`) — total migration failure on exactly the
      secrets operators guarded hardest. Task 1 in this session proved this
      failure mode is real (see B19): it currently happens on the
      DESTINATION side (operator tunes destination `cas_required`), and
      reordering would additionally trigger it from the SOURCE side any
      time source metadata carries `cas_required=true`, which is strictly
      worse.
      - **UPDATE (B19 fixed in a later session):** objection #1 above (a
        reorder would 400 every subsequent write in the same loop) is now
        RETIRED as a blocker on its own — `kv2WriteData` reactively retries
        with `options.cas` on a `400 check-and-set parameter required`
        response, so a `cas_required` destination no longer hard-fails a
        write. This does NOT reopen B6-iii: objections #2
        (`delete_version_after` stamped at write time, corrupting replayed
        history with a live deadline) and #3 (policy — this tool must not
        silently raise an operator's destination retention config) are
        unaffected by B19 and STAND on their own. B6-iii REMAINS REJECTED.
    - Second reason: the payload also carries `delete_version_after`,
      computed as the minimum non-zero of mount and per-secret and applied
      AT WRITE TIME (`delete_version_after.go:16-28`,
      `path_data.go:398-406`). Reordering stamps every replayed version
      with `write_time + source_dva`, so a source `dva=24h` on years-old
      history would make the entire migrated corpus self-delete a day
      after cutover.
    - Third reason (policy): silently RAISING an operator's destination
      retention config exceeds this tool's mandate. The destination
      honoring its own tuned `max_versions` is correct-by-policy, and B6
      (i)/(ii) this session now measure and warn about it instead of lying
      in state. The warning IS the right answer, not a reorder.
    - Verified NON-issue, recorded so it isn't re-litigated: source/
      destination version-number ALIGNMENT survives a reorder fine —
      `AddVersion` increments monotonically and prunes by numeric window
      without renumbering, and delete/destroy fire immediately after each
      write when the version is newest-and-present. Alignment was never the
      blocker; `cas_required`/`delete_version_after` ordering was.
    - Salvage path for a future session, if this is ever revisited: split
      the payload rather than reorder it whole — send ONLY
      `{"max_versions": ...}` before the loop (the sole field that is
      monotonic-upward by plugin design and harmless to already-written
      versions), send `cas_required`/`delete_version_after`/
      `custom_metadata` after the loop as today, and gate the early
      `max_versions`-only write behind an explicit opt-in flag with loud
      logging so it is never silent-by-default.
  - (iv) The destructive recopy loop (destination retention < readable
    source versions -> pruned-version read -> 404 -> treated as mismatch ->
    delete+recopy -> re-pruned identically -> repeats every run) was the
    most serious item under this bug and is FIXED this session — see
    Completed Snapshot.
- [x] B18 (fixed this session): 404-with-data bypassed the placeholder
  path. Vault returns HTTP 404 WITH a `data` body
  (`{"data":{"data":null,"metadata":{...}}}`, no `errors`) for a
  soft-deleted version read, which the Vault SDK reported as SUCCESS
  (`kv2ReadVersion` returned `(nil, nil)`), so `copyOneSecret`/
  `copySecretFull`/`copyIncrementalVersions` skipped the `opts.Placeholder`
  branch and wrote `{"data": null}` to the destination. Fixed by returning
  a new sentinel `errVersionDataUnavailable` from `kv2ReadVersion` when
  `out.Data.Data == nil`, so every existing `if rerr != nil` placeholder
  branch fires naturally across all three copy paths.
  `verifyVersionHashes`/`verifyDestinationMatches` skip that sentinel
  instead of failing the comparison, so it cannot trigger a delete+recopy
  loop. A second, closely related bug found and fixed in the same pass: the
  destination-mirroring `else if vm.DeletionTime != ""` branch (all three
  copy paths) unconditionally soft-deleted the destination version even
  when the source read had just succeeded with real, live data (a
  future-dated `delete_version_after` sets `DeletionTime` while the version
  stays fully readable) — this actively destroyed correctly-copied live
  data. Both fixes now key exclusively off whether the read itself
  produced data, never off `DeletionTime != ""` alone, per the original
  critical warning. Covered by
  `TestKV2ReadVersion_SoftDeletedVersionReturnsErrVersionDataUnavailable`,
  `TestCopyPaths_SoftDeletedVersionGetsPlaceholder` (table-driven across
  `copyOneSecret`/`copySecretFull`/`copyIncrementalVersions`),
  `TestKV2ReadVersion_FutureDeletionTimeStillReadsRealData` +
  `TestCopySecretFull_FutureDeletionTimeCopiesRealData` (the critical-warning
  regression test), and `TestCopyOneSecretWithState_SoftDeletedVersionIsIdempotent`
  (re-run performs zero destination deletes). All new tests confirmed to
  FAIL against the pre-fix `kv2ReadVersion` logic via a stash-based
  verification before landing.
- [x] B19 (fixed this session): migrating INTO a `cas_required` destination
  used to fail completely, loudly, on the very first version write.
  `kv2WriteData` (`kvv2.go:801-811`) sent only `{"data": ...}` — never
  `options.cas`, on any write, for any secret, ever. Real Vault's KV v2
  plugin requires check-and-set on every data write when EITHER the
  destination mount's `cas_required` tunable OR the destination secret's
  own per-secret `cas_required` is true
  (`vault-plugin-secrets-kv@v0.26.2/path_data.go:278-288`); a write with no
  `options.cas` got a 400 "check-and-set parameter required for this call".
  Reachable purely from the DESTINATION side (an operator independently
  tuning their own destination) and, as a second finding this session, also
  SELF-INFLICTED from the SOURCE side: `kv2WriteMetadataSettings`
  (`kvv2.go:839-856`) copies `cas_required` from source metadata
  UNCONDITIONALLY, after the version-write loop, so a source secret with
  `cas_required=true` stamps that flag onto the destination as run 1's last
  step even though run 1 itself succeeded (destination had no
  `cas_required` yet) — run 2's incremental write then 400s with zero
  destination-side operator action. Locked by
  `TestCopySecretFull_SourceCASRequiredSurvivesSecondRun`.

  **Fix — reactive CAS retry, no cached counter:** `kv2WriteData` sends the
  exact same request as before on the first attempt (no `options` key). If,
  and only if, that write comes back 400 "check-and-set parameter required"
  (matched via a narrow, well-documented substring exception to B17's
  no-substring rule — mismatch and missing-cas are both plain 400 with no
  structural distinction, see `isCASRequiredError` in `helpers.go`), it
  reads the destination's current version via `kv2ReadMetadata(...)
  .Data.CurrentVersion` (already-parsed, previously dead field) and retries
  EXACTLY ONCE with `options.cas` set to that value — `CurrentVersion`, not
  `CurrentVersion+1` (the plugin itself advances on success,
  `path_data.go:267-291`) and not a recomputed `getMaxVersion` (wrong when
  every version is destroyed or pruned, since destroy/pruning never touch
  `CurrentVersion`). A missing destination secret reads as `cas=0`. A
  mismatch on the retry (a genuine concurrent writer) PROPAGATES — no loop,
  no second retry. A failed seed metadata read (real error, not
  not-found) never fabricates a cas value; the ORIGINAL CAS-required error
  propagates with the read failure attached as context. On a destination
  that never requires CAS — the overwhelming common case — this is
  byte-identical to the pre-B19 wire format: one write, zero `options` key,
  zero extra requests, locked by
  `TestKV2WriteData_NoCASSentWhenNotRequired` and
  `TestKV2WriteData_NoExtraRequestsWhenNotRequired`.

  Covered by (all confirmed to FAIL pre-fix via stash-based verification):
  `TestKV2WriteData_CASRequiredRetriesWithCurrentVersion`,
  `TestKV2WriteData_CASRequiredAllVersionsDestroyed`,
  `TestKV2WriteData_CASMismatchIsNotRetried`,
  `TestKV2WriteData_CASSeedMetadataReadFailure`,
  `TestCopySecretFull_CASRequiredDestination` (renamed from
  `..._FailsLoudly`, inverted to lock success),
  `TestCopyIncrementalVersions_CASRequiredDestination`,
  `TestCopyOneSecret_CASRequiredDestination`,
  `TestCopyOneSecretWithState_CASRequiredAfterDeleteRecopy` (the
  stale-cas-after-delete trap: delete+recopy on a `cas_required` mount must
  seed `cas=0` from a fresh post-delete read, not a cached pre-delete
  value), and `TestCopySecretFull_SourceCASRequiredSurvivesSecondRun`
  (the source-side self-inflicted finding above). Common-path regression
  locks: `TestKV2WriteData_NoCASSentWhenNotRequired`,
  `TestKV2WriteData_NoExtraRequestsWhenNotRequired`,
  `TestKV2WriteData_400NonCASNotRetried` (B17 regression lock — an
  unrelated 400 never triggers the retry),
  `TestKV2WriteData_Non400ErrorNotRetried` (403/500 propagate immediately,
  no retry, no metadata read).

  Prerequisite hardening: the mock's CAS check (`kvv2_mock_test.go`) used
  to validate only that `options.cas` was PRESENT, which would have let an
  implementation sending a wrong or hardcoded cas value (e.g. always `0`)
  pass every mock test while failing against real Vault. It now validates
  the VALUE against the fake secret's `CurrentVersion`, mirroring
  `path_data.go:283-288` exactly (mismatch vs. missing-cas, both 400 with
  distinct messages). Proven to bite with a throwaway always-`cas=0`
  probe against a `CurrentVersion=3` secret (failed as expected, then
  discarded, not committed).

  E2E-verified against a real Vault 1.18.5 cluster (Docker/podman,
  `test/e2e/e2e_test.go`): `TestE2E_CASRequiredMountDestination` (the ONLY
  real-Vault proof of the mount-level OR at `path_data.go:286`),
  `TestE2E_CASRequiredPerSecretDestination`, and
  `TestE2E_SourceCASRequiredIncremental`. All three passed on the first
  E2E run against the fix.

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
- B8 (client timeout hardcoded at 3s) fixed: `-clientTimeout` flag added,
  default 60s, wired through `client.SetClientTimeout`
- B9 (mount-existence preflight) — REDESIGNED, not implemented as originally
  scoped. A `sys/mounts` preflight was rejected: it needs a read permission
  the README's least-privilege token policy doesn't grant, it degraded to a
  mere warning on 403 and every other error (making it toothless exactly
  when it mattered), and its `options.version != "2"` hard-fail would have
  broken working mounts that are actually fine — in-place
  `vault kv enable-versioning` upgrades, Enterprise replicas, and older
  Vault versions can all report nil/absent `options`. Replaced with a
  0-keys guard in `Run`: if `walkAllKeys` finds zero secrets under the
  configured source mount/base path, the migration now errors with
  "refusing to report success" instead of exiting 0. Needs no new
  permissions and catches a strict superset of B9's target cases (missing
  mount, KV v1 mount, typo'd mount, typo'd base path) since all of them
  resolve to zero keys.
- B10 (dead `srcAddr`/`dstAddr` prompts in `client.BuildClients`) — doc-only
  fix, no code change. Confirmed `cmd.validateConfig` fatals before
  `BuildClients` runs if either address is empty, so those two prompt
  branches are unreachable via the `cmd` entrypoint. Kept in code as a
  library-safe fallback for callers that invoke `client.BuildClients`
  directly without going through `validateConfig` first. README's Prompt
  Order section corrected to state both addresses are flag-required and not
  actually prompted from the CLI, with the remaining prompts renumbered.
- B11 (non-atomic state write) fixed: `state.Save` now writes to a temp file
  in the same directory as the target and `os.Rename`s into place. Atomic
  against a killed process; NOT atomic against power loss (no `fsync`
  before rename).
- B13 (`log.Fatal*` in `client/`) fixed: all `log.Fatal*` calls in
  `client/client.go`'s `getClient` replaced with returned errors; `cmd`
  still fatals at the entrypoint, as intended. Unlocked
  `client/client_test.go`, the first test file ever added for that package
  (0% -> ~52.6% coverage), with regression locks for B5
  (namespace-before-lookup), B8 (timeout honored), and B14 (`SetMaxRetries`
  honored).
- B14 (unused `-maxRetries` flag) fixed: wired into
  `client.SetMaxRetries` + `retryablehttp.RateLimitLinearJitterBackoff`. It
  was previously validated (`>= 0`) but never applied, leaving `RetryMax` at
  0 — FEWER retries than the Vault SDK's own default of 2, with no
  rate-limit-aware backoff on `429`/`503`. Retries live entirely at the
  idempotent HTTP transport layer; deliberately no app-level per-secret
  retry loop.
- B16 (`trimSlashes` single-slash strip) fixed: now `strings.Trim` (was
  `TrimPrefix`/`TrimSuffix`, which only stripped one leading/trailing slash
  each), so `//app/` now normalizes to `app` instead of `/app`.
- B17 (`isNotFound` substring matching) fixed: renamed `isMetadataNotFound`,
  purely structural now (`errors.Is` on the `errMetadataNotFound` sentinel,
  `errors.As` on `*api.ResponseError` checking `StatusCode == 404`). All
  substring matching deleted. The substrings were causing SILENT DATA LOSS:
  a KV v1 mount returns HTTP 400 "unsupported path", which used to match as
  not-found. Bigger win under the same bug number: the `isNotFound` swallow
  branch in `walkAllKeys`/`rec` (`kvv2.go`) was deleted outright — it
  silently skipped an entire subtree on ANY list error and still let the
  overall migration exit 0. `kv2List` already returns `(nil, nil)` for a
  genuinely absent prefix, so that branch could never fire on a real
  not-found; it only ever hid real errors (403, 400, 5xx, timeouts).
- Destructive recopy loop fixed (filed under B6, see REFRAMED entry above
  for the rest of B6's scope): `verifyDestinationMatches` now skips source
  versions absent from destination metadata instead of treating them as a
  mismatch. Previously, when destination retention was lower than the
  number of readable source versions, comparison hit a pruned-away version,
  got a bare 404, treated it as an error, triggered `kv2DeleteSecret` +
  full recopy, which re-pruned to the identical state — repeating on EVERY
  run. Non-idempotent and destructive, worst when local state was
  absent/stale. Genuine mismatches are still detected and recopied.
- Mock fidelity improved: `newFakeVault` now serves real Vault's
  404-with-data shape for destroyed/soft-deleted version reads (rather than
  a bare error), enforces per-secret and mount-level `max_versions`
  sliding-window pruning on write, and enforces per-secret and mount-level
  `cas_required` on data writes (rejecting a write with no `options.cas` as
  400 "check-and-set parameter required for this call", AND — as of this
  session's B19 fix, previously presence-only — rejecting a wrong
  `options.cas` VALUE as 400 "check-and-set parameter did not match the
  current version", matching `path_data.go:283-288`'s unconditional value
  check). The fake used to be more forgiving than real Vault in all three
  ways, which made pruning-, soft-delete-, and CAS-related bugs invisible
  by construction; the CAS gap specifically hid B19 until closed this
  session, and the presence-only (not value-checked) CAS gap would have
  hidden a wrong-cas-value implementation of B19's own fix — proven via a
  throwaway always-`cas=0` probe that failed once the mock was hardened to
  check the value, then discarded.
- B18 (404-with-data null write on soft-deleted versions) fixed: see Active
  Items entry above for full detail; one-line summary — `kv2ReadVersion`
  now returns `errVersionDataUnavailable` instead of a silent nil payload,
  so soft-deleted versions get the configured placeholder everywhere
  instead of a null write, and a future-dated `delete_version_after` no
  longer gets misidentified as already-deleted on either the read-skip or
  destination-mirroring side.
- B19 (migrating into a `cas_required` destination) fixed: see Active Items
  entry above for full detail; one-line summary — `kv2WriteData` now
  reactively retries exactly once with `options.cas` set to the
  destination's `CurrentVersion` after a "check-and-set parameter
  required" 400, covering both the destination-side trigger (operator
  tunes mount or secret `cas_required`) and a newly-found source-side
  self-inflicted trigger (`kv2WriteMetadataSettings` copies source
  `cas_required` onto the destination unconditionally); byte-identical to
  the pre-fix wire format on any destination that never requires CAS.
  E2E-verified against real Vault 1.18.5.
</content>
