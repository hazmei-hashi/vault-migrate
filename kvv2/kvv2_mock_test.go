package kvv2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"vault-migrate/config"
	"vault-migrate/state"

	"github.com/hashicorp/vault/api"
)

type fakeKVVersion struct {
	Data         map[string]any
	DeletionTime string
	Destroyed    bool
}

type fakeKVSecret struct {
	CASRequired        bool
	MaxVersions        int
	DeleteVersionAfter string
	CustomMetadata     map[string]string
	Versions           map[int]*fakeKVVersion
	CurrentVersion     int
}

type fakeVault struct {
	mu sync.Mutex
	// mountMaxVersions mirrors the KV v2 mount-level "max_versions" tunable
	// (`vault secrets tune`). 0 means unset, same as a per-secret MaxVersions
	// of 0.
	mountMaxVersions int
	// mountCASRequired mirrors the KV v2 mount-level "cas_required" tunable.
	// Real Vault enforces check-and-set when EITHER the mount config OR the
	// per-secret metadata has cas_required=true (path_data.go:278-288 in
	// vault-plugin-secrets-kv@v0.26.2). See Task 1.
	mountCASRequired bool
	// deleteSecretCalls counts kv2DeleteSecret calls against this fake, used
	// by Task 4's idempotency test to assert a second migration run performs
	// zero destination deletes.
	deleteSecretCalls int
	// forceListErrorStatus, when non-zero, makes every metadata LIST request
	// fail with this HTTP status instead of returning real data. Used by
	// Task 2's end-to-end proof that a genuine LIST error (403/500/etc.)
	// propagates out of walkAllKeys as a non-nil error instead of being
	// silently swallowed as "empty subtree".
	forceListErrorStatus int
	// forceListErrorMessage is the error string served alongside
	// forceListErrorStatus. Lets tests reproduce specific real-Vault error
	// texts (e.g. literally "unsupported path" for a KV v1 mount, or a path
	// that happens to contain the digits "404") that the old substring-based
	// isNotFound used to misclassify as not-found.
	forceListErrorMessage string
	// forceMetadataReadErrorKey, when non-empty, makes a metadata GET (not
	// LIST) for exactly this key fail with forceMetadataReadErrorStatus
	// (default 500). Used to test that a destination metadata re-read for
	// bookkeeping (measured DestVersionCount) degrades gracefully instead
	// of aborting the migration.
	forceMetadataReadErrorKey    string
	forceMetadataReadErrorStatus int
	secrets                      map[string]*fakeKVSecret
	server                       *httptest.Server
	// dataWriteBodies records every /data/<relKey> POST/PUT request body,
	// in order, for Task 5's common-path regression locks: proving a
	// non-cas_required destination never sends an "options" key at all,
	// and that no extra request is issued per version.
	dataWriteBodies []map[string]any
	// metadataGETCount counts every metadata GET (not LIST) request, used
	// by Task 5 to prove Task 2's CAS retry issues zero metadata reads on
	// the common (non-cas_required) path.
	metadataGETCount int
	// afterMetadataGET, when set for a key, fires exactly once immediately
	// after that key's next metadata GET response is served, then clears
	// itself. Used by TestKV2WriteData_CASMismatchIsNotRetried to simulate
	// a genuine concurrent writer racing between B19's CAS seed read and
	// its single retry write.
	afterMetadataGET map[string]func()
	// forceDataWriteErrorKey/Status/Message, when Key is non-empty, makes
	// every /data/<key> POST/PUT fail with Status/Message instead of
	// reaching the normal CAS-gate/write logic -- simulating a real
	// non-CAS failure (permission denied, KV v1 "unsupported path", 5xx)
	// that has nothing to do with check-and-set, for Task 5's B17
	// regression locks.
	forceDataWriteErrorKey     string
	forceDataWriteErrorStatus  int
	forceDataWriteErrorMessage string
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()

	f := &fakeVault{
		secrets: make(map[string]*fakeKVSecret),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeVault) newClient(t *testing.T) *api.Client {
	t.Helper()

	cfg := api.DefaultConfig()
	cfg.Address = f.server.URL
	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create api client: %v", err)
	}
	client.SetToken("test-token")
	return client
}

func (f *fakeVault) ensureSecret(key string) *fakeKVSecret {
	sec, ok := f.secrets[key]
	if ok {
		return sec
	}
	sec = &fakeKVSecret{
		Versions:       make(map[int]*fakeKVVersion),
		CustomMetadata: make(map[string]string),
	}
	f.secrets[key] = sec
	return sec
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in)
	out := make(map[string]any)
	_ = json.Unmarshal(b, &out)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// defaultMaxVersions mirrors KV v2's built-in default retention window when
// neither the mount nor the secret has max_versions configured.
const defaultMaxVersions = 10

// pruneVersions implements KV v2's sliding-window retention (Task 1b): on
// every write, if the version count exceeds the effective max_versions, the
// oldest versions are dropped from the metadata map entirely (not merely
// soft-deleted -- gone, same as real Vault's storage GC for pruned versions).
// Effective retention = max(per-secret MaxVersions, mount-level
// mountMaxVersions), falling back to defaultMaxVersions when both are 0.
// Caller must hold f.mu.
func (f *fakeVault) pruneVersions(sec *fakeKVSecret) {
	effective := sec.MaxVersions
	if f.mountMaxVersions > effective {
		effective = f.mountMaxVersions
	}
	if effective <= 0 {
		effective = defaultMaxVersions
	}

	if len(sec.Versions) <= effective {
		return
	}

	oldest := sec.CurrentVersion - effective
	for v := range sec.Versions {
		if v <= oldest {
			delete(sec.Versions, v)
		}
	}
}

func (f *fakeVault) putVersion(key string, data map[string]any) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec := f.ensureSecret(key)
	sec.CurrentVersion++
	sec.Versions[sec.CurrentVersion] = &fakeKVVersion{Data: cloneMap(data)}
	f.pruneVersions(sec)
	return sec.CurrentVersion
}

func (f *fakeVault) setMetadata(key string, casRequired bool, maxVersions int, deleteVersionAfter string, customMetadata map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec := f.ensureSecret(key)
	sec.CASRequired = casRequired
	sec.MaxVersions = maxVersions
	sec.DeleteVersionAfter = deleteVersionAfter
	sec.CustomMetadata = cloneStringMap(customMetadata)
}

// setMountMaxVersions sets the mount-level max_versions tunable used as a
// fallback when a secret has no per-secret MaxVersions of its own (Task 1c).
func (f *fakeVault) setMountMaxVersions(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mountMaxVersions = n
}

// setMountCASRequired sets the mount-level cas_required tunable
// (`vault secrets tune -cas-required=true`). Real Vault enforces
// check-and-set on every data write when this OR the per-secret
// cas_required is true (Task 1).
func (f *fakeVault) setMountCASRequired(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mountCASRequired = v
}

// deleteCalls returns how many times kv2DeleteSecret (metadata DELETE) has
// been issued against this fake, for Task 4's idempotency assertion.
func (f *fakeVault) deleteCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.deleteSecretCalls
}

// dataWrites returns a copy of every /data/ POST/PUT request body recorded
// so far, in request order, for Task 5's common-path regression locks.
func (f *fakeVault) dataWrites() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]map[string]any, len(f.dataWriteBodies))
	copy(out, f.dataWriteBodies)
	return out
}

// metadataGETs returns how many metadata GET (not LIST) requests have been
// issued so far, for Task 5's proof that the CAS retry issues zero extra
// metadata reads on a destination that never required CAS.
func (f *fakeVault) metadataGETs() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.metadataGETCount
}

// setAfterMetadataGET registers hook to run exactly once, immediately after
// the next metadata GET response for key is served, then clears itself.
// Used by TestKV2WriteData_CASMismatchIsNotRetried to inject a concurrent
// writer between B19's CAS seed read and its single retry write.
func (f *fakeVault) setAfterMetadataGET(key string, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.afterMetadataGET == nil {
		f.afterMetadataGET = make(map[string]func())
	}
	f.afterMetadataGET[key] = hook
}

// setForceDataWriteError makes every subsequent /data/<key> POST/PUT fail
// with status/message instead of reaching the normal CAS-gate/write logic,
// simulating a real non-CAS failure (403 permission denied, 500, etc.) for
// Task 5's regression lock that such errors propagate on the first attempt
// with no retry and no metadata read.
func (f *fakeVault) setForceDataWriteError(key string, status int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forceDataWriteErrorKey = key
	f.forceDataWriteErrorStatus = status
	f.forceDataWriteErrorMessage = message
}

// bumpCurrentVersionLocked adds a new version directly, bypassing
// kv2WriteData/handleData's CAS gate entirely -- it simulates a genuine
// concurrent writer racing in via some other path. Caller must already
// hold f.mu (this is meant to be invoked from inside a handler, e.g. an
// afterMetadataGET hook).
func (f *fakeVault) bumpCurrentVersionLocked(key string, data map[string]any) {
	sec := f.ensureSecret(key)
	sec.CurrentVersion++
	sec.Versions[sec.CurrentVersion] = &fakeKVVersion{Data: cloneMap(data)}
}

// setForceListError makes every subsequent metadata LIST request fail with
// the given HTTP status and error message instead of returning real data,
// for Task 2's end-to-end proof that walkAllKeys surfaces genuine LIST
// errors instead of swallowing them. Passing status alone reproduces a
// generic failure; a custom message lets a test reproduce a specific
// real-Vault error string (e.g. a KV v1 mount's literal "unsupported path").
func (f *fakeVault) setForceListError(status int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forceListErrorStatus = status
	f.forceListErrorMessage = message
}

// setForceMetadataReadError makes the next metadata GET for key fail with
// status (defaults to 500 if 0), used to test that a destination metadata
// re-read failure for bookkeeping purposes (measured DestVersionCount)
// degrades gracefully instead of aborting the migration.
func (f *fakeVault) setForceMetadataReadError(key string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forceMetadataReadErrorKey = key
	f.forceMetadataReadErrorStatus = status
}

func (f *fakeVault) setVersionData(key string, version int, data map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec := f.ensureSecret(key)
	v, ok := sec.Versions[version]
	if !ok {
		sec.Versions[version] = &fakeKVVersion{Data: cloneMap(data)}
		if version > sec.CurrentVersion {
			sec.CurrentVersion = version
		}
		return
	}
	v.Data = cloneMap(data)
}

func (f *fakeVault) removeVersion(key string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec, ok := f.secrets[key]
	if !ok {
		return
	}
	delete(sec.Versions, version)
}

// rawVersionData reads a version's stored payload directly out of the
// fake's internal map, bypassing handleData's soft-delete/destroy 404
// gating. Needed for B18's tests: once a version is soft-deleted,
// kv2ReadVersion (correctly) errors on it, but the tests still need to
// assert on what payload actually got WRITTEN before the delete call --
// a real placeholder vs. the pre-fix null write.
func (f *fakeVault) rawVersionData(key string, version int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec, ok := f.secrets[key]
	if !ok {
		return nil
	}
	vm, ok := sec.Versions[version]
	if !ok {
		return nil
	}
	return vm.Data
}

