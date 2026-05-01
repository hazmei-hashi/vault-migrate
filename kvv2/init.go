package kvv2

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"vault-migrate/config"
	"vault-migrate/state"

	"github.com/hashicorp/vault/api"
)

type KVV2Cluster struct {
	Client    *api.Client
	MountPath string
	BasePath  string
}

type Migrator struct {
	Src       KVV2Cluster
	Dst       KVV2Cluster
	LogLevel  string
	Config    config.VaultClientConfig
	State     *state.MigrationState
	StateFile string
	Logger    *slog.Logger
}

type Options struct {
	ContinueOnError bool
	Placeholder     map[string]any
	DryRun          bool
}

func Init(srcClient, dstClient *api.Client, cfg config.VaultClientConfig) error {
	scanner := bufio.NewScanner(os.Stdin)

	var srcCluster KVV2Cluster
	var dstCluster KVV2Cluster

	fmt.Print("Source KV-V2 mount (e.g., 'secret'): ")
	if scanner.Scan() {
		srcCluster.MountPath = strings.TrimSpace(scanner.Text())
	} else if scanner.Err() != nil {
		return fmt.Errorf("failed to read source mount: %w", scanner.Err())
	}
	srcCluster.MountPath = trimSlashes(srcCluster.MountPath)
	fmt.Printf("  Normalized mount: %s\n", srcCluster.MountPath)

	fmt.Print("Source KV-V2 base path (e.g., 'myapp/' or leave empty for root): ")
	if scanner.Scan() {
		srcCluster.BasePath = strings.TrimSpace(scanner.Text())
	} else if scanner.Err() != nil {
		return fmt.Errorf("failed to read source base path: %w", scanner.Err())
	}
	srcCluster.BasePath = trimSlashes(srcCluster.BasePath)
	if srcCluster.BasePath != "" {
		fmt.Printf("  Normalized path: %s\n", srcCluster.BasePath)
	} else {
		fmt.Printf("  Using root path\n")
	}

	srcCluster.Client = srcClient

	fmt.Print("Destination KV-V2 mount (e.g., 'secret'): ")
	if scanner.Scan() {
		dstCluster.MountPath = strings.TrimSpace(scanner.Text())
	} else if scanner.Err() != nil {
		return fmt.Errorf("failed to read destination mount: %w", scanner.Err())
	}
	dstCluster.MountPath = trimSlashes(dstCluster.MountPath)
	fmt.Printf("  Normalized mount: %s\n", dstCluster.MountPath)

	fmt.Print("Destination KV-V2 base path (e.g., 'myapp-migrated/' or leave empty for root): ")
	if scanner.Scan() {
		dstCluster.BasePath = strings.TrimSpace(scanner.Text())
	} else if scanner.Err() != nil {
		return fmt.Errorf("failed to read destination base path: %w", scanner.Err())
	}
	dstCluster.BasePath = trimSlashes(dstCluster.BasePath)
	if dstCluster.BasePath != "" {
		fmt.Printf("  Normalized path: %s\n", dstCluster.BasePath)
	} else {
		fmt.Printf("  Using root path\n")
	}

	dstCluster.Client = dstClient

	logger := config.SetupLogger(cfg.LogLevel)

	var m Migrator
	m.Src = srcCluster
	m.Dst = dstCluster
	m.LogLevel = cfg.LogLevel
	m.Config = cfg
	m.StateFile = cfg.StateFile
	m.Logger = logger

	if !cfg.NoState {
		logger.Debug("Loading state file", "path", cfg.StateFile)
		existingState, err := state.Load(cfg.StateFile)
		if err != nil {
			return fmt.Errorf("load state file: %w", err)
		}

		if existingState != nil {
			srcInfo := state.ClusterInfo{
				Address:   cfg.SrcAddr,
				Namespace: cfg.SrcNamespace,
				Mount:     srcCluster.MountPath,
				BasePath:  srcCluster.BasePath,
			}
			dstInfo := state.ClusterInfo{
				Address:   cfg.DstAddr,
				Namespace: cfg.DstNamespace,
				Mount:     dstCluster.MountPath,
				BasePath:  dstCluster.BasePath,
			}

			if err := existingState.Validate(srcInfo, dstInfo); err != nil {
				return fmt.Errorf("state validation failed: %w\n\nUse -stateFile to specify different state file or -noState to ignore", err)
			}

			m.State = existingState
			logger.Info("Loaded existing state file", "secrets", len(existingState.Secrets))
		} else {
			m.State = state.NewMigrationState(
				state.ClusterInfo{
					Address:   cfg.SrcAddr,
					Namespace: cfg.SrcNamespace,
					Mount:     srcCluster.MountPath,
					BasePath:  srcCluster.BasePath,
				},
				state.ClusterInfo{
					Address:   cfg.DstAddr,
					Namespace: cfg.DstNamespace,
					Mount:     dstCluster.MountPath,
					BasePath:  dstCluster.BasePath,
				},
			)
			logger.Info("Created new state file")
		}
	}

	logger.Debug("Initializing KV-V2 copy")

	if cfg.DryRun {
		logger.Info("DRY RUN MODE - No changes will be made to destination")
	}

	err := m.Run(context.Background(), Options{
		ContinueOnError: cfg.ContinueOnError,
		DryRun:          cfg.DryRun,
	})
	if err != nil {
		logger.Error("migration failed", "err", err)
		return err
	}
	return nil
}

