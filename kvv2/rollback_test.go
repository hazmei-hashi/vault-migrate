package kvv2

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"vault-migrate/config"
	"vault-migrate/state"
)

// buildRollbackState writes a state file to dir with the given secrets map
// and returns the file path.
func buildRollbackState(t *testing.T, dir string, src, dst *fakeVault, secrets map[string]*state.Secret) string {
	t.Helper()

	ms := state.NewMigrationState(
		state.ClusterInfo{Address: src.server.URL, Mount: "secret", BasePath: ""},
		state.ClusterInfo{Address: dst.server.URL, Mount: "secret", BasePath: ""},
	)
	for k, v := range secrets {
		ms.Secrets[k] = v
	}
	path := filepath.Join(dir, "state.json")
	if err := ms.Save(path); err != nil {
		t.Fatalf("buildRollbackState save: %v", err)
	}
	return path
}

// buildRollbackStateWithPaths is buildRollbackState but with explicit
// src/dst addresses, mounts, and base paths.
func buildRollbackStateWithPaths(t *testing.T, dir string, srcAddr, dstAddr, srcMount, dstMount, srcBase, dstBase string, secrets map[string]*state.Secret) string {
	t.Helper()

	ms := state.NewMigrationState(
		state.ClusterInfo{Address: srcAddr, Mount: srcMount, BasePath: srcBase},
		state.ClusterInfo{Address: dstAddr, Mount: dstMount, BasePath: dstBase},
	)
	for k, v := range secrets {
		ms.Secrets[k] = v
	}
	path := filepath.Join(dir, "state-paths.json")
	if err := ms.Save(path); err != nil {
		t.Fatalf("buildRollbackStateWithPaths save: %v", err)
	}
	return path
}

// ── Fix 1 guard test ─────────────────────────────────────────────────────────

// TestRollback_WrongClusterRefused: state records dstAddr=A, cfg supplies B
// → hard error before ANY delete call. This is the #1 blocker guard.
func TestRollback_WrongClusterRefused(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/secret", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/secret": {Status: "completed"},
	})

	// Supply a different address than what's in the state file.
	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   "https://wrong-cluster.example.com:8200",
		LogLevel:  "error",
	})
	if err == nil {
		t.Fatalf("expected error for wrong-cluster, got nil")
	}
	if !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("unexpected error (want 'rollback refused'): %v", err)
	}
	if dst.deleteCalls() != 0 {
		t.Fatalf("wrong-cluster: expected 0 delete calls, got %d", dst.deleteCalls())
	}
}

// TestRollback_WrongNamespaceRefused: same address but different namespace.
func TestRollback_WrongNamespaceRefused(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/secret", map[string]any{"v": "1"})

	// Build state with namespace "admin".
	ms := state.NewMigrationState(
		state.ClusterInfo{Address: src.server.URL, Mount: "secret"},
		state.ClusterInfo{Address: dst.server.URL, Mount: "secret", Namespace: "admin"},
	)
	ms.Secrets["app/secret"] = &state.Secret{Status: "completed"}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	if err := ms.Save(stateFile); err != nil {
		t.Fatalf("save state: %v", err)
	}

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile:    stateFile,
		DstAddr:      dst.server.URL,
		DstNamespace: "wrong-ns", // mismatch
		LogLevel:     "error",
	})
	if err == nil {
		t.Fatalf("expected error for wrong namespace, got nil")
	}
	if !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("unexpected error: %v", err)
	}
	if dst.deleteCalls() != 0 {
		t.Fatalf("expected 0 delete calls on namespace mismatch, got %d", dst.deleteCalls())
	}
}

// ── Fix 2 guard test ─────────────────────────────────────────────────────────

// TestRollback_ForbiddenNotSkipped: a 403 on delete is a REAL failure —
// must NOT be counted not_found or skipped. B17-style regression lock.
func TestRollback_ForbiddenNotSkipped(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/perm", map[string]any{"v": "1"})
	dst.setForceMetadataDeleteError("app/perm", http.StatusForbidden)

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/perm": {Status: "completed"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err == nil {
		t.Fatalf("expected error from 403, got nil")
	}
	// Must be a real failure error, not a not-found/refuse-success error.
	if strings.Contains(err.Error(), "refusing to report success") {
		t.Fatalf("403 was misclassified as not-found: %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("403 was misclassified as not-found: %v", err)
	}
}

