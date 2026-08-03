package kvv2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"vault-migrate/state"
)

// errMetadataNotFound sentinels Vault's nil/nil "empty metadata" shape (see
// kv2ReadMetadata) so isMetadataNotFound can match it structurally instead of
// relying on message text.
var errMetadataNotFound = errors.New("metadata not found")

// walkAllKeys returns leaf secret keys relative to the mount
func (m *Migrator) walkAllKeys(ctx context.Context, c KVV2Cluster, startPrefix string) ([]string, error) {
	m.Logger.Info("STARTING COPY", "base-path", startPrefix)
	startPrefix = trimSlashes(startPrefix)

	var out []string
	var rec func(prefix string) error

	rec = func(prefix string) error {
		keys, err := m.kv2List(ctx, c, prefix)
		if err != nil {
			// kv2List already returns (nil, nil) for a genuinely absent
			// prefix (see its sec == nil / sec.Data == nil check below), so
			// any error reaching here is real: 403 permission denied, 400
			// "unsupported path" (e.g. a KV v1 mount), 5xx, or a timeout.
			// Previously this branch treated ALL such errors as "missing
			// subtree" and returned nil, which silently dropped every
			// secret beneath prefix from the walk while the overall Run
			// still exited 0 as a successful migration -- B17's highest-
			// value fix.
			return fmt.Errorf("list %q: %w", prefix, err)
		}

		for _, k := range keys {
			if strings.HasSuffix(k, "/") {
				next := joinRel(prefix, strings.TrimSuffix(k, "/"))
				if err := rec(next); err != nil {
					return err
				}
				continue
			}
			out = append(out, joinRel(prefix, k))
		}
		return nil
	}

	if err := rec(startPrefix); err != nil {
		return nil, err
	}

	sort.Strings(out)
	return out, nil
}

