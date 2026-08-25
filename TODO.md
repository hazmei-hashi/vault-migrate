# TODO

Active backlog only. Obsolete historical notes removed.

## Current Baseline (2026-08-25)

- 334 tests passing across 7 packages (E2E gated behind `E2E_TESTS=1`, 6
  scenarios verified against a real Vault 1.18.5 cluster, not counted in the
  334)
- Coverage: `client` 52.6%, `cmd` 41.4%, `config` 100.0%, `kvv2` 85.6%, `state` 85.5%; **total 80.6%**
- Phases 1-4 complete (unit, integration, mock harness, E2E)
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

### P3: Concurrency — REJECTED (won't implement)

Worker-pool parallel copy considered and rejected. Vault KV v2 migration is
I/O-bound on Vault (server-side request handling + rate limits), not on this
CLI's single-threaded loop. Parallelizing across secrets hits the same
server-side ceiling with added complexity (state-map mutex, escaped-pointer
races in `copyIncrementalVersions`' shared `VersionHashes` map,
per-secret `Save` serialization, progress-counter races). Worker count would
only ever be a coarse throttle the existing transport-layer
`retryablehttp` backoff (B14) already handles per-request. Default-1 serial
behavior is correct as-is. If a real workload ever proves CLI-bound (not
Vault-bound), revisit — but measure first.

### P4: Known gap — `copyOneSecret` stateless read_error self-heal

`copyOneSecret` (the stateless `-noState` path) has no self-heal for a
transient `read_error` placeholder baked into the destination on a previous
run. Re-run in stateful mode or delete and re-migrate the affected secret.
Out of scope — not a regression, flagged so it isn't filed as a new bug.

### P7: Known gap — `custom_metadata` cleared at source is not cleared at destination

`kv2WriteMetadataSettings` only sends `custom_metadata` when the source map is
non-empty (`len(meta.Data.CustomMetadata) > 0`), so a source secret whose
`custom_metadata` was REMOVED after an earlier migration leaves the stale
entries in place on the destination. Found while diagnosing B20; deliberately
NOT changed there — sending `custom_metadata: {}` unconditionally would clear
destination-side metadata the operator may have set themselves, which is the
same "silently rewriting operator config" objection that killed B6-iii. If
faithful mirroring is wanted, gate it behind an explicit flag. Documented as a
limitation in README.

### P6: Deferred Decisions (do not re-litigate)

#### B6-iii REJECTED — reorder `kv2WriteMetadataSettings` before version-write loop

Do NOT retry this. Rationale:

- **`cas_required` ordering:** Moving the metadata write before the loop
  would set `cas_required=true` on the destination up front whenever the
  SOURCE secret has it set, then EVERY subsequent version write in that same
  loop returns HTTP 400 ("check-and-set parameter required"). Even with B19's reactive
  CAS retry, this remains undesirable.
- **`delete_version_after` corruption:** The payload carries
  `delete_version_after`, computed as the minimum non-zero of mount and
  per-secret and applied AT WRITE TIME (`path_data.go:398-406`). Reordering
  stamps every replayed version with `write_time + source_dva`, so a source
  `dva=24h` on years-old history makes the entire migrated corpus
  self-delete a day after cutover.
- **Policy:** Silently RAISING an operator's destination retention config
  exceeds this tool's mandate. B6 (i)/(ii) measure and warn instead — the
  warning is the right answer.

Salvage path if ever revisited: split the payload — send ONLY
`{"max_versions": ...}` before the loop (monotonic-upward by plugin design,
harmless to already-written versions), send `cas_required`/
`delete_version_after`/`custom_metadata` after the loop as today, and gate
the early `max_versions`-only write behind an explicit opt-in flag with loud
logging.

#### B9 — mount-existence preflight REDESIGNED

`sys/mounts` preflight rejected: needs a permission the README's
least-privilege token policy doesn't grant, degrades to a warning on 403,
and `options.version != "2"` hard-fail breaks working mounts. Replaced with
a 0-keys guard in `Run`: zero secrets under the configured source mount/base
path → error "refusing to report success". Catches a strict superset of B9's
target cases with no new permissions.

#### B10 — dead `srcAddr`/`dstAddr` prompts: doc-only fix, no code change

`cmd.validateConfig` fatals before `BuildClients` runs if either address is
empty, so those two prompt branches are unreachable via the `cmd` entrypoint.
Kept in code as a library-safe fallback for callers that invoke
`client.BuildClients` directly without going through `validateConfig` first.