// Initializes the KVV2 migrator
func (m *Migrator) Run(ctx context.Context, opts Options) error {
	if opts.Placeholder == nil {
		opts.Placeholder = map[string]any{
			"_vault_migrate": "placeholder",
			"_reason":        "source_version_unavailable",
		}
	}

	keys, err := m.walkAllKeys(ctx, m.Src, trimSlashes(m.Src.BasePath))
	if err != nil {
		return err
	}

	m.Logger.Info("Starting migration", "total_secrets", len(keys), "dry_run", opts.DryRun)

	if opts.DryRun {
		m.Logger.Info("DRY RUN: Would migrate the following secrets:")
		for i, srcKey := range keys {
			dstKey := m.dstKeyFor(srcKey)
			m.Logger.Info("Would copy", "index", i+1, "src", srcKey, "dst", dstKey)
		}
		m.Logger.Info("DRY RUN complete", "total_secrets", len(keys))
		return nil
	}

	completed := 0
	failed := 0
	skipped := 0

	var progressBar *ProgressBar
	showProgressBar := m.Logger.Enabled(ctx, slog.LevelInfo) &&
		!m.Logger.Enabled(ctx, slog.LevelDebug) &&
		len(keys) > 0
	if showProgressBar {
		progressBar = NewProgressBar(len(keys))
		if progressBar.IsTTY() {
			fmt.Print(progressBar.Render())
		}
	}

	for i, srcKey := range keys {
		dstKey := m.dstKeyFor(srcKey)

		var copyErr error
		if m.State != nil && !m.Config.NoState {
			copyErr = m.copyOneSecretWithState(ctx, srcKey, dstKey, opts)
		} else {
			copyErr = m.copyOneSecret(ctx, srcKey, dstKey, opts)
		}

		if copyErr != nil {
			failed++
			if opts.ContinueOnError {
				m.Logger.Warn("Copy failed", "progress", fmt.Sprintf("%d/%d", i+1, len(keys)), "src", srcKey, "dst", dstKey, "err", copyErr)
				continue
			}
			return fmt.Errorf("copy %q -> %q: %w", srcKey, dstKey, copyErr)
		}

		if m.State != nil {
			secret := m.State.GetSecret(srcKey)
			if secret != nil && secret.Status == "skipped" {
				skipped++
			} else {
				completed++
			}
		} else {
			completed++
		}

		if progressBar != nil {
			progressBar.Update(completed, failed, skipped)
			if progressBar.IsTTY() {
				if progressBar.ShouldRender() {
					fmt.Print(progressBar.Render())
				}
			} else {
				// Non-TTY fallback: periodic line logging
				if (i+1)%10 == 0 || i+1 == len(keys) {
					m.Logger.Info("Progress", "completed", completed, "failed", failed, "skipped", skipped, "total", len(keys))
				}
			}
		} else {
			if (i+1)%10 == 0 {
				m.Logger.Debug("Progress", "completed", completed, "failed", failed, "skipped", skipped, "total", len(keys))
			}
		}
	}

	if progressBar != nil {
		if progressBar.IsTTY() {
			fmt.Print(progressBar.RenderFinal())
		}
		progressBar.Finish()
	}

	m.Logger.Info("Migration completed", "total", len(keys), "completed", completed, "failed", failed, "skipped", skipped)

	if m.State != nil && !m.Config.NoState {
		if err := m.State.Save(m.StateFile); err != nil {
			m.Logger.Error("failed to save final state", "err", err)
		} else {
			m.Logger.Info("State saved", "file", m.StateFile)
		}
	}

	return nil
}