// kv2List lists keys under <mount>/metadata/<relPrefix>.
func (m *Migrator) kv2List(ctx context.Context, c KVV2Cluster, relPrefix string) ([]string, error) {
	m.Logger.Debug("LIST", "relative-path", relPrefix)
	relPrefix = trimSlashes(relPrefix)

	mount := trimSlashes(c.MountPath)
	logical := c.Client.Logical()

	var path string
	if relPrefix == "" {
		path = mount + "/metadata"
	} else {
		path = mount + "/metadata/" + relPrefix
	}

	sec, err := logical.ListWithContext(ctx, path)
	if err != nil && relPrefix == "" {
		sec, err = logical.ListWithContext(ctx, mount+"/metadata/")
	}
	if err != nil {
		return nil, err
	}
	if sec == nil || sec.Data == nil {
		return nil, nil
	}

	keysAny, ok := sec.Data["keys"]
	if !ok || keysAny == nil {
		return nil, nil
	}

	raw, ok := keysAny.([]any)
	if !ok {
		if rs, ok := keysAny.([]string); ok {
			return rs, nil
		}
		return nil, fmt.Errorf("unexpected keys type %T", keysAny)
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

type kv2MetadataResp struct {
	Data struct {
		CASRequired        bool              `json:"cas_required"`
		CurrentVersion     int               `json:"current_version"`
		DeleteVersionAfter string            `json:"delete_version_after"`
		MaxVersions        int               `json:"max_versions"`
		CustomMetadata     map[string]string `json:"custom_metadata"`
		Versions           map[string]struct {
			DeletionTime string `json:"deletion_time"`
			Destroyed    bool   `json:"destroyed"`
		} `json:"versions"`
	} `json:"data"`
}

type kv2ReadVersionResp struct {
	Data struct {
		Data     map[string]any `json:"data"`
		Metadata struct {
			Version      int    `json:"version"`
			DeletionTime string `json:"deletion_time"`
			Destroyed    bool   `json:"destroyed"`
		} `json:"metadata"`
	} `json:"data"`
}

func (m *Migrator) copyOneSecret(ctx context.Context, srcKey, dstKey string, opts Options) error {
	meta, err := m.kv2ReadMetadata(ctx, m.Src, srcKey)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}

	maxV := 0
	for vs := range meta.Data.Versions {
		if v, err := strconv.Atoi(vs); err == nil && v > maxV {
			maxV = v
		}
	}

	for v := 1; v <= maxV; v++ {
		vm, ok := meta.Data.Versions[strconv.Itoa(v)]
		if !ok {
			if err := m.kv2WriteData(ctx, m.Dst, dstKey, map[string]any{
				"_vault_migrate":  "placeholder",
				"_source_version": v,
				"_reason":         "missing_in_metadata",
			}); err != nil {
				return fmt.Errorf("write placeholder v=%d: %w", v, err)
			}
			continue
		}

		var payload map[string]any
		if vm.Destroyed {
			payload = opts.Placeholder
		} else {
			p, rerr := m.kv2ReadVersion(ctx, m.Src, srcKey, v)
			if rerr != nil {
				payload = opts.Placeholder
			} else {
				payload = p
			}
		}

		if err := m.kv2WriteData(ctx, m.Dst, dstKey, payload); err != nil {
			return fmt.Errorf("write dst v=%d: %w", v, err)
		}
		payload = nil

		if vm.Destroyed {
			if err := m.kv2DestroyVersions(ctx, m.Dst, dstKey, []int{v}); err != nil {
				return fmt.Errorf("destroy dst v=%d: %w", v, err)
			}
		} else if vm.DeletionTime != "" {
			if err := m.kv2DeleteVersions(ctx, m.Dst, dstKey, []int{v}); err != nil {
				return fmt.Errorf("delete dst v=%d: %w", v, err)
			}
		}
	}

	if err := m.kv2WriteMetadataSettings(ctx, m.Dst, dstKey, meta); err != nil {
		return fmt.Errorf("write metadata settings: %w", err)
	}

	meta.Data.CustomMetadata = nil
	meta.Data.Versions = nil
	meta = nil

	return nil
}

func (m *Migrator) copyOneSecretWithState(ctx context.Context, srcKey, dstKey string, opts Options) error {
	if m.State == nil {
		return m.copyOneSecret(ctx, srcKey, dstKey, opts)
	}

	existingState := m.State.GetSecret(srcKey)

	srcMeta, err := m.kv2ReadMetadata(ctx, m.Src, srcKey)
	if err != nil {
		secretState := &state.Secret{
			Status:     "failed",
			Error:      fmt.Sprintf("read source metadata: %v", err),
			RetryCount: 0,
		}
		if existingState != nil {
			secretState.RetryCount = existingState.RetryCount + 1
		}
		m.State.UpdateSecret(srcKey, secretState)
		if err := m.State.Save(m.StateFile); err != nil {
			m.Logger.Error("failed to save state", "err", err)
		}
		return fmt.Errorf("read source metadata: %w", err)
	}

	srcMaxVersion := getMaxVersion(srcMeta)

	dstMeta, err := m.kv2ReadMetadata(ctx, m.Dst, dstKey)
	destExists := err == nil
	if err != nil && !isMetadataNotFound(err) {
		return fmt.Errorf("read destination metadata: %w", err)
	}

	if destExists {
		dstMaxVersion := getMaxVersion(dstMeta)

		// Case 1: Same version count - verify if already migrated
		if srcMaxVersion == dstMaxVersion {
			// If we have completed state with hashes, use those
			if existingState != nil && existingState.Status == "completed" && len(existingState.VersionHashes) > 0 {
				allMatch, err := m.verifyVersionHashes(ctx, srcKey, dstKey, srcMeta, existingState)
				if err != nil {
					m.Logger.Warn("Failed to verify hashes", "secret", srcKey, "err", err)
				} else if allMatch && !m.Config.ForceRecopy {
					m.Logger.Debug("SKIP (already migrated)", "secret", srcKey, "versions", srcMaxVersion)
					secretState := &state.Secret{
						Status:             "skipped",
						SourceVersionCount: srcMaxVersion,
						DestVersionCount:   dstMaxVersion,
						VersionHashes:      existingState.VersionHashes,
						VersionStates:      existingState.VersionStates,
						MetadataChecksum:   existingState.MetadataChecksum,
					}
					m.State.UpdateSecret(srcKey, secretState)
					if err := m.State.Save(m.StateFile); err != nil {
						m.Logger.Error("failed to save state", "err", err)
					}
					return nil
				}
			} else {
				// No state or no hashes - need to verify by reading actual data
				m.Logger.Debug("Verifying destination secret (no state)", "secret", srcKey)
				allMatch, err := m.verifyDestinationMatches(ctx, srcKey, dstKey, srcMeta)
				if err != nil {
					m.Logger.Warn("Failed to verify destination", "secret", srcKey, "err", err)
					// On verification error, recreate to be safe
					m.Logger.Debug("RECREATE (verification failed)", "secret", srcKey)
					if err := m.kv2DeleteSecret(ctx, m.Dst, dstKey); err != nil {
						return fmt.Errorf("delete destination secret: %w", err)
					}
					return m.copySecretFull(ctx, srcKey, dstKey, srcMeta, opts)
				}

				if allMatch && !m.Config.ForceRecopy {
					m.Logger.Debug("SKIP (destination matches)", "secret", srcKey, "versions", srcMaxVersion)
					// Don't update state here - let it be marked completed on next run
					return nil
				}

				// Hashes don't match - recreate
				m.Logger.Debug("RECREATE (hash mismatch)", "secret", srcKey)
				if err := m.kv2DeleteSecret(ctx, m.Dst, dstKey); err != nil {
					return fmt.Errorf("delete destination secret: %w", err)
				}
				return m.copySecretFull(ctx, srcKey, dstKey, srcMeta, opts)
			}
		}

		// Case 2: Source has more versions - incremental copy
		if srcMaxVersion > dstMaxVersion {
			m.Logger.Debug("INCREMENTAL COPY", "secret", srcKey, "existing_versions", dstMaxVersion, "new_versions", srcMaxVersion-dstMaxVersion)
			return m.copyIncrementalVersions(ctx, srcKey, dstKey, srcMeta, dstMaxVersion+1, srcMaxVersion, opts)
		}

		// Case 3: Destination has more versions than source - error
		if srcMaxVersion < dstMaxVersion {
			m.Logger.Warn("SKIP (destination ahead of source)", "secret", srcKey, "src_versions", srcMaxVersion, "dst_versions", dstMaxVersion)
			secretState := &state.Secret{
				Status:             "failed",
				Error:              "destination has more versions than source - manual review needed",
				SourceVersionCount: srcMaxVersion,
				DestVersionCount:   dstMaxVersion,
			}
			m.State.UpdateSecret(srcKey, secretState)
			if err := m.State.Save(m.StateFile); err != nil {
				m.Logger.Error("failed to save state", "err", err)
			}
			return fmt.Errorf("destination has more versions than source")
		}
	}

	m.Logger.Debug("FULL COPY", "secret", srcKey, "versions", srcMaxVersion)
	return m.copySecretFull(ctx, srcKey, dstKey, srcMeta, opts)
}

func (m *Migrator) copySecretFull(ctx context.Context, srcKey, dstKey string, srcMeta *kv2MetadataResp, opts Options) error {

	maxV := getMaxVersion(srcMeta)
	versionHashes := make(map[string]string)
	versionStates := make(map[string]string)

	for v := 1; v <= maxV; v++ {
		vm, ok := srcMeta.Data.Versions[strconv.Itoa(v)]
		if !ok {
			if err := m.kv2WriteData(ctx, m.Dst, dstKey, map[string]any{
				"_vault_migrate":  "placeholder",
				"_source_version": v,
				"_reason":         "missing_in_metadata",
			}); err != nil {
				return fmt.Errorf("write placeholder v=%d: %w", v, err)
			}
			versionStates[strconv.Itoa(v)] = "missing"
			continue
		}

		var payload map[string]any
		var versionState string

		if vm.Destroyed {
			payload = opts.Placeholder
			versionState = "destroyed"
		} else {
			p, rerr := m.kv2ReadVersion(ctx, m.Src, srcKey, v)
			if rerr != nil {
				payload = opts.Placeholder
				versionState = "read_error"
			} else {
				payload = p
				if vm.DeletionTime != "" {
					versionState = "deleted"
				} else {
					versionState = "active"
				}

				hash, err := state.HashPayload(payload)
				if err != nil {
					m.Logger.Warn("Failed to hash payload", "version", v, "err", err)
				} else {
					versionHashes[strconv.Itoa(v)] = hash
				}
			}
		}

		versionStates[strconv.Itoa(v)] = versionState

		if err := m.kv2WriteData(ctx, m.Dst, dstKey, payload); err != nil {
			return fmt.Errorf("write dst v=%d: %w", v, err)
		}
		payload = nil

		if vm.Destroyed {
			if err := m.kv2DestroyVersions(ctx, m.Dst, dstKey, []int{v}); err != nil {
				return fmt.Errorf("destroy dst v=%d: %w", v, err)
			}
		} else if vm.DeletionTime != "" {
			if err := m.kv2DeleteVersions(ctx, m.Dst, dstKey, []int{v}); err != nil {
				return fmt.Errorf("delete dst v=%d: %w", v, err)
			}
		}
	}

	if err := m.kv2WriteMetadataSettings(ctx, m.Dst, dstKey, srcMeta); err != nil {
		return fmt.Errorf("write metadata settings: %w", err)
	}

	metaChecksum, err := state.HashMetadata(
		srcMeta.Data.CASRequired,
		srcMeta.Data.MaxVersions,
		srcMeta.Data.DeleteVersionAfter,
		srcMeta.Data.CustomMetadata,
	)
	if err != nil {
		m.Logger.Warn("Failed to hash metadata", "err", err)
	}

	if m.State != nil {
		destCount := m.measureDestVersionCount(ctx, dstKey, maxV)
		m.warnDestTruncated(srcKey, maxV, destCount)

		secretState := &state.Secret{
			Status:             "completed",
			SourceVersionCount: maxV,
			DestVersionCount:   destCount,
			VersionHashes:      versionHashes,
			VersionStates:      versionStates,
			MetadataChecksum:   metaChecksum,
			MigratedAt:         time.Now().UTC().Format(time.RFC3339),
		}
		m.State.UpdateSecret(srcKey, secretState)
		if err := m.State.Save(m.StateFile); err != nil {
			m.Logger.Error("failed to save state", "err", err)
		}
	}

	return nil
}

func (m *Migrator) copyIncrementalVersions(ctx context.Context, srcKey, dstKey string, srcMeta *kv2MetadataResp, startVersion, endVersion int, opts Options) error {

	existingState := m.State.GetSecret(srcKey)
	versionHashes := make(map[string]string)
	versionStates := make(map[string]string)

	if existingState != nil {
		versionHashes = existingState.VersionHashes
		versionStates = existingState.VersionStates
		if versionHashes == nil {
			versionHashes = make(map[string]string)
		}
		if versionStates == nil {
			versionStates = make(map[string]string)
		}
	}

	for v := startVersion; v <= endVersion; v++ {
		vm, ok := srcMeta.Data.Versions[strconv.Itoa(v)]
		if !ok {
			if err := m.kv2WriteData(ctx, m.Dst, dstKey, map[string]any{
				"_vault_migrate":  "placeholder",
				"_source_version": v,
				"_reason":         "missing_in_metadata",
			}); err != nil {
				return fmt.Errorf("write placeholder v=%d: %w", v, err)
			}
			versionStates[strconv.Itoa(v)] = "missing"
			continue
		}

		var payload map[string]any
		var versionState string

		if vm.Destroyed {
			payload = opts.Placeholder
			versionState = "destroyed"
		} else {
			p, rerr := m.kv2ReadVersion(ctx, m.Src, srcKey, v)
			if rerr != nil {
				payload = opts.Placeholder
				versionState = "read_error"
			} else {
				payload = p
				if vm.DeletionTime != "" {
					versionState = "deleted"
				} else {
					versionState = "active"
				}

				hash, err := state.HashPayload(payload)
				if err != nil {
					m.Logger.Warn("Failed to hash payload", "version", v, "err", err)
				} else {
					versionHashes[strconv.Itoa(v)] = hash
				}
			}
		}

		versionStates[strconv.Itoa(v)] = versionState

		if err := m.kv2WriteData(ctx, m.Dst, dstKey, payload); err != nil {
			return fmt.Errorf("write dst v=%d: %w", v, err)
		}
		payload = nil

		if vm.Destroyed {
			if err := m.kv2DestroyVersions(ctx, m.Dst, dstKey, []int{v}); err != nil {
				return fmt.Errorf("destroy dst v=%d: %w", v, err)
			}
		} else if vm.DeletionTime != "" {
			if err := m.kv2DeleteVersions(ctx, m.Dst, dstKey, []int{v}); err != nil {
				return fmt.Errorf("delete dst v=%d: %w", v, err)
			}
		}
	}

	if err := m.kv2WriteMetadataSettings(ctx, m.Dst, dstKey, srcMeta); err != nil {
		return fmt.Errorf("write metadata settings: %w", err)
	}

	metaChecksum, err := state.HashMetadata(
		srcMeta.Data.CASRequired,
		srcMeta.Data.MaxVersions,
		srcMeta.Data.DeleteVersionAfter,
		srcMeta.Data.CustomMetadata,
	)
	if err != nil {
		m.Logger.Warn("Failed to hash metadata", "err", err)
	}

	if m.State != nil {
		destCount := m.measureDestVersionCount(ctx, dstKey, endVersion)
		m.warnDestTruncated(srcKey, endVersion, destCount)

		secretState := &state.Secret{
			Status:             "completed",
			SourceVersionCount: endVersion,
			DestVersionCount:   destCount,
			VersionHashes:      versionHashes,
			VersionStates:      versionStates,
			MetadataChecksum:   metaChecksum,
			MigratedAt:         time.Now().UTC().Format(time.RFC3339),
		}
		m.State.UpdateSecret(srcKey, secretState)
		if err := m.State.Save(m.StateFile); err != nil {
			m.Logger.Error("failed to save state", "err", err)
		}
	}

	return nil
}

func getMaxVersion(meta *kv2MetadataResp) int {
	maxV := 0
	for vs := range meta.Data.Versions {
		if v, err := strconv.Atoi(vs); err == nil && v > maxV {
			maxV = v
		}
	}
	return maxV
}

// measureDestVersionCount reads back the destination's actual metadata to
// count versions really persisted there, since destination-side
// max_versions retention can silently prune below the count assumed at
// write time. Bookkeeping only: a read failure must never abort the
// migration, so it logs at debug and falls back to assumed.
func (m *Migrator) measureDestVersionCount(ctx context.Context, dstKey string, assumed int) int {
	dstMeta, err := m.kv2ReadMetadata(ctx, m.Dst, dstKey)
	if err != nil {
		m.Logger.Debug("measure destination version count failed, using assumed value",
			"secret", dstKey, "assumed", assumed, "err", err)
		return assumed
	}
	return len(dstMeta.Data.Versions)
}

// warnDestTruncated logs when destination retention kept fewer versions
// than the source had. This is the destination honoring its own configured
// max_versions -- expected, not an error -- so it only warns; it never
// fails or retries.
func (m *Migrator) warnDestTruncated(srcKey string, srcCount, destCount int) {
	if destCount < srcCount {
		m.Logger.Warn("destination retention truncated version history",
			"secret", srcKey, "source_versions", srcCount, "dest_versions", destCount)
	}
}

func (m *Migrator) verifyVersionHashes(ctx context.Context, srcKey, dstKey string, srcMeta *kv2MetadataResp, existingState *state.Secret) (bool, error) {
	if existingState == nil || existingState.VersionHashes == nil {
		return false, nil
	}

	maxV := getMaxVersion(srcMeta)

	for v := 1; v <= maxV; v++ {
		vm, ok := srcMeta.Data.Versions[strconv.Itoa(v)]
		if !ok || vm.Destroyed {
			continue
		}

		expectedHash, hasHash := existingState.VersionHashes[strconv.Itoa(v)]
		if !hasHash {
			continue
		}

		payload, err := m.kv2ReadVersion(ctx, m.Src, srcKey, v)
		if err != nil {
			return false, fmt.Errorf("read version %d: %w", v, err)
		}

		actualHash, err := state.HashPayload(payload)
		if err != nil {
			return false, fmt.Errorf("hash version %d: %w", v, err)
		}

		if actualHash != expectedHash {
			return false, nil
		}
	}

	return true, nil
}

func (m *Migrator) verifyDestinationMatches(ctx context.Context, srcKey, dstKey string, srcMeta *kv2MetadataResp) (bool, error) {
	maxV := getMaxVersion(srcMeta)

	dstMeta, err := m.kv2ReadMetadata(ctx, m.Dst, dstKey)
	if err != nil {
		return false, fmt.Errorf("read destination metadata: %w", err)
	}

	for v := 1; v <= maxV; v++ {
		vm, ok := srcMeta.Data.Versions[strconv.Itoa(v)]
		if !ok || vm.Destroyed {
			continue
		}

		if _, destHas := dstMeta.Data.Versions[strconv.Itoa(v)]; !destHas {
			// Version was pruned away by the destination's own retention
			// window (max_versions) -- e.g. dest max_versions=3 but the
			// source has 5 readable versions. This is NOT a genuine
			// mismatch: the version simply no longer exists to compare.
			// Erroring here (as this used to) sent copyOneSecretWithState
			// down the delete+recopy path (kvv2.go verifyDestinationMatches
			// caller), which immediately re-prunes back to the identical
			// state -- a destructive, non-idempotent loop that repeats
			// EVERY run. Skip the version instead of failing the
			// comparison over it.
			continue
		}

		srcPayload, err := m.kv2ReadVersion(ctx, m.Src, srcKey, v)
		if err != nil {
			return false, fmt.Errorf("read source version %d: %w", v, err)
		}

		dstPayload, err := m.kv2ReadVersion(ctx, m.Dst, dstKey, v)
		if err != nil {
			return false, fmt.Errorf("read destination version %d: %w", v, err)
		}

		srcHash, err := state.HashPayload(srcPayload)
		if err != nil {
			return false, fmt.Errorf("hash source version %d: %w", v, err)
		}

		dstHash, err := state.HashPayload(dstPayload)
		if err != nil {
			return false, fmt.Errorf("hash destination version %d: %w", v, err)
		}

		if srcHash != dstHash {
			return false, nil
		}
	}

	return true, nil
}

func (m *Migrator) kv2DeleteSecret(ctx context.Context, c KVV2Cluster, relKey string) error {

	m.Logger.Debug("DELETE", "kvv2-secret", relKey)
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/metadata/" + relKey

	_, err := c.Client.Logical().DeleteWithContext(ctx, path)
	return err
}

func (m *Migrator) kv2ReadMetadata(ctx context.Context, c KVV2Cluster, relKey string) (*kv2MetadataResp, error) {

	m.Logger.Debug("READ", "kvv2-metadata", relKey)
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/metadata/" + relKey

	sec, err := c.Client.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if sec == nil || sec.Data == nil {
		// Vault's Read() collapses a bare 404 (no warnings/data) into
		// (nil, nil) rather than an error. Surface it as a "not found"
		// so callers using isMetadataNotFound (e.g. destination-exists
		// checks) treat a missing secret the same whether the API returned
		// an explicit 404 error or this nil/nil shape.
		return nil, fmt.Errorf("%w: empty metadata response for %q", errMetadataNotFound, path)
	}

	wrapped := map[string]any{"data": sec.Data}

	var out kv2MetadataResp
	if err := mapToStruct(wrapped, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (m *Migrator) kv2ReadVersion(ctx context.Context, c KVV2Cluster, relKey string, version int) (map[string]any, error) {

	m.Logger.Debug("READ", "kvv2-secret", relKey, "version", version)
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/data/" + relKey

	sec, err := c.Client.Logical().ReadWithDataWithContext(ctx, path, map[string][]string{
		"version": {strconv.Itoa(version)},
	})
	if err != nil {
		return nil, err
	}
	if sec == nil || sec.Data == nil {
		return nil, fmt.Errorf("empty read response for %q version=%d", path, version)
	}

	wrapped := map[string]any{"data": sec.Data}
	var out kv2ReadVersionResp
	if err := mapToStruct(wrapped, &out); err != nil {
		return nil, err
	}
	return out.Data.Data, nil
}

func (m *Migrator) kv2WriteData(ctx context.Context, c KVV2Cluster, relKey string, data map[string]any) error {

	m.Logger.Debug("WRITE", "kvv2-secret", relKey)
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/data/" + relKey

	_, err := c.Client.Logical().WriteWithContext(ctx, path, map[string]any{
		"data": data,
	})
	return err
}

func (m *Migrator) kv2DeleteVersions(ctx context.Context, c KVV2Cluster, relKey string, versions []int) error {
	if len(versions) == 0 {
		return nil
	}
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/delete/" + relKey

	_, err := c.Client.Logical().WriteWithContext(ctx, path, map[string]any{
		"versions": versions,
	})
	return err
}

func (m *Migrator) kv2DestroyVersions(ctx context.Context, c KVV2Cluster, relKey string, versions []int) error {
	if len(versions) == 0 {
		return nil
	}
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/destroy/" + relKey

	_, err := c.Client.Logical().WriteWithContext(ctx, path, map[string]any{
		"versions": versions,
	})
	return err
}

func (m *Migrator) kv2WriteMetadataSettings(ctx context.Context, c KVV2Cluster, relKey string, meta *kv2MetadataResp) error {

	m.Logger.Debug("WRITE", "kvv2-metadata", relKey)
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/metadata/" + relKey

	body := map[string]any{
		"cas_required":         meta.Data.CASRequired,
		"max_versions":         meta.Data.MaxVersions,
		"delete_version_after": meta.Data.DeleteVersionAfter,
	}
	if len(meta.Data.CustomMetadata) > 0 {
		body["custom_metadata"] = meta.Data.CustomMetadata
	}

	_, err := c.Client.Logical().WriteWithContext(ctx, path, body)
	return err
}