func (f *fakeVault) markVersionDeleted(key string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec, ok := f.secrets[key]
	if !ok {
		return
	}
	vm, ok := sec.Versions[version]
	if !ok {
		return
	}
	vm.DeletionTime = time.Now().UTC().Format(time.RFC3339)
}

// markVersionDeletionScheduled sets a deletion_time on a version WITHOUT
// making it unreadable, for the B18 regression test: `delete_version_after`
// stamps a future deletion_time on write, and real Vault keeps that
// version's data fully readable until the deadline actually passes
// (vault-plugin-secrets-kv path_data.go). Passing a time.Time in the past
// behaves the same as markVersionDeleted; passing one in the future
// reproduces the "readable, deletion_time set" case the B18 fix must not
// treat as unavailable.
func (f *fakeVault) markVersionDeletionScheduled(key string, version int, deletionTime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec, ok := f.secrets[key]
	if !ok {
		return
	}
	vm, ok := sec.Versions[version]
	if !ok {
		return
	}
	vm.DeletionTime = deletionTime.UTC().Format(time.RFC3339)
}

func (f *fakeVault) markVersionDestroyed(key string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec, ok := f.secrets[key]
	if !ok {
		return
	}
	vm, ok := sec.Versions[version]
	if !ok {
		return
	}
	vm.Destroyed = true
}

func (f *fakeVault) listKeys(prefix string) []string {
	prefix = trimSlashes(prefix)
	seen := make(map[string]struct{})

	for key := range f.secrets {
		var remaining string
		if prefix == "" {
			remaining = key
		} else {
			base := prefix + "/"
			if !strings.HasPrefix(key, base) {
				continue
			}
			remaining = strings.TrimPrefix(key, base)
		}

		if remaining == "" {
			continue
		}

		if idx := strings.Index(remaining, "/"); idx >= 0 {
			seen[remaining[:idx+1]] = struct{}{}
		} else {
			seen[remaining] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isListRequest(r *http.Request) bool {
	if r.Method == "LIST" {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-HTTP-Method-Override"), "LIST") {
		return true
	}
	return r.URL.Query().Get("list") == "true"
}

func readBodyMap(r *http.Request) (map[string]any, error) {
	defer r.Body.Close()

	body := make(map[string]any)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		if err == io.EOF {
			return body, nil
		}
		return nil, err
	}
	return body, nil
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func parseVersions(body map[string]any) []int {
	raw, ok := body["versions"]
	if !ok || raw == nil {
		return nil
	}

	switch vv := raw.(type) {
	case []int:
		return vv
	case []any:
		out := make([]int, 0, len(vv))
		for _, v := range vv {
			out = append(out, asInt(v))
		}
		return out
	default:
		return nil
	}
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"errors": []string{"not found"},
	})
}

// isActuallyDeleted reports whether a version's deletion_time deadline has
// passed. An empty deletionTime means never scheduled for deletion. A
// deletionTime that fails to parse is treated as already-deleted -- real
// Vault's own timestamp is always well-formed, so a bad value here is a
// test-construction mistake, not a case worth silently treating as
// "readable". Used by handleData so a FUTURE-dated deletion_time (set via
// `delete_version_after`) keeps serving real data, per B18's critical
// warning: skip-on-read must key off the read itself producing nil data,
// never off DeletionTime != "" alone.
func isActuallyDeleted(deletionTime string) bool {
	if deletionTime == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, deletionTime)
	if err != nil {
		return true
	}
	return !t.After(time.Now().UTC())
}

// writeNotFoundWithData serves real Vault's shape for a read against a
// deleted/destroyed KV v2 version: HTTP 404, but WITH a "data" key (no
// "errors" key) carrying nil data plus the version metadata. See
// vault-plugin-secrets-kv path_data.go pathDataRead. This is the shape the
// SDK's ParseRawResponseAndCloseBody (api/logical.go) treats as a 404 that
// still has data/warnings, which api/secret.go's DeepEqual check then
// reports as SUCCESS (secret, nil) rather than (nil, nil) -- unlike a bare
// 404 for a version/secret that never existed at all.
func writeNotFoundWithData(w http.ResponseWriter, version int, destroyed bool, deletionTime string) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"data": map[string]any{
			"data": nil,
			"metadata": map[string]any{
				"version":       version,
				"deletion_time": deletionTime,
				"destroyed":     destroyed,
			},
		},
		"warnings": nil,
	})
}

func (f *fakeVault) handle(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		writeNotFound(w)
		return
	}

	kind := parts[1]
	relKey := ""
	if len(parts) == 3 {
		relKey = trimSlashes(parts[2])
	}

	switch kind {
	case "metadata":
		f.handleMetadata(w, r, relKey)
	case "data":
		f.handleData(w, r, relKey)
	case "delete":
		f.handleDelete(w, r, relKey)
	case "destroy":
		f.handleDestroy(w, r, relKey)
	default:
		writeNotFound(w)
	}
}

func (f *fakeVault) handleMetadata(w http.ResponseWriter, r *http.Request, relKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if isListRequest(r) {
		if f.forceListErrorStatus != 0 {
			msg := f.forceListErrorMessage
			if msg == "" {
				msg = "injected failure"
			}
			writeJSON(w, f.forceListErrorStatus, map[string]any{
				"errors": []string{msg},
			})
			return
		}
		keys := f.listKeys(relKey)
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"keys": keys,
			},
		})
		return
	}

	switch r.Method {
	case http.MethodDelete:
		delete(f.secrets, relKey)
		f.deleteSecretCalls++
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		f.metadataGETCount++
		if f.forceMetadataReadErrorKey != "" && f.forceMetadataReadErrorKey == relKey {
			status := f.forceMetadataReadErrorStatus
			if status == 0 {
				status = http.StatusInternalServerError
			}
			writeJSON(w, status, map[string]any{"errors": []string{"injected metadata read failure"}})
			return
		}
		sec, ok := f.secrets[relKey]
		if !ok {
			writeNotFound(w)
			return
		}

		versions := make(map[string]any, len(sec.Versions))
		for v, meta := range sec.Versions {
			versions[strconv.Itoa(v)] = map[string]any{
				"deletion_time": meta.DeletionTime,
				"destroyed":     meta.Destroyed,
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"cas_required":         sec.CASRequired,
				"current_version":      sec.CurrentVersion,
				"delete_version_after": sec.DeleteVersionAfter,
				"max_versions":         sec.MaxVersions,
				"custom_metadata":      sec.CustomMetadata,
				"versions":             versions,
			},
		})
		if hook, ok := f.afterMetadataGET[relKey]; ok {
			delete(f.afterMetadataGET, relKey)
			hook()
		}
		return
	case http.MethodPost, http.MethodPut:
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []string{err.Error()}})
			return
		}

		sec := f.ensureSecret(relKey)
		if cas, ok := body["cas_required"].(bool); ok {
			sec.CASRequired = cas
		}
		if max, ok := body["max_versions"]; ok {
			sec.MaxVersions = asInt(max)
		}
		if dva, ok := body["delete_version_after"].(string); ok {
			sec.DeleteVersionAfter = dva
		}
		if cm, ok := body["custom_metadata"].(map[string]any); ok {
			sec.CustomMetadata = make(map[string]string, len(cm))
			for k, v := range cm {
				sec.CustomMetadata[k] = fmt.Sprintf("%v", v)
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{}})
		return
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeVault) handleData(w http.ResponseWriter, r *http.Request, relKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		sec, ok := f.secrets[relKey]
		if !ok {
			writeNotFound(w)
			return
		}

		version := sec.CurrentVersion
		if qv := r.URL.Query().Get("version"); qv != "" {
			parsed, err := strconv.Atoi(qv)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []string{"invalid version"}})
				return
			}
			version = parsed
		}

		v, ok := sec.Versions[version]
		if !ok {
			// Version never existed in metadata at all -> real Vault's
			// storage layer returns nil,nil for a genuinely absent key,
			// which the plugin turns into a bare 404 (no data key). See
			// Task 1a.
			writeNotFound(w)
			return
		}
		if v.Destroyed || isActuallyDeleted(v.DeletionTime) {
			// Version exists but has been soft-deleted or destroyed ->
			// real Vault serves 404-with-data (see writeNotFoundWithData).
			//
			// A non-empty DeletionTime alone does NOT mean unreadable:
			// `delete_version_after` stamps a FUTURE deletion_time on the
			// version metadata at write time, but real Vault's KV v2
			// plugin only actually soft-deletes the version once its
			// background reaper fires at that deadline
			// (delete_version_after.go) -- the data stays fully readable
			// until then. isActuallyDeleted checks the deadline has
			// passed, not just that the field is set (B18 regression
			// coverage for exactly this distinction).
			writeNotFoundWithData(w, version, v.Destroyed, v.DeletionTime)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"data": v.Data,
				"metadata": map[string]any{
					"version":       version,
					"deletion_time": v.DeletionTime,
					"destroyed":     v.Destroyed,
				},
			},
		})
		return
	case http.MethodPost, http.MethodPut:
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []string{err.Error()}})
			return
		}
		f.dataWriteBodies = append(f.dataWriteBodies, body)

		if f.forceDataWriteErrorKey != "" && f.forceDataWriteErrorKey == relKey {
			status := f.forceDataWriteErrorStatus
			if status == 0 {
				status = http.StatusInternalServerError
			}
			msg := f.forceDataWriteErrorMessage
			if msg == "" {
				msg = "injected data write failure"
			}
			writeJSON(w, status, map[string]any{"errors": []string{msg}})
			return
		}

		// Real Vault enforces check-and-set at write time when EITHER the
		// mount-level or per-secret cas_required is true
		// (vault-plugin-secrets-kv@v0.26.2 path_data.go:278-288): a write
		// with no "options.cas" fails 400 with this exact error string. An
		// existing secret keeps its recorded CASRequired; a brand-new key
		// (not yet in f.secrets) can only be gated by the mount, since it
		// has no per-secret metadata yet.
		existing, hasExisting := f.secrets[relKey]
		casRequired := f.mountCASRequired
		if hasExisting && existing.CASRequired {
			casRequired = true
		}
		currentVersion := 0
		if hasExisting {
			currentVersion = existing.CurrentVersion
		}

		options, _ := body["options"].(map[string]any)
		casVal, hasCAS := options["cas"]
		if hasCAS {
			// path_data.go:283-284 -- "if casOk" validates the cas VALUE
			// unconditionally, before ever checking cas_required. A
			// caller that sends cas=N on a destination that never asked
			// for CAS still gets rejected if N is wrong. Mirror that here
			// so an implementation sending a wrong/hardcoded cas value
			// (e.g. always 0, or CurrentVersion+1) fails this mock the
			// same way it would fail real Vault, instead of passing every
			// mock test on presence alone.
			if asInt(casVal) != currentVersion {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"errors": []string{"check-and-set parameter did not match the current version"},
				})
				return
			}
		} else if casRequired {
			// path_data.go:286-288 -- "else if config.CasRequired ||
			// meta.CasRequired" only fires when cas was NOT sent at all.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"errors": []string{"check-and-set parameter required for this call"},
			})
			return
		}

		payload, _ := body["data"].(map[string]any)
		sec := f.ensureSecret(relKey)
		sec.CurrentVersion++
		sec.Versions[sec.CurrentVersion] = &fakeKVVersion{Data: cloneMap(payload)}
		f.pruneVersions(sec)

		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{"version": sec.CurrentVersion},
		})
		return
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeVault) handleDelete(w http.ResponseWriter, r *http.Request, relKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := readBodyMap(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []string{err.Error()}})
		return
	}

	sec, ok := f.secrets[relKey]
	if !ok {
		writeNotFound(w)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, v := range parseVersions(body) {
		if vm, ok := sec.Versions[v]; ok {
			vm.DeletionTime = now
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{}})
}

