package kvv2

import (
	"encoding/json"
	"errors"
	"strings"
)

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
	if errors.Is(err, errMetadataNotFound) {
		return true
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
