package state

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

func HashPayload(data map[string]any) (string, error) {
	if data == nil {
		return "", nil
	}

	jsonBytes, err := json.Marshal(sortedMap(data))
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	hash := sha256.Sum256(jsonBytes)
	return fmt.Sprintf("sha256:%x", hash), nil
}

func HashMetadata(casRequired bool, maxVersions int, deleteVersionAfter string, customMetadata map[string]string) (string, error) {
	data := map[string]any{
		"cas_required":         casRequired,
		"max_versions":         maxVersions,
		"delete_version_after": deleteVersionAfter,
		"custom_metadata":      customMetadata,
	}

	jsonBytes, err := json.Marshal(sortedMap(data))
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}

	hash := sha256.Sum256(jsonBytes)
	return fmt.Sprintf("sha256:%x", hash), nil
}

func sortedMap(m map[string]any) map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	sorted := make(map[string]any, len(m))
	for _, k := range keys {
		sorted[k] = m[k]
	}
	return sorted
}