func (f *fakeVault) handleDestroy(w http.ResponseWriter, r *http.Request, relKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := readBodyMap(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []string{err.Error()}})
		return
	}

	sec, ok := f.secrets[relKey]
	if !ok {
		writeNotFound(w)
		return
	}

	for _, v := range parseVersions(body) {
		if vm, ok := sec.Versions[v]; ok {
			vm.Destroyed = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{}})
}

func newTestMigrator(t *testing.T, src, dst *fakeVault, withState bool) *Migrator {
	t.Helper()

	m := &Migrator{
		Src: KVV2Cluster{
			Client:    src.newClient(t),
			MountPath: "secret",
		},
		Dst: KVV2Cluster{
			Client:    dst.newClient(t),
			MountPath: "secret",
		},
		Config: config.VaultClientConfig{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if withState {
		m.State = state.NewMigrationState(
			state.ClusterInfo{Address: src.server.URL, Mount: "secret", BasePath: ""},
			state.ClusterInfo{Address: dst.server.URL, Mount: "secret", BasePath: ""},
		)
		m.StateFile = filepath.Join(t.TempDir(), "state.json")
	}

	return m
}

// newTestMigratorWithLogBuf is newTestMigrator plus a buffer capturing all
// log output, for tests asserting on a specific Logger.Warn message (e.g.
// destination retention truncation warnings).
func newTestMigratorWithLogBuf(t *testing.T, src, dst *fakeVault, withState bool) (*Migrator, *bytes.Buffer) {
	t.Helper()

	m := newTestMigrator(t, src, dst, withState)
	var buf bytes.Buffer
	m.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	return m, &buf
}

func useStdinInput(t *testing.T, input string) {
	t.Helper()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdin pipe: %v", err)
	}

	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("failed to write stdin input: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close stdin writer: %v", err)
	}

	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})
}

func TestRun_DryRun_DoesNotWriteDestination(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/dry", map[string]any{"value": "source"})
	m := newTestMigrator(t, src, dst, false)

	err := m.Run(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("Run dry-run failed: %v", err)
	}

	if _, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/dry"); err == nil {
		t.Fatalf("destination metadata exists after dry-run; expected no writes")
	}
}

// TestRun_ZeroKeys_RefusesToReportSuccess is B9: Run must error rather than
// exit 0 whenever walkAllKeys returns zero keys, since 0 keys is
// indistinguishable (from the SDK's point of view) from a missing mount, a
// KV v1 mount, a typo'd mount, or a typo'd base path -- all of which would
// otherwise silently report a "successful" migration that copied nothing.
func TestRun_ZeroKeys_RefusesToReportSuccess(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
	}{
		{"genuinely empty mount", ""},
		{"nonexistent mount subtree (typo'd base path)", "does/not/exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)

			m := newTestMigrator(t, src, dst, false)
			m.Src.BasePath = tt.basePath

			err := m.Run(context.Background(), Options{})
			if err == nil {
				t.Fatalf("expected Run to fail on 0 keys, got nil error")
			}
			if !strings.Contains(err.Error(), "refusing to report success") {
				t.Fatalf("unexpected Run error: %v", err)
			}
		})
	}
}

// TestRun_ZeroKeys_NonexistentMount proves the same guard fires for a mount
// that was never created at all (as opposed to an existing-but-empty one),
// exercised through Init's full prompt-driven path so the failure is not a
// silent success end to end.
func TestRun_ZeroKeys_NonexistentMount(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	// dst gets a secret so a nonexistent-mount migration cannot be confused
	// with a fully-empty test double; src intentionally has zero secrets.
	dst.putVersion("unrelated/secret", map[string]any{"value": "noop"})

	useStdinInput(t, "totally-nonexistent-mount\n\nsecret\n\n")

	err := Init(
		src.newClient(t),
		dst.newClient(t),
		config.VaultClientConfig{
			NoState:  true,
			LogLevel: "error",
		},
	)
	if err == nil {
		t.Fatalf("expected Init to fail for a nonexistent source mount, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to report success") {
		t.Fatalf("unexpected Init error: %v", err)
	}
}

func TestRun_ContinueOnError_ProcessesRemainingSecrets(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/a", map[string]any{"value": "src-a-v1"})
	src.putVersion("app/b", map[string]any{"value": "src-b-v1"})

	// Destination ahead for app/a -> copyOneSecretWithState returns error.
	dst.putVersion("app/a", map[string]any{"value": "dst-a-v1"})
	dst.putVersion("app/a", map[string]any{"value": "dst-a-v2"})

	m := newTestMigrator(t, src, dst, true)

	err := m.Run(context.Background(), Options{ContinueOnError: true})
	if err != nil {
		t.Fatalf("Run with ContinueOnError should not fail, got: %v", err)
	}

	b, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/b", 1)
	if err != nil {
		t.Fatalf("app/b should be copied despite app/a failure: %v", err)
	}
	if b["value"] != "src-b-v1" {
		t.Fatalf("app/b value = %v, want src-b-v1", b["value"])
	}

	st := m.State.GetSecret("app/a")
	if st == nil || st.Status != "failed" {
		t.Fatalf("app/a state = %+v, want failed", st)
	}
}

func TestRun_StopOnFirstError_WhenContinueOnErrorFalse(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/a", map[string]any{"value": "src-a-v1"})
	src.putVersion("app/b", map[string]any{"value": "src-b-v1"})

	// Destination ahead for app/a.
	dst.putVersion("app/a", map[string]any{"value": "dst-a-v1"})
	dst.putVersion("app/a", map[string]any{"value": "dst-a-v2"})

	m := newTestMigrator(t, src, dst, true)

	err := m.Run(context.Background(), Options{ContinueOnError: false})
	if err == nil {
		t.Fatalf("expected Run to fail when ContinueOnError=false")
	}
	if !strings.Contains(err.Error(), "destination has more versions than source") {
		t.Fatalf("unexpected Run error: %v", err)
	}

	if _, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/b"); err == nil {
		t.Fatalf("app/b should not be copied after first error")
	}
}

func TestInit_WithPromptInput_MigratesSecrets(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/init", map[string]any{"value": "from-init"})

	useStdinInput(t, "secret\n\nsecret\n\n")

	err := Init(
		src.newClient(t),
		dst.newClient(t),
		config.VaultClientConfig{
			NoState:         true,
			LogLevel:        "error",
			ContinueOnError: false,
		},
	)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	m := newTestMigrator(t, src, dst, false)
	v, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/init", 1)
	if err != nil {
		t.Fatalf("destination secret missing after Init: %v", err)
	}
	if v["value"] != "from-init" {
		t.Fatalf("destination value = %v, want from-init", v["value"])
	}
}

func TestInit_EmptyMount_Rejected(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	// A blank line at the mount prompt must be rejected (re-prompted) rather
	// than silently accepted as an empty MountPath. Since only one line of
	// input is provided and it's consumed by the re-prompt, Init should
	// fail with EOF instead of proceeding with an empty mount.
	useStdinInput(t, "\n")

	err := Init(
		src.newClient(t),
		dst.newClient(t),
		config.VaultClientConfig{
			NoState:  true,
			LogLevel: "error",
		},
	)
	if err == nil {
		t.Fatalf("expected Init to fail on empty mount input, got nil error")
	}
}

// TestInit_SlashOnlyMount_Rejected proves that a mount value consisting only
// of slashes is rejected (re-prompted) rather than accepted as non-empty and
// silently normalized to an empty MountPath.
//
// Each input supplies exactly the 4 lines that TestInit_WithPromptInput_MigratesSecrets
// uses for a *successful* run ("<mount>\n<basepath>\n<dst-mount>\n<dst-basepath>\n"),
// just with a slash-only value standing in for the source mount. Before Fix
// 1, PromptRequired validated pre-normalization, so e.g. "/" passed as
// "non-empty" input, trimSlashes("/") -> "", and Init ran to completion with
// err == nil (0 keys migrated, logged as "Migration completed") - the exact
// silent no-op this fix set out to kill. Confirmed against pre-fix init.go:
// these same 4-line inputs return a nil error there because the slash-only
// value is accepted outright, leaving 3 spare lines to satisfy every
// remaining prompt. Post-fix, promptMount's re-prompt loop consumes those
// spare lines trying to find a real mount value, runs out of input, and
// Init correctly fails with io.EOF instead.
func TestInit_SlashOnlyMount_Rejected(t *testing.T) {
	inputs := map[string]string{
		"single slash": "/\n\nsecret\n\n",
		"double slash": "//\n\nsecret\n\n",
		"triple slash": "///\n\nsecret\n\n",
		"spaced slash": " / \n\nsecret\n\n",
	}

	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)

			useStdinInput(t, input)

			err := Init(
				src.newClient(t),
				dst.newClient(t),
				config.VaultClientConfig{
					NoState:  true,
					LogLevel: "error",
				},
			)
			if err == nil {
				t.Fatalf("expected Init to fail on slash-only mount input %q, got nil error", input)
			}
		})
	}
}

// TestPromptMount_NormalizesOrRejects exercises promptMount directly across
// valid mounts (with/without stray slashes) and slash-only/blank input that
// must re-prompt to EOF.
func TestPromptMount_NormalizesOrRejects(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain", input: "secret\n", want: "secret"},
		{name: "leading and trailing slash", input: "/secret/\n", want: "secret"},
		{name: "doubled leading and trailing slash", input: "//app//\n", want: "app"},
		{name: "single slash re-prompts to EOF", input: "/\n", wantErr: true},
		{name: "double slash re-prompts to EOF", input: "//\n", wantErr: true},
		{name: "triple slash re-prompts to EOF", input: "///\n", wantErr: true},
		{name: "spaced single slash re-prompts to EOF", input: " / \n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useStdinInput(t, tt.input)

			got, err := promptMount("mount: ")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("promptMount(%q) = %q, nil; want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("promptMount(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("promptMount(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestWalkAllKeys_WithNestedTree(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/db/password", map[string]any{"value": "db-pass"})
	src.putVersion("app/api/key", map[string]any{"value": "api-key"})
	src.putVersion("root", map[string]any{"value": "top"})

	m := newTestMigrator(t, src, dst, false)

	keys, err := m.walkAllKeys(context.Background(), m.Src, "")
	if err != nil {
		t.Fatalf("walkAllKeys failed: %v", err)
	}

	want := []string{"app/api/key", "app/db/password", "root"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("walkAllKeys keys = %v, want %v", keys, want)
	}
}

// TestWalkAllKeys_PropagatesGenuineListErrors is B17's end-to-end proof: a
// genuine LIST error (403 permission denied, 500, or 400 "unsupported path"
// against a non-KV2 mount) must make walkAllKeys return a non-nil error,
// NOT silently return an empty/partial key list while Run still exits 0.
//
// Before the fix, the isNotFound(err) branch inside walkAllKeys' rec()
// treated every LIST error alike as "missing prefix" and returned nil,
// discarding the whole subtree under prefix and any real cause. This test
// was confirmed to FAIL against that pre-fix code (git stash + run), proving
// it actually exercises the bug rather than trivially passing either way.
func TestWalkAllKeys_PropagatesGenuineListErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
	}{
		{"403 permission denied", http.StatusForbidden, ""},
		{"500 internal server error", http.StatusInternalServerError, ""},
		{"400 unsupported path (kv1 mount)", http.StatusBadRequest, "unsupported path"},
		{"500 with path containing digits 404", http.StatusInternalServerError, "failed to read secret/metadata/app/error-404"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)

			// A secret must exist so the first LIST call has something to
			// fail on (an empty mount would return keys=[] before any error
			// path is exercised).
			src.putVersion("app/secret", map[string]any{"value": "v1"})
			src.setForceListError(tt.status, tt.message)

			m := newTestMigrator(t, src, dst, false)

			keys, err := m.walkAllKeys(context.Background(), m.Src, "")
			if err == nil {
				t.Fatalf("walkAllKeys returned nil error for a %d LIST failure (msg=%q, keys=%v); genuine errors must propagate, not be swallowed as an empty subtree", tt.status, tt.message, keys)
			}
		})
	}
}

