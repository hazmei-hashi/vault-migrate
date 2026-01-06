package kvv2

import (
	"context"
	"fmt"

	"github.com/hashicorp/vault/api"
)

func Copy(srcClient, dstClient *api.Client) error {
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

	var m Migrator
	m.Src = srcCluster
	m.Dst = dstCluster
	err := m.Run(context.Background(), Options{})
	if err != nil {
		fmt.Printf("%v", err)
	}
	return nil
}

type KVV2Cluster struct {
	Client    *api.Client
	MountPath string // e.g. "kv" (no leading/trailing slash)
	BasePath  string // e.g. "apps/team-a" or "" for root of mount
}

type Migrator struct {
	Src KVV2Cluster
	Dst KVV2Cluster
}

type Options struct {
	ContinueOnError bool
	Placeholder     map[string]any
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

	for i, srcKey := range keys {
		dstKey := m.dstKeyFor(srcKey)

		if err := m.copyOneSecret(ctx, srcKey, dstKey, opts); err != nil {
			if opts.ContinueOnError {
				fmt.Printf("[WARN] (%d/%d) %q -> %q failed: %v\n", i+1, len(keys), srcKey, dstKey, err)
				continue
			}
			return fmt.Errorf("copy %q -> %q: %w", srcKey, dstKey, err)
		}
	}

	return nil
}
