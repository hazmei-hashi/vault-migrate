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

// isCASRequiredError reports whether err is Vault's 400 "check-and-set
// parameter required for this call" response
// (vault-plugin-secrets-kv@v0.26.2 path_data.go:286-288) -- the ONLY case
// kv2WriteData's B19 retry fires on.
//
// DELIBERATE, NARROW exception to B17's no-substring-matching rule (see
// isMetadataNotFound above, and TODO.md B17). B17's substring matching
// caused a FALSE POSITIVE: a real error (a KV v1 mount's literal
// "unsupported path", or any message that happened to contain "404") got
// misclassified as not-found and SILENTLY SWALLOWED -- an entire subtree
// vanished from the migration while it still exited 0. That is the
// dangerous direction: matching too eagerly turns a real failure into a
// false success.
//
// Here a match failure produces the opposite: a FALSE NEGATIVE. If this
// substring check fails to match (e.g. a future Vault release rewords the
// message), isCASRequiredError simply returns false, kv2WriteData skips the
// retry, and the original 400 propagates unchanged -- degrading to exactly
// today's loud, pre-B19 failure. There is no path from a wrong match here to
// a silent swallow: the retry is purely additive convenience on top of an
// error that was already going to be returned to the caller.
//
// A structural (status/type-only) check is NOT possible for this case: a
// genuine cas MISMATCH ("check-and-set parameter did not match the current
// version") and a MISSING cas ("check-and-set parameter required for this
// call") are both plain 400 *api.ResponseError with no distinguishing field
// -- path_data.go:283-288 returns the identical shape for both branches,
// only the message text differs. Do NOT "fix" this into a structural check;
// one cannot be built without losing the mismatch/missing distinction this
// retry depends on. If HashiCorp ever changes this exact string, this match
// stops firing and B19 reappears exactly as it was before this fix -- a
// clear, loud regression to notice and re-diagnose, never a silent one.
func isCASRequiredError(err error) bool {
	if err == nil {
		return false
	}
	var re *api.ResponseError
	if !errors.As(err, &re) || re.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(err.Error(), "check-and-set parameter required")
}

func mapToStruct(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