func TestCopySecretFull_TracksStateAndMetadata(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "v1"})
	src.putVersion("app/secret", map[string]any{"value": "v2"})
	src.setMetadata("app/secret", true, 7, "24h", map[string]string{"owner": "platform"})

	m := newTestMigrator(t, src, dst, true)

	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("kv2ReadMetadata src failed: %v", err)
	}

	err = m.copySecretFull(context.Background(), "app/secret", "app/secret", srcMeta, Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copySecretFull failed: %v", err)
	}

	dstMeta, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/secret")
	if err != nil {
		t.Fatalf("kv2ReadMetadata dst failed: %v", err)
	}

	if dstMeta.Data.CASRequired != true {
		t.Fatalf("dst CASRequired = %v, want true", dstMeta.Data.CASRequired)
	}
	if dstMeta.Data.MaxVersions != 7 {
		t.Fatalf("dst MaxVersions = %d, want 7", dstMeta.Data.MaxVersions)
	}
	if dstMeta.Data.DeleteVersionAfter != "24h" {
		t.Fatalf("dst DeleteVersionAfter = %q, want 24h", dstMeta.Data.DeleteVersionAfter)
	}
	if dstMeta.Data.CustomMetadata["owner"] != "platform" {
		t.Fatalf("dst custom_metadata.owner = %q, want platform", dstMeta.Data.CustomMetadata["owner"])
	}

	v1, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", 1)
	if err != nil {
		t.Fatalf("read dst v1 failed: %v", err)
	}
	if v1["value"] != "v1" {
		t.Fatalf("dst v1 value = %v, want v1", v1["value"])
	}

	v2, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", 2)
	if err != nil {
		t.Fatalf("read dst v2 failed: %v", err)
	}
	if v2["value"] != "v2" {
		t.Fatalf("dst v2 value = %v, want v2", v2["value"])
	}

	secretState := m.State.GetSecret("app/secret")
	if secretState == nil {
		t.Fatalf("expected state for app/secret")
	}
	if secretState.Status != "completed" {
		t.Fatalf("state status = %q, want completed", secretState.Status)
	}
	if secretState.SourceVersionCount != 2 || secretState.DestVersionCount != 2 {
		t.Fatalf("state version counts = (%d, %d), want (2, 2)", secretState.SourceVersionCount, secretState.DestVersionCount)
	}
	if secretState.VersionHashes["1"] == "" || secretState.VersionHashes["2"] == "" {
		t.Fatalf("expected version hashes for v1 and v2, got %+v", secretState.VersionHashes)
	}
	if secretState.MetadataChecksum == "" {
		t.Fatalf("expected metadata checksum to be set")
	}

	if _, err := os.Stat(m.StateFile); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
}

func TestCopyOneSecret_HandlesVersionEdgeCases(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/edge", map[string]any{"value": "v1"})
	src.putVersion("app/edge", map[string]any{"value": "v2"})
	src.putVersion("app/edge", map[string]any{"value": "v3"})

	// v1 exists in metadata but is deleted (read will fail -> placeholder + delete)
	src.markVersionDeleted("app/edge", 1)
	// v2 missing from metadata map entirely (placeholder path)
	src.removeVersion("app/edge", 2)
	// v3 destroyed (placeholder + destroy)
	src.markVersionDestroyed("app/edge", 3)

	m := newTestMigrator(t, src, dst, false)

	err := m.copyOneSecret(context.Background(), "app/edge", "app/edge", Options{
		Placeholder: map[string]any{
			"_vault_migrate": "placeholder",
			"_reason":        "source_version_unavailable",
		},
	})
	if err != nil {
		t.Fatalf("copyOneSecret failed: %v", err)
	}

	dstMeta, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/edge")
	if err != nil {
		t.Fatalf("read dst metadata failed: %v", err)
	}

	v1 := dstMeta.Data.Versions["1"]
	if v1.DeletionTime == "" {
		t.Fatalf("dst v1 should be marked deleted")
	}

	v3 := dstMeta.Data.Versions["3"]
	if !v3.Destroyed {
		t.Fatalf("dst v3 should be marked destroyed")
	}

	v2, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/edge", 2)
	if err != nil {
		t.Fatalf("read dst v2 failed: %v", err)
	}
	if v2["_reason"] != "missing_in_metadata" {
		t.Fatalf("dst v2 _reason = %v, want missing_in_metadata", v2["_reason"])
	}
}

// TestKV2ReadVersion_SoftDeletedVersionReturnsErrVersionDataUnavailable is
// B18's unit-level fix proof: kv2ReadVersion must turn Vault's
// 404-with-data ("read succeeded but data is nil") shape for a
// soft-deleted version into errVersionDataUnavailable, not a silent
// (nil-map, nil) success. Previously named
// TestCopyOneSecret_BugA_DeletedVersionReadSucceedsWithEmptyPayload and
// asserted the opposite (buggy) behavior -- renamed and inverted now that
// the bug is fixed.
func TestKV2ReadVersion_SoftDeletedVersionReturnsErrVersionDataUnavailable(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/softdel", map[string]any{"value": "v1"})
	src.markVersionDeleted("app/softdel", 1)

	m := newTestMigrator(t, src, dst, false)

	payload, err := m.kv2ReadVersion(context.Background(), m.Src, "app/softdel", 1)
	if !errors.Is(err, errVersionDataUnavailable) {
		t.Fatalf("kv2ReadVersion err = %v, want errVersionDataUnavailable", err)
	}
	if payload != nil {
		t.Fatalf("kv2ReadVersion payload = %v, want nil alongside the error", payload)
	}
}

// TestCopyPaths_SoftDeletedVersionGetsPlaceholder is B18's end-to-end proof
// across all three copy paths: a soft-deleted source version must produce
// the configured placeholder on the destination (with "_reason" present),
// never a null/empty payload, and the state label for that version must
// stay "deleted" (copySecretFull/copyIncrementalVersions only -- copyOneSecret
// does not track VersionStates at all).
func TestCopyPaths_SoftDeletedVersionGetsPlaceholder(t *testing.T) {
	placeholder := map[string]any{
		"_vault_migrate": "placeholder",
		"_reason":        "source_version_unavailable",
	}

	tests := []struct {
		name string
		run  func(t *testing.T, m *Migrator, srcMeta *kv2MetadataResp) (versionStates map[string]string)
	}{
		{
			name: "copyOneSecret",
			run: func(t *testing.T, m *Migrator, _ *kv2MetadataResp) map[string]string {
				if err := m.copyOneSecret(context.Background(), "app/softdel", "app/softdel", Options{Placeholder: placeholder}); err != nil {
					t.Fatalf("copyOneSecret failed: %v", err)
				}
				return nil
			},
		},
		{
			name: "copySecretFull",
			run: func(t *testing.T, m *Migrator, srcMeta *kv2MetadataResp) map[string]string {
				if err := m.copySecretFull(context.Background(), "app/softdel", "app/softdel", srcMeta, Options{Placeholder: placeholder}); err != nil {
					t.Fatalf("copySecretFull failed: %v", err)
				}
				return m.State.GetSecret("app/softdel").VersionStates
			},
		},
		{
			name: "copyIncrementalVersions",
			run: func(t *testing.T, m *Migrator, srcMeta *kv2MetadataResp) map[string]string {
				// Seed dest with nothing so the whole range [1,1] is
				// "incremental" -- exercises the same loop body as a real
				// incremental run picking up a soft-deleted tail version.
				if err := m.copyIncrementalVersions(context.Background(), "app/softdel", "app/softdel", srcMeta, 1, 1, Options{Placeholder: placeholder}); err != nil {
					t.Fatalf("copyIncrementalVersions failed: %v", err)
				}
				return m.State.GetSecret("app/softdel").VersionStates
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)

			src.putVersion("app/softdel", map[string]any{"value": "v1"})
			src.markVersionDeleted("app/softdel", 1)

			m := newTestMigrator(t, src, dst, true)

			srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/softdel")
			if err != nil {
				t.Fatalf("kv2ReadMetadata src failed: %v", err)
			}

			versionStates := tt.run(t, m, srcMeta)

			// The destination version itself was soft-deleted to mirror
			// the source, so read the underlying stored payload directly
			// rather than through kv2ReadVersion (which would now, itself
			// correctly, return errVersionDataUnavailable).
			gotPayload := dst.rawVersionData("app/softdel", 1)
			if len(gotPayload) == 0 {
				t.Fatalf("dst v1 payload is empty; want the configured placeholder written before soft-delete")
			}
			if gotPayload["_reason"] != "source_version_unavailable" {
				t.Fatalf("dst v1 payload = %+v, want placeholder with _reason=source_version_unavailable", gotPayload)
			}
			if _, hasValue := gotPayload["value"]; hasValue {
				t.Fatalf("dst v1 payload = %+v, still carries the real source value -- placeholder was not written", gotPayload)
			}

			if versionStates != nil {
				if got := versionStates["1"]; got != "deleted" {
					t.Fatalf("VersionStates[\"1\"] = %q, want \"deleted\" (must not regress to read_error)", got)
				}
			}

			dstMeta, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/softdel")
			if err != nil {
				t.Fatalf("kv2ReadMetadata dst failed: %v", err)
			}
			if dstMeta.Data.Versions["1"].DeletionTime == "" {
				t.Fatalf("dst v1 should be marked deleted to mirror the source")
			}
		})
	}
}

