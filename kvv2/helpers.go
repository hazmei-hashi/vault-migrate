package kvv2

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
)

// walkAllKeys returns leaf secret keys relative to the mount
func (m *Migrator) walkAllKeys(ctx context.Context, c KVV2Cluster, startPrefix string) ([]string, error) {
	log.Printf("Scanning base path: %s\n", startPrefix)
	startPrefix = trimSlashes(startPrefix)

	var out []string
	var rec func(prefix string) error

	rec = func(prefix string) error {
		keys, err := m.kv2List(ctx, c, prefix)
		if err != nil {
			// Treat missing prefix as empty.
			if isNotFound(err) {
				return nil
			}
			return err
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
	log.Printf("Listing contents at path: %s\n", relPrefix)
	relPrefix = trimSlashes(relPrefix)

	mount := trimSlashes(c.MountPath)
	logical := c.Client.Logical()

	// Root listing can be finicky across setups; try both forms.
	// 1) "<mount>/metadata" (no trailing slash)
	// 2) "<mount>/metadata/" (with trailing slash)
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

func (m *Migrator) kv2ReadMetadata(ctx context.Context, c KVV2Cluster, relKey string) (*kv2MetadataResp, error) {
	log.Printf("Scanning secret: %s -> ", relKey)
	relKey = trimSlashes(relKey)
	path := trimSlashes(c.MountPath) + "/metadata/" + relKey

	sec, err := c.Client.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if sec == nil || sec.Data == nil {
		return nil, fmt.Errorf("empty metadata response for %q", path)
	}

	wrapped := map[string]any{"data": sec.Data}

	var out kv2MetadataResp
	if err := mapToStruct(wrapped, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (m *Migrator) kv2ReadVersion(ctx context.Context, c KVV2Cluster, relKey string, version int) (map[string]any, error) {
	log.Printf("Reading secret: %s (version %b) -> ", relKey, version)
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
	log.Printf("Writing secret to destination: %s -> ", relKey)
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
	log.Printf("Setting metadata: %s\n", relKey)
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

func (m *Migrator) dstKeyFor(srcRelKey string) string {
	srcRelKey = trimSlashes(srcRelKey)
	srcBase := trimSlashes(m.Src.BasePath)
	dstBase := trimSlashes(m.Dst.BasePath)

	if srcBase == "" {
		return joinRel(dstBase, srcRelKey)
	}

	if srcRelKey == srcBase {
		return dstBase
	}

	prefix := srcBase + "/"
	if strings.HasPrefix(srcRelKey, prefix) {
		suffix := strings.TrimPrefix(srcRelKey, prefix)
		return joinRel(dstBase, suffix)
	}

	return joinRel(dstBase, srcRelKey)
}

func trimSlashes(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	return s
}

func joinRel(a, b string) string {
	a = trimSlashes(a)
	b = trimSlashes(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "/" + b
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "permission denied") == false && // not 403
		(strings.Contains(msg, "404") ||
			strings.Contains(msg, "not found") ||
			strings.Contains(msg, "no handler for route") ||
			strings.Contains(msg, "unsupported path"))
}

func mapToStruct(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
