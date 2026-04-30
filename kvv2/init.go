package kvv2

import (
	"context"
	"fmt"
	"log/slog"
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
}

func Init(srcClient, dstClient *api.Client, cfg config.VaultClientConfig) error {
	var srcCluster KVV2Cluster
	var dstCluster KVV2Cluster

	fmt.Print("Source KV-V2 mount: ")
	fmt.Scan(&srcCluster.MountPath)

	fmt.Print("Source KV-V2 base path: ")
	fmt.Scan(&srcCluster.BasePath)

	srcCluster.Client = srcClient

	fmt.Print("Destination KV-V2 mount: ")
	fmt.Scan(&dstCluster.MountPath)

	fmt.Print("Destination KV-V2 base path: ")
	fmt.Scan(&dstCluster.BasePath)

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
	err := m.Run(context.Background(), Options{
		ContinueOnError: cfg.ContinueOnError,
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

	m.Logger.Info("Starting migration", "total_secrets", len(keys))

	completed := 0
	failed := 0
	skipped := 0

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

		if (i+1)%10 == 0 || i+1 == len(keys) {
			m.Logger.Info("Progress", "completed", completed, "failed", failed, "skipped", skipped, "total", len(keys))
		}
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