// TestKV2ReadVersion_FutureDeletionTimeStillReadsRealData is the regression
// test for B18's critical warning: a version with a FUTURE-dated
// deletion_time (set via `delete_version_after`, not yet reaped by Vault)
// is still fully readable with real data. The fix must key off the read
// itself returning nil data, never off DeletionTime != "" alone -- keying
// off the metadata field would cause genuine loss of live, currently
// accessible data.
func TestKV2ReadVersion_FutureDeletionTimeStillReadsRealData(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/scheduled", map[string]any{"value": "still-here"})
	src.markVersionDeletionScheduled("app/scheduled", 1, time.Now().Add(24*time.Hour))

	m := newTestMigrator(t, src, dst, false)

	payload, err := m.kv2ReadVersion(context.Background(), m.Src, "app/scheduled", 1)
	if err != nil {
		t.Fatalf("kv2ReadVersion on a future-scheduled (still readable) version failed: %v", err)
	}
	if payload["value"] != "still-here" {
		t.Fatalf("payload = %+v, want real data {value: still-here}, not a placeholder", payload)
	}
}

// TestCopySecretFull_FutureDeletionTimeCopiesRealData is
// TestKV2ReadVersion_FutureDeletionTimeStillReadsRealData at the copy-path
// level: copySecretFull must write the version's REAL data to the
// destination, not a placeholder, and label it "active" (not "deleted") --
// the deadline hasn't passed, so nothing has actually been soft-deleted yet.
func TestCopySecretFull_FutureDeletionTimeCopiesRealData(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/scheduled", map[string]any{"value": "still-here"})
	src.markVersionDeletionScheduled("app/scheduled", 1, time.Now().Add(24*time.Hour))
	m := newTestMigrator(t, src, dst, true)

	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/scheduled")
	if err != nil {
		t.Fatalf("kv2ReadMetadata src failed: %v", err)
	}

	if err := m.copySecretFull(context.Background(), "app/scheduled", "app/scheduled", srcMeta, Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder", "_reason": "source_version_unavailable"},
	}); err != nil {
		t.Fatalf("copySecretFull failed: %v", err)
	}

	v1, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/scheduled", 1)
	if err != nil {
		t.Fatalf("read dst v1 failed: %v", err)
	}
	if v1["value"] != "still-here" {
		t.Fatalf("dst v1 = %+v, want real data {value: still-here}, got a placeholder instead", v1)
	}

	secretState := m.State.GetSecret("app/scheduled")
	if secretState == nil {
		t.Fatalf("expected state for app/scheduled")
	}
	if got := secretState.VersionStates["1"]; got != "active" {
		t.Fatalf("VersionStates[\"1\"] = %q, want \"active\" (deadline has not passed, data was actually copied)", got)
	}
}

// TestCopyOneSecretWithState_SoftDeletedVersionIsIdempotent locks B18's fix
// against the exact failure mode the pruning bug had earlier this session:
// a soft-deleted source version must not, on a second run over an already
// migrated secret, trigger a destination delete+recopy. Verified via the
// fake's deleteCalls() counter, which must stay at 0 across both runs.
func TestCopyOneSecretWithState_SoftDeletedVersionIsIdempotent(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/softdel", map[string]any{"value": "v1"})
	src.putVersion("app/softdel", map[string]any{"value": "v2"})
	src.markVersionDeleted("app/softdel", 1)

	m := newTestMigrator(t, src, dst, true)
	opts := Options{Placeholder: map[string]any{"_vault_migrate": "placeholder", "_reason": "source_version_unavailable"}}

	if err := m.copyOneSecretWithState(context.Background(), "app/softdel", "app/softdel", opts); err != nil {
		t.Fatalf("first copyOneSecretWithState run failed: %v", err)
	}
	if got := dst.deleteCalls(); got != 0 {
		t.Fatalf("deleteCalls after first run = %d, want 0", got)
	}

	if err := m.copyOneSecretWithState(context.Background(), "app/softdel", "app/softdel", opts); err != nil {
		t.Fatalf("second (idempotent) copyOneSecretWithState run failed: %v", err)
	}
	if got := dst.deleteCalls(); got != 0 {
		t.Fatalf("deleteCalls after idempotent re-run = %d, want 0 (soft-deleted version must not trigger delete+recopy)", got)
	}
}

func TestCopyIncrementalVersions_AppendsNewVersion(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	p1 := map[string]any{"value": "v1"}
	p2 := map[string]any{"value": "v2"}
	p3 := map[string]any{"value": "v3"}

	src.putVersion("app/secret", p1)
	src.putVersion("app/secret", p2)
	src.putVersion("app/secret", p3)

	dst.putVersion("app/secret", p1)
	dst.putVersion("app/secret", p2)

	m := newTestMigrator(t, src, dst, true)

	h1, _ := state.HashPayload(p1)
	h2, _ := state.HashPayload(p2)
	m.State.UpdateSecret("app/secret", &state.Secret{
		Status:             "completed",
		SourceVersionCount: 2,
		DestVersionCount:   2,
		VersionHashes: map[string]string{
			"1": h1,
			"2": h2,
		},
		VersionStates: map[string]string{
			"1": "active",
			"2": "active",
		},
	})

	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("kv2ReadMetadata src failed: %v", err)
	}

	err = m.copyIncrementalVersions(context.Background(), "app/secret", "app/secret", srcMeta, 3, 3, Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyIncrementalVersions failed: %v", err)
	}

	v3, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", 3)
	if err != nil {
		t.Fatalf("read dst v3 failed: %v", err)
	}
	if v3["value"] != "v3" {
		t.Fatalf("dst v3 value = %v, want v3", v3["value"])
	}

	secretState := m.State.GetSecret("app/secret")
	if secretState == nil {
		t.Fatalf("expected state for app/secret")
	}
	if secretState.SourceVersionCount != 3 || secretState.DestVersionCount != 3 {
		t.Fatalf("state version counts = (%d, %d), want (3, 3)", secretState.SourceVersionCount, secretState.DestVersionCount)
	}
	if secretState.VersionHashes["3"] == "" {
		t.Fatalf("expected version hash for v3, got %+v", secretState.VersionHashes)
	}
}

func TestKVWrappers_DeleteDestroyDeleteSecret(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/wrappers", map[string]any{"value": "v1"})
	dst.putVersion("app/wrappers", map[string]any{"value": "v2"})

	m := newTestMigrator(t, src, dst, false)

	if err := m.kv2DeleteVersions(context.Background(), m.Dst, "app/wrappers", []int{1}); err != nil {
		t.Fatalf("kv2DeleteVersions failed: %v", err)
	}
	if err := m.kv2DestroyVersions(context.Background(), m.Dst, "app/wrappers", []int{2}); err != nil {
		t.Fatalf("kv2DestroyVersions failed: %v", err)
	}

	meta, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/wrappers")
	if err != nil {
		t.Fatalf("read metadata failed: %v", err)
	}

	if meta.Data.Versions["1"].DeletionTime == "" {
		t.Fatalf("version 1 should be marked deleted")
	}
	if !meta.Data.Versions["2"].Destroyed {
		t.Fatalf("version 2 should be marked destroyed")
	}

	if err := m.kv2DeleteSecret(context.Background(), m.Dst, "app/wrappers"); err != nil {
		t.Fatalf("kv2DeleteSecret failed: %v", err)
	}
	if _, err := m.kv2ReadMetadata(context.Background(), m.Dst, "app/wrappers"); err == nil {
		t.Fatalf("expected metadata read error after delete secret")
	}
}

func TestVerifyDestinationMatches(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		src := newFakeVault(t)
		dst := newFakeVault(t)

		src.putVersion("app/secret", map[string]any{"value": "v1"})
		src.putVersion("app/secret", map[string]any{"value": "v2"})
		dst.putVersion("app/secret", map[string]any{"value": "v1"})
		dst.putVersion("app/secret", map[string]any{"value": "v2"})

		m := newTestMigrator(t, src, dst, false)
		srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
		if err != nil {
			t.Fatalf("read src metadata failed: %v", err)
		}

		ok, err := m.verifyDestinationMatches(context.Background(), "app/secret", "app/secret", srcMeta)
		if err != nil {
			t.Fatalf("verifyDestinationMatches err = %v", err)
		}
		if !ok {
			t.Fatalf("verifyDestinationMatches = false, want true")
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		src := newFakeVault(t)
		dst := newFakeVault(t)

		src.putVersion("app/secret", map[string]any{"value": "v1"})
		src.putVersion("app/secret", map[string]any{"value": "v2"})
		dst.putVersion("app/secret", map[string]any{"value": "v1"})
		dst.putVersion("app/secret", map[string]any{"value": "v2"})
		dst.setVersionData("app/secret", 2, map[string]any{"value": "changed"})

		m := newTestMigrator(t, src, dst, false)
		srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
		if err != nil {
			t.Fatalf("read src metadata failed: %v", err)
		}

		ok, err := m.verifyDestinationMatches(context.Background(), "app/secret", "app/secret", srcMeta)
		if err != nil {
			t.Fatalf("verifyDestinationMatches err = %v", err)
		}
		if ok {
			t.Fatalf("verifyDestinationMatches = true, want false")
		}
	})
}

func TestVerifyVersionHashes(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	p1 := map[string]any{"value": "v1"}
	p2 := map[string]any{"value": "v2"}
	src.putVersion("app/secret", p1)
	src.putVersion("app/secret", p2)

	m := newTestMigrator(t, src, dst, false)
	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("read src metadata failed: %v", err)
	}

	h1, _ := state.HashPayload(p1)
	h2, _ := state.HashPayload(p2)
	existing := &state.Secret{
		VersionHashes: map[string]string{"1": h1, "2": h2},
	}

	ok, err := m.verifyVersionHashes(context.Background(), "app/secret", "app/secret", srcMeta, existing)
	if err != nil {
		t.Fatalf("verifyVersionHashes err = %v", err)
	}
	if !ok {
		t.Fatalf("verifyVersionHashes = false, want true")
	}

	existing.VersionHashes["2"] = "sha256:mismatch"
	ok, err = m.verifyVersionHashes(context.Background(), "app/secret", "app/secret", srcMeta, existing)
	if err != nil {
		t.Fatalf("verifyVersionHashes mismatch err = %v", err)
	}
	if ok {
		t.Fatalf("verifyVersionHashes = true with mismatched hash, want false")
	}
}