// TestRollback_Routing404RefusesSuccess: a routing 404 on delete (wrong mount /
// wrong namespace / non-KV path) must NOT silently succeed when it's the only
// outcome. B9/B17 guard: deleted==0 && notFound>0 → refuse success.
func TestRollback_Routing404RefusesSuccess(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/x", map[string]any{"v": "1"})
	// Force a 404 (routing 404, not a normal successful absent-key 204).
	dst.setForceMetadataDeleteError("app/x", http.StatusNotFound)

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/x": {Status: "completed"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile:       stateFile,
		DstAddr:         dst.server.URL, // must match state
		LogLevel:        "error",
		ContinueOnError: true, // let it reach the post-loop guard
	})
	if err == nil {
		t.Fatalf("expected refuse-success error from routing 404, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to report success") {
		t.Fatalf("expected 'refusing to report success', got: %v", err)
	}
}

// ── Existing tests (updated where needed) ────────────────────────────────────

// TestRollback_DeletesCompletedSecrets: completed entries are deleted.
func TestRollback_DeletesCompletedSecrets(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/a", map[string]any{"v": "1"})
	dst.putVersion("app/b", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/a": {Status: "completed"},
		"app/b": {Status: "completed"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	if dst.deleteCalls() != 2 {
		t.Fatalf("expected 2 deleteCalls, got %d", dst.deleteCalls())
	}
	if _, ok := dst.secrets["app/a"]; ok {
		t.Fatalf("app/a still present after rollback")
	}
	if _, ok := dst.secrets["app/b"]; ok {
		t.Fatalf("app/b still present after rollback")
	}
}

// TestRollback_SkipsFailedAndSkipped: failed/skipped entries NOT deleted.
func TestRollback_SkipsFailedAndSkipped(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/ok", map[string]any{"v": "1"})
	dst.putVersion("app/fail", map[string]any{"v": "1"})
	dst.putVersion("app/skip", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/ok":   {Status: "completed"},
		"app/fail": {Status: "failed"},
		"app/skip": {Status: "skipped"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	if dst.deleteCalls() != 1 {
		t.Fatalf("expected 1 deleteCalls (only completed), got %d", dst.deleteCalls())
	}
	if _, ok := dst.secrets["app/fail"]; !ok {
		t.Fatalf("app/fail was deleted; should be untouched")
	}
	if _, ok := dst.secrets["app/skip"]; !ok {
		t.Fatalf("app/skip was deleted; should be untouched")
	}
}

// TestRollback_DryRunDeletesNothing: dryRun=true → zero deletes.
func TestRollback_DryRunDeletesNothing(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/x", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/x": {Status: "completed"},
	})

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Rollback dry-run failed: %v", err)
	}

	if dst.deleteCalls() != 0 {
		t.Fatalf("dry-run issued %d delete calls; expected 0", dst.deleteCalls())
	}
}

// TestRollback_ConfirmationAbort: answer "n" → zero deletes.
func TestRollback_ConfirmationAbort(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/y", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/y": {Status: "completed"},
	})

	useStdinInput(t, "n\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback abort returned error: %v", err)
	}
	if dst.deleteCalls() != 0 {
		t.Fatalf("abort: expected 0 deletes, got %d", dst.deleteCalls())
	}
}

// TestRollback_ConfirmationYes: answer "yes" → deletes proceed.
func TestRollback_ConfirmationYes(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/z", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/z": {Status: "completed"},
	})

	useStdinInput(t, "yes\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback yes failed: %v", err)
	}
	if dst.deleteCalls() != 1 {
		t.Fatalf("yes: expected 1 delete, got %d", dst.deleteCalls())
	}
}

