package kvv2

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hashicorp/vault/api"
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
	return strings.Trim(s, "/")
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

// isMetadataNotFound reports whether err represents a genuine "not found"
// for a KV v2 metadata/data path -- structurally, not by scanning the error
// message.
//
// Renamed from isNotFound: the generic name invited generic (substring)
// matching. There are exactly two callers, both checking the same thing
// (a metadata read came back empty), so the specific name says what it
// actually verifies.
//
// WHY structural-only, no substring fallback (B17):
//
// Vault's SDK collapses every real 404 into (nil, nil) before an error ever
// reaches this package:
//   - Logical.Read / ReadWithContext -> ReadWithDataWithContext ->
//     ParseRawResponseAndCloseBody (api/logical.go ~line 142): a bare 404
//     (no data/warnings) returns (nil, nil), not an error.
//   - Logical.List / ListWithContext -> list() (api/logical.go ~line 206):
//     same collapse for LIST 404s.
//   - api/secret.go ParseSecret (~line 375): a response containing only
//     {"errors": [...]} with no other keys also becomes (nil, nil).
//
// Because of that collapse, a genuine "not found" from this package's call
// sites arrives one of two ways: as our own errMetadataNotFound sentinel
// (kv2ReadMetadata wraps the nil/nil case, see kvv2.go), or not as an error
// at all. The *api.ResponseError/404 arm below is therefore UNREACHABLE
// through any currently-known SDK code path -- it exists purely as
// forward-insurance in case a future SDK version, custom RoundTripper, or
// different call site starts surfacing 404s as errors again. The old
// substring checks ("404", "not found", "no handler for route",
// "unsupported path") and the "permission denied" negative guard added
// nothing over this: they matched on error TEXT, so a genuine 500 whose
// message happened to contain "404" (e.g. a key literally named
// "app/error-404") was misclassified as not-found, silently swallowing real
// failures (403, 400 "unsupported path" on a non-KV2 mount, 5xx, timeouts)
// wherever a caller branched on isNotFound. Do not re-add substring
// matching; it re-opens exactly that hole.
func isMetadataNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errMetadataNotFound) {
		return true
	}
	var re *api.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

func mapToStruct(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