func TestCopyOneSecretWithState_DestinationAheadFails(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.putVersion("app/secret", map[string]any{"value": "v2"})

	m := newTestMigrator(t, src, dst, true)

	err := m.copyOneSecretWithState(context.Background(), "app/secret", "app/secret", Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err == nil {
		t.Fatalf("expected error when destination has more versions")
	}
	if !strings.Contains(err.Error(), "destination has more versions") {
		t.Fatalf("unexpected error: %v", err)
	}

	secretState := m.State.GetSecret("app/secret")
	if secretState == nil {
		t.Fatalf("expected state entry for app/secret")
	}
	if secretState.Status != "failed" {
		t.Fatalf("state status = %q, want failed", secretState.Status)
	}
	if secretState.SourceVersionCount != 1 || secretState.DestVersionCount != 2 {
		t.Fatalf("state version counts = (%d, %d), want (1, 2)", secretState.SourceVersionCount, secretState.DestVersionCount)
	}
}

func TestCopyOneSecretWithState_IncrementalBranch(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/incremental", map[string]any{"value": "v1"})
	src.putVersion("app/incremental", map[string]any{"value": "v2"})
	src.putVersion("app/incremental", map[string]any{"value": "v3"})
	dst.putVersion("app/incremental", map[string]any{"value": "v1"})

	m := newTestMigrator(t, src, dst, true)

	err := m.copyOneSecretWithState(context.Background(), "app/incremental", "app/incremental", Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyOneSecretWithState incremental failed: %v", err)
	}

	v3, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/incremental", 3)
	if err != nil {
		t.Fatalf("read destination v3 failed: %v", err)
	}
	if v3["value"] != "v3" {
		t.Fatalf("destination v3 value = %v, want v3", v3["value"])
	}

	sec := m.State.GetSecret("app/incremental")
	if sec == nil {
		t.Fatalf("expected state for app/incremental")
	}
	if sec.Status != "completed" {
		t.Fatalf("state status = %q, want completed", sec.Status)
	}
	if sec.DestVersionCount != 3 {
		t.Fatalf("DestVersionCount = %d, want 3", sec.DestVersionCount)
	}
}

func TestCopyOneSecretWithState_RecreateWhenDestinationMismatch(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/recreate", map[string]any{"value": "source"})
	dst.putVersion("app/recreate", map[string]any{"value": "destination"})

	m := newTestMigrator(t, src, dst, true)

	err := m.copyOneSecretWithState(context.Background(), "app/recreate", "app/recreate", Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyOneSecretWithState recreate failed: %v", err)
	}

	v1, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/recreate", 1)
	if err != nil {
		t.Fatalf("read recreated v1 failed: %v", err)
	}
	if v1["value"] != "source" {
		t.Fatalf("recreated v1 value = %v, want source", v1["value"])
	}
}

func TestCopyOneSecretWithState_SkipsWhenHashesMatch(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	p1 := map[string]any{"value": "v1"}
	src.putVersion("app/secret", p1)
	dst.putVersion("app/secret", p1)

	m := newTestMigrator(t, src, dst, true)
	h1, _ := state.HashPayload(p1)
	m.State.UpdateSecret("app/secret", &state.Secret{
		Status:           "completed",
		VersionHashes:    map[string]string{"1": h1},
		VersionStates:    map[string]string{"1": "active"},
		MetadataChecksum: "sha256:test",
	})

	err := m.copyOneSecretWithState(context.Background(), "app/secret", "app/secret", Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyOneSecretWithState failed: %v", err)
	}

	secretState := m.State.GetSecret("app/secret")
	if secretState == nil {
		t.Fatalf("expected state entry for app/secret")
	}
	if secretState.Status != "skipped" {
		t.Fatalf("state status = %q, want skipped", secretState.Status)
	}
	if secretState.SourceVersionCount != 1 || secretState.DestVersionCount != 1 {
		t.Fatalf("state version counts = (%d, %d), want (1, 1)", secretState.SourceVersionCount, secretState.DestVersionCount)
	}
}

// TestFakeVault_PruneVersions exercises Task 1b: on write, the fake prunes
// versions older than the sliding retention window, mirroring real Vault's
// max_versions behavior (effective = max(per-secret, mount), default 10).
func TestFakeVault_PruneVersions(t *testing.T) {
	tests := []struct {
		name             string
		writes           int
		perSecretMax     int
		mountMax         int
		wantOldestExists int // oldest version number expected to survive
		wantCount        int // total versions expected to remain
	}{
		{
			name:             "under default retention keeps everything",
			writes:           5,
			wantOldestExists: 1,
			wantCount:        5,
		},
		{
			name:             "exceeds default retention prunes to 10",
			writes:           12,
			wantOldestExists: 3,
			wantCount:        10,
		},
		{
			name:             "per-secret max_versions overrides default",
			writes:           5,
			perSecretMax:     3,
			wantOldestExists: 3,
			wantCount:        3,
		},
		{
			name:             "mount max_versions used when higher than per-secret",
			writes:           8,
			perSecretMax:     2,
			mountMax:         5,
			wantOldestExists: 4,
			wantCount:        5,
		},
		{
			name:             "per-secret max_versions used when higher than mount",
			writes:           8,
			perSecretMax:     6,
			mountMax:         2,
			wantOldestExists: 3,
			wantCount:        6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeVault(t)
			if tt.mountMax > 0 {
				f.setMountMaxVersions(tt.mountMax)
			}
			if tt.perSecretMax > 0 {
				f.setMetadata("app/prune", false, tt.perSecretMax, "", nil)
			}

			for i := 1; i <= tt.writes; i++ {
				f.putVersion("app/prune", map[string]any{"n": i})
			}

			f.mu.Lock()
			sec := f.secrets["app/prune"]
			gotCount := len(sec.Versions)
			_, oldestExists := sec.Versions[tt.wantOldestExists]
			_, prunedExists := sec.Versions[tt.wantOldestExists-1]
			f.mu.Unlock()

			if gotCount != tt.wantCount {
				t.Fatalf("remaining version count = %d, want %d", gotCount, tt.wantCount)
			}
			if !oldestExists {
				t.Fatalf("expected version %d to survive pruning, it did not", tt.wantOldestExists)
			}
			if tt.wantOldestExists > 1 && prunedExists {
				t.Fatalf("expected version %d to be pruned, but it still exists", tt.wantOldestExists-1)
			}
		})
	}
}

// TestRun_PrunedDestination_DoesNotRecopyOnSecondRun is Task 4's idempotency
// proof: destination retention (max_versions=3) is lower than the source's 5
// readable versions, so versions 1-2 are pruned away from the destination's
// own metadata immediately after the first migration writes them. A second
// migration run must NOT delete-and-recopy the secret over those pruned
// versions.
//
// The bug required verifyDestinationMatches to be reached with no cached
// state to short-circuit it (existingState == nil or without version
// hashes) -- exactly the "state absent/stale" case called out in the task:
// a completed first run's hash cache lets a second run skip via
// verifyVersionHashes without ever touching verifyDestinationMatches, which
// would mask this bug. This test therefore uses a second, independent
// Migrator (fresh, empty state) against the same destination backend for
// "run 2", reproducing a state file that was lost/never loaded for this
// secret while the destination itself still holds the previously-migrated
// (now partially pruned) versions.
//
// Pre-fix, verifyDestinationMatches errored trying to read a pruned-away
// destination version, which copyOneSecretWithState treated as "verification
// failed" and answered by deleting the destination secret and doing a full
// recopy -- which pruned right back to the same state, forever, every run.
func TestRun_PrunedDestination_DoesNotRecopyOnSecondRun(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)
	dst.setMountMaxVersions(3)

	src.putVersion("app/pruned", map[string]any{"value": "v1"})
	src.putVersion("app/pruned", map[string]any{"value": "v2"})
	src.putVersion("app/pruned", map[string]any{"value": "v3"})
	src.putVersion("app/pruned", map[string]any{"value": "v4"})
	src.putVersion("app/pruned", map[string]any{"value": "v5"})

	m1 := newTestMigrator(t, src, dst, true)
	if err := m1.Run(context.Background(), Options{}); err != nil {
		t.Fatalf("first Run failed: %v", err)
	}

	dstMeta, err := m1.kv2ReadMetadata(context.Background(), m1.Dst, "app/pruned")
	if err != nil {
		t.Fatalf("read dst metadata after first run failed: %v", err)
	}
	if _, ok := dstMeta.Data.Versions["1"]; ok {
		t.Fatalf("expected destination version 1 to be pruned away after first run, but it exists")
	}
	if _, ok := dstMeta.Data.Versions["3"]; !ok {
		t.Fatalf("expected destination version 3 to survive pruning after first run")
	}

	deletesAfterFirstRun := dst.deleteCalls()

	// Fresh Migrator, fresh (empty) state -> reproduces a state file that
	// never recorded this secret, forcing copyOneSecretWithState into the
	// verifyDestinationMatches path instead of the cached-hash skip.
	m2 := newTestMigrator(t, src, dst, true)
	if err := m2.Run(context.Background(), Options{}); err != nil {
		t.Fatalf("second Run failed: %v", err)
	}

	if got := dst.deleteCalls(); got != deletesAfterFirstRun {
		t.Fatalf("second run issued %d kv2DeleteSecret call(s) (delta), want 0 -- destructive recopy loop regressed", got-deletesAfterFirstRun)
	}
}

// TestVerifyDestinationMatches_PrunedVersionSkipped is Task 4's unit-level
// proof that a version absent from destination metadata (pruned by
// max_versions) is skipped rather than failing verification, while a
// version PRESENT on the destination with genuinely different content still
// fails it.
func TestVerifyDestinationMatches_PrunedVersionSkipped(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "v1"})
	src.putVersion("app/secret", map[string]any{"value": "v2"})
	dst.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.putVersion("app/secret", map[string]any{"value": "v2"})
	// Version 1 is intentionally removed from the destination's metadata (as
	// if pruned by max_versions), not present-but-different.
	dst.removeVersion("app/secret", 1)

	m := newTestMigrator(t, src, dst, false)
	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("read src metadata failed: %v", err)
	}

	ok, err := m.verifyDestinationMatches(context.Background(), "app/secret", "app/secret", srcMeta)
	if err != nil {
		t.Fatalf("verifyDestinationMatches err = %v, want nil (pruned version must be skipped, not errored)", err)
	}
	if !ok {
		t.Fatalf("verifyDestinationMatches = false, want true (only a genuinely absent version differs)")
	}
}

// TestCopySecretFull_DestVersionCountMeasuredFromDestination is B6 item (i)'s
// proof for the full-copy path: DestVersionCount in state must be measured
// from an actual destination metadata read, not assumed equal to the source
// count -- destination-side max_versions retention can prune below that
// assumption. It also covers item (ii): a warning naming the secret and both
// counts is emitted only when truncation actually occurred, and item (iii)'s
// failure-graceful-degradation requirement: a destination metadata read
// failure must not abort the migration, only fall back to the assumed count.
func TestCopySecretFull_DestVersionCountMeasuredFromDestination(t *testing.T) {
	tests := []struct {
		name             string
		dstMountMax      int  // 0 = unset (default retention, no pruning at 5 versions)
		forceReadFailure bool // force the destination metadata re-read to fail
		wantDestCount    int  // DestVersionCount expected in state
		wantWarn         bool // a truncation warning must be emitted
	}{
		{
			name:          "destination truncates history -> measured count and warning",
			dstMountMax:   3,
			wantDestCount: 3,
			wantWarn:      true,
		},
		{
			name:          "destination retains everything -> full count, no warning",
			dstMountMax:   0,
			wantDestCount: 5,
			wantWarn:      false,
		},
		{
			name:             "destination metadata read fails -> falls back to assumed, migration still succeeds",
			dstMountMax:      3,
			forceReadFailure: true,
			wantDestCount:    5, // assumed (source count), since measurement failed
			wantWarn:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)
			if tt.dstMountMax > 0 {
				dst.setMountMaxVersions(tt.dstMountMax)
			}

			for i := 1; i <= 5; i++ {
				src.putVersion("app/secret", map[string]any{"n": i})
			}

			m, logBuf := newTestMigratorWithLogBuf(t, src, dst, true)

			if tt.forceReadFailure {
				dst.setForceMetadataReadError("app/secret", 0)
			}

			srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
			if err != nil {
				t.Fatalf("read src metadata failed: %v", err)
			}

			if err := m.copySecretFull(context.Background(), "app/secret", "app/secret", srcMeta, Options{
				Placeholder: map[string]any{"_vault_migrate": "placeholder"},
			}); err != nil {
				t.Fatalf("copySecretFull failed (bookkeeping must never abort migration): %v", err)
			}

			sec := m.State.GetSecret("app/secret")
			if sec == nil {
				t.Fatalf("expected state entry for app/secret")
			}
			if sec.DestVersionCount != tt.wantDestCount {
				t.Fatalf("DestVersionCount = %d, want %d", sec.DestVersionCount, tt.wantDestCount)
			}

			gotWarn := strings.Contains(logBuf.String(), "destination retention truncated")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warning emitted = %v, want %v (log: %s)", gotWarn, tt.wantWarn, logBuf.String())
			}
			if tt.wantWarn {
				log := logBuf.String()
				if !strings.Contains(log, "app/secret") {
					t.Fatalf("warning does not name the secret key: %s", log)
				}
				if !strings.Contains(log, "source_versions=5") {
					t.Fatalf("warning does not name source version count: %s", log)
				}
				if !strings.Contains(log, "dest_versions=3") {
					t.Fatalf("warning does not name dest version count: %s", log)
				}
			}
		})
	}
}