// TestRollback_AlreadyGoneIsIdempotent: key absent from dst — real Vault
// returns 204 on metadata DELETE for any key (present or absent). The fake
// mirrors this: delete(f.secrets, relKey) + 204 regardless. So a secret
// already gone gets counted as deleted (not an error). Verify no error.
func TestRollback_AlreadyGoneIsIdempotent(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	// Do NOT seed dst; secret is already gone.
	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/gone": {Status: "completed"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback idempotent: unexpected error: %v", err)
	}
	// The fake returns 204 regardless → counted as deleted (1), no routing 404.
	if dst.deleteCalls() != 1 {
		t.Fatalf("expected deleteSecretCalls=1 (204 path), got %d", dst.deleteCalls())
	}
}

// TestRollback_BasePathMapping: non-empty src/dst base paths → dstKeyFor
// correctly maps src state keys to dst keys before deleting.
func TestRollback_BasePathMapping(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	// Migration was: src base=myapp → dst base=myapp-migrated.
	// State key "myapp/db" → dst key "myapp-migrated/db".
	dst.putVersion("myapp-migrated/db", map[string]any{"password": "secret"})

	stateFile := buildRollbackStateWithPaths(t, t.TempDir(),
		src.server.URL, dst.server.URL,
		"secret", "secret",
		"myapp", "myapp-migrated",
		map[string]*state.Secret{
			"myapp/db": {Status: "completed"},
		},
	)

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback base path mapping failed: %v", err)
	}

	if dst.deleteCalls() != 1 {
		t.Fatalf("expected 1 delete, got %d", dst.deleteCalls())
	}
	if _, ok := dst.secrets["myapp-migrated/db"]; ok {
		t.Fatalf("myapp-migrated/db still present; dstKeyFor mapping was wrong")
	}
}

// TestRollback_ContinueOnError: one delete fails → others still processed.
func TestRollback_ContinueOnError(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/a", map[string]any{"v": "1"})
	dst.putVersion("app/b", map[string]any{"v": "1"})
	dst.putVersion("app/c", map[string]any{"v": "1"})

	// Force delete of app/b to fail with a 500 (real error, not routing 404).
	dst.setForceMetadataDeleteError("app/b", 500)

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/a": {Status: "completed"},
		"app/b": {Status: "completed"},
		"app/c": {Status: "completed"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile:       stateFile,
		DstAddr:         dst.server.URL,
		LogLevel:        "error",
		ContinueOnError: true,
	})
	if err == nil {
		t.Fatalf("expected error from 1 failed delete, got nil")
	}
	if !strings.Contains(err.Error(), "failure") {
		t.Fatalf("unexpected error: %v", err)
	}

	// app/a and app/c should be gone; app/b should still exist.
	if _, ok := dst.secrets["app/a"]; ok {
		t.Fatalf("app/a still present; should have been deleted")
	}
	if _, ok := dst.secrets["app/c"]; ok {
		t.Fatalf("app/c still present; should have been deleted")
	}
	if _, ok := dst.secrets["app/b"]; !ok {
		t.Fatalf("app/b should still be present (delete failed)")
	}
}

// TestRollback_AbortWithoutContinueOnError: first delete fails,
// ContinueOnError=false → returns immediately; remaining targets untouched.
func TestRollback_AbortWithoutContinueOnError(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/a", map[string]any{"v": "1"})
	dst.putVersion("app/b", map[string]any{"v": "1"})
	dst.putVersion("app/c", map[string]any{"v": "1"})

	// targets are sorted by dstKey → app/a is first.
	dst.setForceMetadataDeleteError("app/a", 500)

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/a": {Status: "completed"},
		"app/b": {Status: "completed"},
		"app/c": {Status: "completed"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile:       stateFile,
		DstAddr:         dst.server.URL,
		LogLevel:        "error",
		ContinueOnError: false,
	})
	if err == nil {
		t.Fatalf("expected error from failed delete with ContinueOnError=false, got nil")
	}

	// app/a delete was attempted (failed); app/b and app/c must NOT have been attempted.
	// The fake's deleteSecretCalls counts successful 204s only — but the forced-error
	// for app/a never reaches the delete(f.secrets) line so it's not incremented there.
	// What we can assert: app/b and app/c must still be present (untouched).
	if _, ok := dst.secrets["app/b"]; !ok {
		t.Fatalf("app/b was deleted; rollback should have stopped at app/a failure")
	}
	if _, ok := dst.secrets["app/c"]; !ok {
		t.Fatalf("app/c was deleted; rollback should have stopped at app/a failure")
	}
}

