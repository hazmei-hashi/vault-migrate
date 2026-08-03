package kvv2

import (
	"context"
	"encoding/json"
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
	mu      sync.Mutex
	secrets map[string]*fakeKVSecret
	server  *httptest.Server
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

func (f *fakeVault) putVersion(key string, data map[string]any) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	sec := f.ensureSecret(key)
	sec.CurrentVersion++
	sec.Versions[sec.CurrentVersion] = &fakeKVVersion{Data: cloneMap(data)}
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
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
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
		if !ok || v.Destroyed || v.DeletionTime != "" {
			writeNotFound(w)
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

		payload, _ := body["data"].(map[string]any)
		sec := f.ensureSecret(relKey)
		sec.CurrentVersion++
		sec.Versions[sec.CurrentVersion] = &fakeKVVersion{Data: cloneMap(payload)}

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