// TestCopyIncrementalVersions_DestVersionCountMeasuredFromDestination mirrors
// TestCopySecretFull_DestVersionCountMeasuredFromDestination for the
// incremental-copy path (copyIncrementalVersions), which has its own
// separate (and separately buggy, pre-fix) DestVersionCount assignment.
func TestCopyIncrementalVersions_DestVersionCountMeasuredFromDestination(t *testing.T) {
	tests := []struct {
		name             string
		dstMountMax      int
		forceReadFailure bool
		wantDestCount    int
		wantWarn         bool
	}{
		{
			name:          "destination truncates history -> measured count and warning",
			dstMountMax:   3,
			wantDestCount: 3,
			wantWarn:      true,
		},
		{
			name:          "destination retains everything -> full count, no warning",
			dstMountMax:   0,
			wantDestCount: 5,
			wantWarn:      false,
		},
		{
			name:             "destination metadata read fails -> falls back to assumed, migration still succeeds",
			dstMountMax:      3,
			forceReadFailure: true,
			wantDestCount:    5,
			wantWarn:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)
			if tt.dstMountMax > 0 {
				dst.setMountMaxVersions(tt.dstMountMax)
			}

			for i := 1; i <= 5; i++ {
				src.putVersion("app/secret", map[string]any{"n": i})
			}

			m, logBuf := newTestMigratorWithLogBuf(t, src, dst, true)

			if tt.forceReadFailure {
				dst.setForceMetadataReadError("app/secret", 0)
			}

			srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
			if err != nil {
				t.Fatalf("read src metadata failed: %v", err)
			}

			if err := m.copyIncrementalVersions(context.Background(), "app/secret", "app/secret", srcMeta, 1, 5, Options{
				Placeholder: map[string]any{"_vault_migrate": "placeholder"},
			}); err != nil {
				t.Fatalf("copyIncrementalVersions failed (bookkeeping must never abort migration): %v", err)
			}

			sec := m.State.GetSecret("app/secret")
			if sec == nil {
				t.Fatalf("expected state entry for app/secret")
			}
			if sec.DestVersionCount != tt.wantDestCount {
				t.Fatalf("DestVersionCount = %d, want %d", sec.DestVersionCount, tt.wantDestCount)
			}

			gotWarn := strings.Contains(logBuf.String(), "destination retention truncated")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warning emitted = %v, want %v (log: %s)", gotWarn, tt.wantWarn, logBuf.String())
			}
			if tt.wantWarn {
				log := logBuf.String()
				if !strings.Contains(log, "app/secret") {
					t.Fatalf("warning does not name the secret key: %s", log)
				}
				if !strings.Contains(log, "source_versions=5") {
					t.Fatalf("warning does not name source version count: %s", log)
				}
				if !strings.Contains(log, "dest_versions=3") {
					t.Fatalf("warning does not name dest version count: %s", log)
				}
			}
		})
	}
}

// TestCopySecretFull_CASRequiredDestination locks B19's fix. Real Vault's
// KV v2 plugin requires check-and-set on every data write whenever the
// destination secret's own per-secret cas_required OR the destination
// mount's cas_required tunable is true
// (vault-plugin-secrets-kv@v0.26.2 path_data.go:278-288). kv2WriteData now
// reactively retries with options.cas=<destination CurrentVersion> after
// the first no-cas write 400s, so migrating INTO a cas_required destination
// succeeds instead of failing on version 1. Covers both triggers: the
// per-secret cas_required (via setMetadata) and the mount-level tunable
// (via setMountCASRequired).
func TestCopySecretFull_CASRequiredDestination(t *testing.T) {
	tests := []struct {
		name  string
		setup func(dst *fakeVault)
	}{
		{
			name: "per-secret cas_required on destination",
			setup: func(dst *fakeVault) {
				dst.setMetadata("app/secret", true, 0, "", nil)
			},
		},
		{
			name: "mount-level cas_required on destination",
			setup: func(dst *fakeVault) {
				dst.setMountCASRequired(true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeVault(t)
			dst := newFakeVault(t)

			src.putVersion("app/secret", map[string]any{"value": "v1"})
			src.putVersion("app/secret", map[string]any{"value": "v2"})
			src.putVersion("app/secret", map[string]any{"value": "v3"})
			tt.setup(dst)

			m := newTestMigrator(t, src, dst, true)

			srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
			if err != nil {
				t.Fatalf("read src metadata failed: %v", err)
			}

			err = m.copySecretFull(context.Background(), "app/secret", "app/secret", srcMeta, Options{
				Placeholder: map[string]any{"_vault_migrate": "placeholder"},
			})
			if err != nil {
				t.Fatalf("copySecretFull against a cas_required destination = %v, want nil (B19 fixed)", err)
			}

			for v := 1; v <= 3; v++ {
				srcPayload, err := m.kv2ReadVersion(context.Background(), m.Src, "app/secret", v)
				if err != nil {
					t.Fatalf("read src version %d: %v", v, err)
				}
				dstPayload, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", v)
				if err != nil {
					t.Fatalf("read dst version %d: %v", v, err)
				}
				if !reflect.DeepEqual(srcPayload, dstPayload) {
					t.Fatalf("version %d payload mismatch: src=%v dst=%v", v, srcPayload, dstPayload)
				}
			}

			sec := m.State.GetSecret("app/secret")
			if sec == nil {
				t.Fatalf("expected a completed state entry, got none")
			}
			if sec.Status != "completed" {
				t.Fatalf("state status = %q, want completed", sec.Status)
			}
		})
	}
}

// TestKV2WriteData_CASRequiredRetriesWithCurrentVersion is Task 4's core
// fix-proving test: kv2WriteData's single retry must seed options.cas from
// the destination's CurrentVersion, not a fabricated/derived value.
func TestKV2WriteData_CASRequiredRetriesWithCurrentVersion(t *testing.T) {
	t.Run("fresh secret seeds cas=0", func(t *testing.T) {
		dst := newFakeVault(t)
		dst.setMountCASRequired(true)
		m := newTestMigrator(t, dst, dst, false)

		err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v1"})
		if err != nil {
			t.Fatalf("kv2WriteData failed: %v", err)
		}

		writes := dst.dataWrites()
		if len(writes) != 2 {
			t.Fatalf("expected exactly 2 write attempts (rejected + single retry), got %d", len(writes))
		}
		if _, hasOpts := writes[0]["options"]; hasOpts {
			t.Fatalf("first write should carry no options key, got %v", writes[0])
		}
		opts, ok := writes[1]["options"].(map[string]any)
		if !ok {
			t.Fatalf("retry write missing options: %v", writes[1])
		}
		if asInt(opts["cas"]) != 0 {
			t.Fatalf("cas = %v, want 0 for a brand-new destination secret", opts["cas"])
		}
	})

	t.Run("existing secret seeds cas=CurrentVersion not +1", func(t *testing.T) {
		dst := newFakeVault(t)
		dst.putVersion("app/secret", map[string]any{"value": "v1"})
		dst.putVersion("app/secret", map[string]any{"value": "v2"})
		dst.putVersion("app/secret", map[string]any{"value": "v3"})
		dst.setMountCASRequired(true)
		m := newTestMigrator(t, dst, dst, false)

		err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v4"})
		if err != nil {
			t.Fatalf("kv2WriteData failed: %v", err)
		}

		writes := dst.dataWrites()
		last := writes[len(writes)-1]
		opts, ok := last["options"].(map[string]any)
		if !ok {
			t.Fatalf("retry write missing options: %v", last)
		}
		if asInt(opts["cas"]) != 3 {
			t.Fatalf("cas = %v, want 3 (CurrentVersion); the plugin itself advances to 4 on success -- sending 4 would be rejected as a mismatch", opts["cas"])
		}
	})
}

// TestKV2WriteData_CASRequiredAllVersionsDestroyed locks the case the design
// explicitly calls out: destroy never touches CurrentVersion
// (path_destroy.go:82), so a destination whose every version has been
// destroyed still has a non-zero CurrentVersion, and the retry must use it
// -- NOT fall back to 0 as a naive "secret looks empty" heuristic would.
func TestKV2WriteData_CASRequiredAllVersionsDestroyed(t *testing.T) {
	dst := newFakeVault(t)
	dst.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.putVersion("app/secret", map[string]any{"value": "v2"})
	dst.putVersion("app/secret", map[string]any{"value": "v3"})
	dst.markVersionDestroyed("app/secret", 1)
	dst.markVersionDestroyed("app/secret", 2)
	dst.markVersionDestroyed("app/secret", 3)
	dst.setMountCASRequired(true)

	m := newTestMigrator(t, dst, dst, false)
	err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v4"})
	if err != nil {
		t.Fatalf("kv2WriteData failed: %v", err)
	}

	writes := dst.dataWrites()
	last := writes[len(writes)-1]
	opts, ok := last["options"].(map[string]any)
	if !ok {
		t.Fatalf("retry write missing options: %v", last)
	}
	if asInt(opts["cas"]) != 3 {
		t.Fatalf("cas = %v, want 3 (CurrentVersion unaffected by destroy), NOT 0", opts["cas"])
	}
}

// TestKV2WriteData_CASMismatchIsNotRetried locks that a genuine concurrent
// writer -- one that lands between B19's seed metadata read and its single
// retry write -- produces a propagated mismatch error, NOT a second retry.
// Exactly 2 write attempts total (the original rejected write + the one
// retry); a loop would keep re-reading and re-writing indefinitely.
func TestKV2WriteData_CASMismatchIsNotRetried(t *testing.T) {
	dst := newFakeVault(t)
	dst.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.setMountCASRequired(true)
	dst.setAfterMetadataGET("app/secret", func() {
		// A different writer advances CurrentVersion right after our seed
		// read observes it, so the cas we send next is already stale.
		dst.bumpCurrentVersionLocked("app/secret", map[string]any{"value": "concurrent"})
	})

	m := newTestMigrator(t, dst, dst, false)
	err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v2"})
	if err == nil {
		t.Fatalf("expected CAS mismatch error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "did not match the current version") {
		t.Fatalf("error = %q, want the mismatch message propagated as-is", err.Error())
	}

	writes := dst.dataWrites()
	if len(writes) != 2 {
		t.Fatalf("expected exactly 2 write attempts (no loop on a genuine mismatch), got %d", len(writes))
	}
}

// TestKV2WriteData_CASSeedMetadataReadFailure locks that a failed seed
// metadata read (network, 5xx, perms -- NOT "not found") never fabricates a
// cas value. The ORIGINAL CAS-required write error must propagate, with the
// read failure attached as context, and no retry write may be attempted.
func TestKV2WriteData_CASSeedMetadataReadFailure(t *testing.T) {
	dst := newFakeVault(t)
	dst.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.setMountCASRequired(true)
	dst.setForceMetadataReadError("app/secret", http.StatusInternalServerError)

	m := newTestMigrator(t, dst, dst, false)
	err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v2"})
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "check-and-set parameter required") {
		t.Fatalf("error = %q, want the original CAS-required error preserved", err.Error())
	}
	if !strings.Contains(err.Error(), "injected metadata read failure") {
		t.Fatalf("error = %q, want the seed metadata read failure attached as context", err.Error())
	}

	writes := dst.dataWrites()
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 write attempt (no retry when the cas seed read fails), got %d", len(writes))
	}
	if _, hasOpts := writes[0]["options"]; hasOpts {
		t.Fatalf("no retry should have been attempted: %v", writes[0])
	}
}