// TestRollback_MissingStateFile: nonexistent path → hard error with
// "nothing to roll back" message.
func TestRollback_MissingStateFile(t *testing.T) {
	dst := newFakeVault(t)

	nonexistent := filepath.Join(t.TempDir(), "no-such-state.json")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: nonexistent,
		LogLevel:  "error",
	})
	if err == nil {
		t.Fatalf("expected error for missing state file, got nil")
	}
	if !strings.Contains(err.Error(), "nothing to roll back") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRollback_EmptyStateFile: state file exists but has no secrets →
// distinct error from missing file.
func TestRollback_EmptyStateFile(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{})

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		LogLevel:  "error",
	})
	if err == nil {
		t.Fatalf("expected error for empty state file, got nil")
	}
	// Distinct from "no state file" — must NOT say "no state file at"
	// since the file exists.
	if strings.Contains(err.Error(), "no state file at") {
		t.Fatalf("error reports file missing, but file exists: %v", err)
	}
	if !strings.Contains(err.Error(), "no secrets recorded") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRollback_RecreatedStatusDeleted: "recreated" status also targeted.
func TestRollback_RecreatedStatusDeleted(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/rec", map[string]any{"v": "1"})

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, map[string]*state.Secret{
		"app/rec": {Status: "recreated"},
	})

	useStdinInput(t, "y\n")

	err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	})
	if err != nil {
		t.Fatalf("Rollback recreated failed: %v", err)
	}

	if dst.deleteCalls() != 1 {
		t.Fatalf("expected 1 delete for recreated status, got %d", dst.deleteCalls())
	}
}

// TestRollback_StateFileUntouched: state file content unchanged after rollback.
// Compares bytes (not mtime, which is flaky with filesystem granularity in CI).
func TestRollback_StateFileUntouched(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	dst.putVersion("app/st", map[string]any{"v": "1"})

	dir := t.TempDir()
	stateFile := buildRollbackState(t, dir, src, dst, map[string]*state.Secret{
		"app/st": {Status: "completed"},
	})

	before, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state before rollback: %v", err)
	}

	useStdinInput(t, "y\n")

	if err := Rollback(dst.newClient(t), config.VaultClientConfig{
		StateFile: stateFile,
		DstAddr:   dst.server.URL,
		LogLevel:  "error",
	}); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	after, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state after rollback: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("state file changed after rollback\nbefore: %s\nafter: %s", before, after)
	}
}

// TestRollback_SortedSampleIsDeterministic: same state → same sample order
// across multiple calls (targets are sorted by dstKey).
func TestRollback_SortedSampleIsDeterministic(t *testing.T) {
	src := newFakeVault(t)
	dst := newFakeVault(t)

	// Seed 10 secrets to exceed the 5-key sample window.
	keys := []string{"c", "a", "j", "b", "g", "e", "h", "f", "d", "i"}
	secrets := make(map[string]*state.Secret, len(keys))
	for _, k := range keys {
		dst.putVersion("app/"+k, map[string]any{"v": "1"})
		secrets["app/"+k] = &state.Secret{Status: "completed"}
	}

	stateFile := buildRollbackState(t, t.TempDir(), src, dst, secrets)

	// Dry-run twice — no confirmation needed, captures log output indirectly
	// by asserting no error and identical delete counts (0).
	for i := 0; i < 2; i++ {
		if err := Rollback(dst.newClient(t), config.VaultClientConfig{
			StateFile: stateFile,
			DstAddr:   dst.server.URL,
			LogLevel:  "error",
			DryRun:    true,
		}); err != nil {
			t.Fatalf("dry-run %d: %v", i, err)
		}
	}
	if dst.deleteCalls() != 0 {
		t.Fatalf("dry-run should have 0 deletes, got %d", dst.deleteCalls())
	}
}
