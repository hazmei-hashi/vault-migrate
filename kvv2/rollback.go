package kvv2

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"vault-migrate/config"
	"vault-migrate/state"

	"github.com/hashicorp/vault/api"
)

// Rollback deletes destination secrets that were successfully migrated,
// as recorded in the state file. Targets only secrets with status
// "completed" or "recreated". Keys are derived via dstKeyFor using the
// source/destination base paths from the loaded state's ClusterInfo —
// not re-prompted, so mappings match the original migration exactly.
//
// Requires -stateFile; incompatible with -noState.
// State file is left untouched after rollback (Decision 6).
// Source client is not used but BuildClients still requires a reachable
// source cluster + valid src token (Decision 5 — no changes to validateConfig
// or BuildClients signature).
//
// WARNING: metadata-delete PERMANENTLY destroys all versions + metadata
// for each targeted secret. This operation is IRREVERSIBLE.
func Rollback(dstClient *api.Client, cfg config.VaultClientConfig) error {
	logger := config.SetupLogger(cfg.LogLevel)

	// Step 1: load state — hard error if file missing or no secrets.
	loaded, err := state.Load(cfg.StateFile)
	if err != nil {
		return fmt.Errorf("load state file: %w", err)
	}
	if loaded == nil {
		return fmt.Errorf("no state file at %q, nothing to roll back", cfg.StateFile)
	}
	if len(loaded.Secrets) == 0 {
		return fmt.Errorf("state file %q has no secrets recorded, nothing to roll back", cfg.StateFile)
	}

	// Step 2: validate operator-supplied dst address+namespace against state.
	// Rollback derives mount/basePath FROM state by design; the operator
	// controls only DstAddr and DstNamespace. A mismatch means the operator
	// is pointing at a DIFFERENT cluster than the one that was migrated to —
	// which would metadata-delete keys on a cluster this tool never wrote to.
	//
	// Normalize both sides before comparing: trimSlashes strips whitespace
	// and trailing/leading slashes so "-dstAddr https://vault:8200/" and
	// "https://vault:8200" match correctly on the same cluster.
	if trimSlashes(cfg.DstAddr) != trimSlashes(loaded.Destination.Address) ||
		trimSlashes(cfg.DstNamespace) != trimSlashes(loaded.Destination.Namespace) {
		return fmt.Errorf(
			"rollback refused: destination address/namespace in config does not match state file\n"+
				"  State records:  %s (namespace %q)\n"+
				"  Config supplies: %s (namespace %q)\n"+
				"Pass the correct -dstAddr/-dstNamespace or use the right -stateFile.",
			loaded.Destination.Address, loaded.Destination.Namespace,
			cfg.DstAddr, cfg.DstNamespace,
		)
	}

	// Step 3: build a Migrator from loaded state's ClusterInfo so
	// dstKeyFor uses the original base paths, not re-prompted values.
	m := &Migrator{
		Src: KVV2Cluster{
			MountPath: loaded.Source.Mount,
			BasePath:  loaded.Source.BasePath,
		},
		Dst: KVV2Cluster{
			Client:    dstClient,
			MountPath: loaded.Destination.Mount,
			BasePath:  loaded.Destination.BasePath,
		},
		Config:    cfg,
		StateFile: cfg.StateFile,
		Logger:    logger,
	}

	// Step 4: collect targets — only completed/recreated secrets.
	type target struct {
		srcKey    string
		dstKey    string
		recreated bool
	}
	var targets []target
	for srcKey, sec := range loaded.Secrets {
		if sec.Status == "completed" || sec.Status == "recreated" {
			targets = append(targets, target{
				srcKey:    srcKey,
				dstKey:    m.dstKeyFor(srcKey),
				recreated: sec.Status == "recreated",
			})
		}
	}

	if len(targets) == 0 {
		logger.Info("No completed/recreated secrets in state file; nothing to roll back")
		return nil
	}

	// Sort by dstKey for deterministic sample display and delete order.
	sort.Slice(targets, func(i, j int) bool { return targets[i].dstKey < targets[j].dstKey })

	// Count recreated separately — those secrets existed before migration;
	// deleting them removes pre-migration data (more destructive than the
	// migration was — it cannot be undone by re-running the migration).
	recreatedCount := 0
	for _, t := range targets {
		if t.recreated {
			recreatedCount++
		}
	}

	// Step 5: dry-run — preview only, no deletes.
	if cfg.DryRun {
		logger.Info("DRY RUN MODE - No changes will be made to destination")
		logger.Info("Target destination",
			"address", loaded.Destination.Address,
			"namespace", loaded.Destination.Namespace,
			"mount", loaded.Destination.Mount,
			"base_path", loaded.Destination.BasePath,
		)
		for _, t := range targets {
			if t.recreated {
				logger.Info("Would delete (WARNING: pre-migration data)", "src", t.srcKey, "dst", t.dstKey)
			} else {
				logger.Info("Would delete", "src", t.srcKey, "dst", t.dstKey)
			}
		}
		if recreatedCount > 0 {
			logger.Warn("recreated secrets will lose pre-migration data permanently",
				"count", recreatedCount)
		}
		logger.Info("DRY RUN complete", "would_delete", len(targets), "recreated", recreatedCount)
		return nil
	}

	// Step 6: confirmation prompt — show destination cluster, count + sample,
	// and call out recreated secrets before asking.
	dstNS := loaded.Destination.Namespace
	if dstNS == "" {
		dstNS = "(root)"
	}
	dstBase := loaded.Destination.BasePath
	if dstBase == "" {
		dstBase = "(root)"
	}

	var banner bytes.Buffer
	fmt.Fprintf(&banner, "\n⚠  ROLLBACK: permanently deletes ALL versions + metadata (IRREVERSIBLE)\n")
	fmt.Fprintf(&banner, "\nTarget destination:\n")
	fmt.Fprintf(&banner, "  Address:   %s\n", loaded.Destination.Address)
	fmt.Fprintf(&banner, "  Namespace: %s\n", dstNS)
	fmt.Fprintf(&banner, "  Mount:     %s\n", loaded.Destination.Mount)
	fmt.Fprintf(&banner, "  Base path: %s\n", dstBase)
	fmt.Fprintf(&banner, "\nWill delete %d destination secret(s).\n", len(targets))
	if recreatedCount > 0 {
		fmt.Fprintf(&banner, "  ⚠  WARNING: %d of these secrets existed BEFORE migration (status 'recreated')\n", recreatedCount)
		fmt.Fprintf(&banner, "     and will be PERMANENTLY DELETED, not restored to their prior state.\n")
	}

	maxSample := 5
	if len(targets) < maxSample {
		maxSample = len(targets)
	}
	fmt.Fprintf(&banner, "\nSample keys (destination paths):\n")
	for i := 0; i < maxSample; i++ {
		tag := ""
		if targets[i].recreated {
			tag = " [pre-migration data]"
		}
		fmt.Fprintf(&banner, "  %s%s\n", targets[i].dstKey, tag)
	}
	if len(targets) > maxSample {
		fmt.Fprintf(&banner, "  ... and %d more\n", len(targets)-maxSample)
	}
	fmt.Fprintf(&banner, "\n")
	fmt.Print(banner.String())

	answer, err := config.Prompt("Proceed with rollback? [y/N]: ")
	if err != nil && answer == "" {
		return fmt.Errorf("read confirmation: %w", err)
	}
	// ponytail: reuse client.go prompt pattern — only "y"/"yes" (case-insensitive) proceeds.
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		fmt.Println("Rollback aborted.")
		return nil
	}

	// Step 7: delete loop.
	ctx := context.Background()
	deleted := 0
	failed := 0
	notFound := 0 // routing 404s (wrong mount/namespace/non-KV path); NOT confirmed-already-deleted

	for _, t := range targets {
		delErr := m.kv2DeleteSecret(ctx, m.Dst, t.dstKey)
		if delErr != nil {
			// kv2DeleteSecret DELETE on a Vault metadata path:
			// - absent secret → real Vault returns 204 (idempotent), counted deleted++
			// - routing 404 (wrong mount, wrong namespace, non-KV mount) → error here
			//   These must NOT silently succeed (B9/B17 pattern).
			if isMetadataNotFound(delErr) {
				logger.Warn("not found (wrong mount/namespace?)", "dst", t.dstKey, "err", delErr)
				notFound++
				if cfg.ContinueOnError {
					continue
				}
				return fmt.Errorf("delete %q: not found — verify mount/namespace match state; if expected, use -continueOnError: %w", t.dstKey, delErr)
			}
			failed++
			if cfg.ContinueOnError {
				logger.Warn("Delete failed", "dst", t.dstKey, "err", delErr)
				continue
			}
			return fmt.Errorf("delete %q: %w", t.dstKey, delErr)
		}
		logger.Debug("Deleted", "dst", t.dstKey)
		deleted++
	}

	// Step 8: report.
	logger.Info("Migration rollback complete",
		"deleted", deleted,
		"failed", failed,
		"not_found", notFound,
		"total_targeted", len(targets),
	)

	// B9/B17 guard: if nothing was deleted but we got routing-404s, refuse
	// to report success — the destination mount/namespace is likely mismatched.
	if deleted == 0 && notFound > 0 {
		return fmt.Errorf(
			"rollback deleted 0 secrets but %d were not found; "+
				"destination mount/namespace likely mismatched — refusing to report success",
			notFound,
		)
	}

	if failed > 0 {
		return fmt.Errorf("rollback finished with %d failure(s); check logs above", failed)
	}

	fmt.Printf("\nState file left untouched: %s\n", cfg.StateFile)
	return nil
}