// TestCopyIncrementalVersions_CASRequiredDestination locks B19's fix through
// the incremental copy path (not just copySecretFull).
func TestCopyIncrementalVersions_CASRequiredDestination(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "v1"})
	src.putVersion("app/secret", map[string]any{"value": "v2"})
	src.putVersion("app/secret", map[string]any{"value": "v3"})
	dst.putVersion("app/secret", map[string]any{"value": "v1"})
	dst.setMountCASRequired(true)

	m := newTestMigrator(t, src, dst, true)

	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("read src metadata failed: %v", err)
	}

	err = m.copyIncrementalVersions(context.Background(), "app/secret", "app/secret", srcMeta, 2, 3, Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyIncrementalVersions against a cas_required destination = %v, want nil", err)
	}

	for v := 2; v <= 3; v++ {
		got, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", v)
		if err != nil {
			t.Fatalf("read dst v%d: %v", v, err)
		}
		want, err := m.kv2ReadVersion(context.Background(), m.Src, "app/secret", v)
		if err != nil {
			t.Fatalf("read src v%d: %v", v, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("v%d mismatch: got=%v want=%v", v, got, want)
		}
	}

	sec := m.State.GetSecret("app/secret")
	if sec == nil || sec.Status != "completed" {
		t.Fatalf("expected completed state, got %+v", sec)
	}
}

// TestCopyOneSecret_CASRequiredDestination locks B19's fix through the
// no-state copy path (copyOneSecret), which never performs its own
// destination metadata read outside of kv2WriteData's internal retry.
func TestCopyOneSecret_CASRequiredDestination(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "v1"})
	src.putVersion("app/secret", map[string]any{"value": "v2"})
	dst.setMountCASRequired(true)

	m := newTestMigrator(t, src, dst, false)

	err := m.copyOneSecret(context.Background(), "app/secret", "app/secret", Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyOneSecret against a cas_required destination = %v, want nil", err)
	}

	for v := 1; v <= 2; v++ {
		got, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", v)
		if err != nil {
			t.Fatalf("read dst v%d: %v", v, err)
		}
		if got["value"] != fmt.Sprintf("v%d", v) {
			t.Fatalf("dst v%d value = %v, want v%d", v, got["value"], v)
		}
	}
}

// TestCopyOneSecretWithState_CASRequiredAfterDeleteRecopy locks the
// stale-cas-after-delete trap: copyOneSecretWithState detects a destination
// mismatch, calls kv2DeleteSecret (destination secret now genuinely gone),
// then calls copySecretFull on a cas_required MOUNT. The very first write
// after the delete must seed cas=0 from a fresh metadata read -- any cached
// pre-delete CurrentVersion would be stale and get rejected as a mismatch.
func TestCopyOneSecretWithState_CASRequiredAfterDeleteRecopy(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "source"})
	dst.putVersion("app/secret", map[string]any{"value": "destination"}) // forces hash mismatch
	dst.setMountCASRequired(true)

	m := newTestMigrator(t, src, dst, true)

	err := m.copyOneSecretWithState(context.Background(), "app/secret", "app/secret", Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("copyOneSecretWithState delete+recopy on a cas_required mount = %v, want nil", err)
	}

	if dst.deleteCalls() != 1 {
		t.Fatalf("expected exactly 1 kv2DeleteSecret call, got %d", dst.deleteCalls())
	}

	v1, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", 1)
	if err != nil {
		t.Fatalf("read recreated v1: %v", err)
	}
	if v1["value"] != "source" {
		t.Fatalf("recreated v1 value = %v, want source", v1["value"])
	}
}

// TestKV2WriteData_NoCASSentWhenNotRequired is Task 5's highest-value test:
// on a destination that never requires check-and-set, NOT ONE write may
// carry an "options" key. This is the overwhelming common case in real
// runs and must be byte-identical to the pre-B19 wire format.
func TestKV2WriteData_NoCASSentWhenNotRequired(t *testing.T) {
	dst := newFakeVault(t)
	m := newTestMigrator(t, dst, dst, false)

	for i := 0; i < 3; i++ {
		if err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": i}); err != nil {
			t.Fatalf("kv2WriteData failed: %v", err)
		}
	}

	for i, body := range dst.dataWrites() {
		if _, hasOpts := body["options"]; hasOpts {
			t.Fatalf("write %d carries an options key on a non-cas_required destination: %v", i, body)
		}
	}
}

// TestKV2WriteData_NoExtraRequestsWhenNotRequired locks that the common
// path issues exactly one /data/ write per version and ZERO
// CAS-attributable metadata GETs -- the retry path in kv2WriteData must
// never fire when the initial write already succeeds.
func TestKV2WriteData_NoExtraRequestsWhenNotRequired(t *testing.T) {
	dst := newFakeVault(t)
	m := newTestMigrator(t, dst, dst, false)

	before := dst.metadataGETs()
	const n = 5
	for i := 0; i < n; i++ {
		if err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": i}); err != nil {
			t.Fatalf("kv2WriteData failed: %v", err)
		}
	}

	if got := len(dst.dataWrites()); got != n {
		t.Fatalf("/data/ write count = %d, want exactly %d", got, n)
	}
	if got := dst.metadataGETs() - before; got != 0 {
		t.Fatalf("metadata GET count = %d, want 0 (no CAS retry should fire)", got)
	}
}

// TestKV2WriteData_400NonCASNotRetried is a B17 regression lock: a 400 with
// an unrelated message (e.g. a KV v1 mount's "unsupported path") must
// propagate untouched, never triggering the CAS retry.
func TestKV2WriteData_400NonCASNotRetried(t *testing.T) {
	dst := newFakeVault(t)
	dst.setForceDataWriteError("app/secret", http.StatusBadRequest, "unsupported path")

	m := newTestMigrator(t, dst, dst, false)
	err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v1"})
	if err == nil {
		t.Fatalf("expected the unrelated 400 to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported path") {
		t.Fatalf("error = %q, want the original unrelated 400 message preserved", err.Error())
	}
	if got := len(dst.dataWrites()); got != 1 {
		t.Fatalf("write count = %d, want exactly 1 (no retry on an unrelated 400)", got)
	}
	if got := dst.metadataGETs(); got != 0 {
		t.Fatalf("metadata GET count = %d, want 0 (no CAS retry should fire)", got)
	}
}

// TestKV2WriteData_Non400ErrorNotRetried locks that 403 and 500 responses
// propagate immediately on the first attempt -- no retry, no metadata read.
// Client-level transport retries (B14, retryablehttp's own 5xx backoff) are
// a separate concern from this app-level CAS retry; disable them here via
// SetMaxRetries(0) so the assertion isolates kv2WriteData's own behavior.
func TestKV2WriteData_Non400ErrorNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			dst := newFakeVault(t)
			dst.setForceDataWriteError("app/secret", status, "")

			m := newTestMigrator(t, dst, dst, false)
			m.Dst.Client.SetMaxRetries(0)
			err := m.kv2WriteData(context.Background(), m.Dst, "app/secret", map[string]any{"value": "v1"})
			if err == nil {
				t.Fatalf("expected a %d error to propagate, got nil", status)
			}
			if got := len(dst.dataWrites()); got != 1 {
				t.Fatalf("write count = %d, want exactly 1 (no retry on a %d)", got, status)
			}
			if got := dst.metadataGETs(); got != 0 {
				t.Fatalf("metadata GET count = %d, want 0 (no CAS retry should fire on a %d)", got, status)
			}
		})
	}
}

// TestCopySecretFull_SourceCASRequiredSurvivesSecondRun is Task 6's test for
// a finding NOT in TODO.md's original B19 entry: kv2WriteMetadataSettings
// (kvv2.go:839-856) sends cas_required from SOURCE metadata unconditionally,
// AFTER the version-write loop. A source secret with cas_required=true
// therefore self-inflicts B19 on the destination: run 1 succeeds (dest had
// no cas_required yet) then stamps cas_required=true onto the dest as its
// very last step; run 2's incremental write would 400 without B19's fix.
// Needs ZERO destination-side operator action.
func TestCopySecretFull_SourceCASRequiredSurvivesSecondRun(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	src.putVersion("app/secret", map[string]any{"value": "v1"})
	src.setMetadata("app/secret", true, 0, "", nil) // source cas_required=true

	m := newTestMigrator(t, src, dst, true)

	srcMeta, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("read src metadata failed: %v", err)
	}
	if err := m.copySecretFull(context.Background(), "app/secret", "app/secret", srcMeta, Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	}); err != nil {
		t.Fatalf("run 1 (copySecretFull) failed: %v", err)
	}

	// Destination now has cas_required=true stamped on it by run 1's own
	// kv2WriteMetadataSettings call, with zero destination-side operator
	// action. Add a source version and run an incremental copy -- this is
	// self-inflicted B19.
	src.putVersion("app/secret", map[string]any{"value": "v2"})

	srcMeta2, err := m.kv2ReadMetadata(context.Background(), m.Src, "app/secret")
	if err != nil {
		t.Fatalf("read src metadata (run 2) failed: %v", err)
	}
	err = m.copyIncrementalVersions(context.Background(), "app/secret", "app/secret", srcMeta2, 2, 2, Options{
		Placeholder: map[string]any{"_vault_migrate": "placeholder"},
	})
	if err != nil {
		t.Fatalf("run 2 (copyIncrementalVersions) failed: %v -- source cas_required must not survive as a self-inflicted destination failure", err)
	}

	v2, err := m.kv2ReadVersion(context.Background(), m.Dst, "app/secret", 2)
	if err != nil {
		t.Fatalf("read dst v2: %v", err)
	}
	if v2["value"] != "v2" {
		t.Fatalf("dst v2 value = %v, want v2", v2["value"])
	}
}
