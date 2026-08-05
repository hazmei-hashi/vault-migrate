package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewMigrationState(t *testing.T) {
	src := ClusterInfo{
		Address:   "https://vault-src.example.com:8200",
		Namespace: "admin",
		Mount:     "secret",
		BasePath:  "myapp",
	}
	dst := ClusterInfo{
		Address:   "https://vault-dst.example.com:8200",
		Namespace: "admin",
		Mount:     "secret",
		BasePath:  "newapp",
	}

	state := NewMigrationState(src, dst)

	if state.Version != "1.0" {
		t.Errorf("NewMigrationState() Version = %q, want %q", state.Version, "1.0")
	}
	if state.MigrationID == "" {
		t.Error("NewMigrationState() MigrationID is empty")
	}
	if state.Source != src {
		t.Errorf("NewMigrationState() Source = %+v, want %+v", state.Source, src)
	}
	if state.Destination != dst {
		t.Errorf("NewMigrationState() Destination = %+v, want %+v", state.Destination, dst)
	}
	if state.Secrets == nil {
		t.Error("NewMigrationState() Secrets map is nil")
	}
	if len(state.Secrets) != 0 {
		t.Errorf("NewMigrationState() Secrets len = %d, want 0", len(state.Secrets))
	}
	if state.Summary.StartedAt == "" {
		t.Error("NewMigrationState() Summary.StartedAt is empty")
	}
	if state.Summary.LastUpdatedAt == "" {
		t.Error("NewMigrationState() Summary.LastUpdatedAt is empty")
	}
}

func TestMigrationState_Save_Load(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "state.json")

	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}

	original := NewMigrationState(src, dst)
	original.UpdateSecret("secret/db/password", &Secret{
		Status:             "completed",
		SourceVersionCount: 3,
		DestVersionCount:   3,
		VersionHashes: map[string]string{
			"1": "sha256:abc123",
			"2": "sha256:def456",
		},
		VersionStates: map[string]string{
			"1": "active",
			"2": "source_version_unavailable",
		},
		MetadataChecksum: "sha256:meta123",
		MigratedAt:       "2026-05-01T10:00:00Z",
		RetryCount:       0,
	})

	err := original.Save(path)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("Save() file permissions = %o, want 0600", info.Mode().Perm())
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.Version != original.Version {
		t.Errorf("Load() Version = %q, want %q", loaded.Version, original.Version)
	}
	if loaded.MigrationID != original.MigrationID {
		t.Errorf("Load() MigrationID = %q, want %q", loaded.MigrationID, original.MigrationID)
	}
	if loaded.Source != original.Source {
		t.Errorf("Load() Source mismatch")
	}
	if loaded.Destination != original.Destination {
		t.Errorf("Load() Destination mismatch")
	}
	if len(loaded.Secrets) != 1 {
		t.Errorf("Load() Secrets len = %d, want 1", len(loaded.Secrets))
	}

	secret := loaded.Secrets["secret/db/password"]
	if secret == nil {
		t.Fatal("Load() secret not found")
	}
	if secret.Status != "completed" {
		t.Errorf("Load() secret.Status = %q, want %q", secret.Status, "completed")
	}
	if secret.SourceVersionCount != 3 {
		t.Errorf("Load() secret.SourceVersionCount = %d, want 3", secret.SourceVersionCount)
	}
}

func TestMigrationState_Save_Atomic(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "state.json")

	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}

	// Pre-existing valid state file.
	first := NewMigrationState(src, dst)
	first.UpdateSecret("secret/old", &Secret{Status: "completed"})
	if err := first.Save(path); err != nil {
		t.Fatalf("Save() first error = %v", err)
	}

	// Save again over the existing file.
	second := NewMigrationState(src, dst)
	second.UpdateSecret("secret/new", &Secret{Status: "completed"})
	if err := second.Save(path); err != nil {
		t.Fatalf("Save() second error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("Load() returned nil")
	}
	if _, ok := loaded.Secrets["secret/new"]; !ok {
		t.Errorf("Load() Secrets missing %q, got %+v", "secret/new", loaded.Secrets)
	}
	if _, ok := loaded.Secrets["secret/old"]; ok {
		t.Errorf("Load() Secrets should not contain stale %q", "secret/old")
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("ReadDir() entries = %d, want 1 (leaked temp file?); got %v", len(entries), names)
	}
}

func TestLoad_NotExist(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "nonexistent.json")

	state, err := Load(path)
	if err != nil {
		t.Errorf("Load() nonexistent file error = %v, want nil", err)
	}
	if state != nil {
		t.Errorf("Load() nonexistent file = %+v, want nil", state)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "invalid.json")

	err := os.WriteFile(path, []byte("not valid json {{}"), 0600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	state, err := Load(path)
	if err == nil {
		t.Error("Load() invalid JSON error = nil, want error")
	}
	if state != nil {
		t.Errorf("Load() invalid JSON = %+v, want nil", state)
	}
}

func TestMigrationState_Validate(t *testing.T) {
	src := ClusterInfo{
		Address:   "https://vault-src:8200",
		Namespace: "admin",
		Mount:     "secret",
		BasePath:  "myapp",
	}
	dst := ClusterInfo{
		Address:   "https://vault-dst:8200",
		Namespace: "admin",
		Mount:     "secret",
		BasePath:  "newapp",
	}

	tests := []struct {
		name        string
		stateSrc    ClusterInfo
		stateDst    ClusterInfo
		currentSrc  ClusterInfo
		currentDst  ClusterInfo
		wantErr     bool
		errContains string
	}{
		{
			name:       "matching configs",
			stateSrc:   src,
			stateDst:   dst,
			currentSrc: src,
			currentDst: dst,
			wantErr:    false,
		},
		{
			name:        "mismatched source address",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  ClusterInfo{Address: "https://other:8200", Namespace: "admin", Mount: "secret", BasePath: "myapp"},
			currentDst:  dst,
			wantErr:     true,
			errContains: "source mismatch",
		},
		{
			name:        "mismatched source namespace",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  ClusterInfo{Address: "https://vault-src:8200", Namespace: "other", Mount: "secret", BasePath: "myapp"},
			currentDst:  dst,
			wantErr:     true,
			errContains: "source mismatch",
		},
		{
			name:        "mismatched source mount",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  ClusterInfo{Address: "https://vault-src:8200", Namespace: "admin", Mount: "kv", BasePath: "myapp"},
			currentDst:  dst,
			wantErr:     true,
			errContains: "source mismatch",
		},
		{
			name:        "mismatched source base path",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  ClusterInfo{Address: "https://vault-src:8200", Namespace: "admin", Mount: "secret", BasePath: "other"},
			currentDst:  dst,
			wantErr:     true,
			errContains: "source mismatch",
		},
		{
			name:        "mismatched destination address",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  src,
			currentDst:  ClusterInfo{Address: "https://other:8200", Namespace: "admin", Mount: "secret", BasePath: "newapp"},
			wantErr:     true,
			errContains: "destination mismatch",
		},
		{
			name:        "mismatched destination namespace",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  src,
			currentDst:  ClusterInfo{Address: "https://vault-dst:8200", Namespace: "other", Mount: "secret", BasePath: "newapp"},
			wantErr:     true,
			errContains: "destination mismatch",
		},
		{
			name:        "mismatched destination mount",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  src,
			currentDst:  ClusterInfo{Address: "https://vault-dst:8200", Namespace: "admin", Mount: "kv", BasePath: "newapp"},
			wantErr:     true,
			errContains: "destination mismatch",
		},
		{
			name:        "mismatched destination base path",
			stateSrc:    src,
			stateDst:    dst,
			currentSrc:  src,
			currentDst:  ClusterInfo{Address: "https://vault-dst:8200", Namespace: "admin", Mount: "secret", BasePath: "other"},
			wantErr:     true,
			errContains: "destination mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewMigrationState(tt.stateSrc, tt.stateDst)
			err := state.Validate(tt.currentSrc, tt.currentDst)

			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestMigrationState_UpdateSecret(t *testing.T) {
	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}
	state := NewMigrationState(src, dst)

	secret1 := &Secret{Status: "completed", SourceVersionCount: 3, DestVersionCount: 3}
	state.UpdateSecret("secret/db/pass", secret1)

	if len(state.Secrets) != 1 {
		t.Errorf("UpdateSecret() len = %d, want 1", len(state.Secrets))
	}
	if state.Summary.Total != 1 {
		t.Errorf("UpdateSecret() Summary.Total = %d, want 1", state.Summary.Total)
	}
	if state.Summary.Completed != 1 {
		t.Errorf("UpdateSecret() Summary.Completed = %d, want 1", state.Summary.Completed)
	}

	secret2 := &Secret{Status: "failed", Error: "network timeout"}
	state.UpdateSecret("secret/api/key", secret2)

	if len(state.Secrets) != 2 {
		t.Errorf("UpdateSecret() len = %d, want 2", len(state.Secrets))
	}
	if state.Summary.Total != 2 {
		t.Errorf("UpdateSecret() Summary.Total = %d, want 2", state.Summary.Total)
	}
	if state.Summary.Failed != 1 {
		t.Errorf("UpdateSecret() Summary.Failed = %d, want 1", state.Summary.Failed)
	}

	secret3 := &Secret{Status: "skipped"}
	state.UpdateSecret("secret/unused", secret3)

	if state.Summary.Skipped != 1 {
		t.Errorf("UpdateSecret() Summary.Skipped = %d, want 1", state.Summary.Skipped)
	}
}

func TestMigrationState_GetSecret(t *testing.T) {
	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}
	state := NewMigrationState(src, dst)

	secret := &Secret{Status: "completed", SourceVersionCount: 5}
	state.UpdateSecret("secret/test", secret)

	got := state.GetSecret("secret/test")
	if got == nil {
		t.Fatal("GetSecret() returned nil")
	}
	if got.Status != "completed" {
		t.Errorf("GetSecret() Status = %q, want %q", got.Status, "completed")
	}

	notFound := state.GetSecret("secret/nonexistent")
	if notFound != nil {
		t.Errorf("GetSecret() nonexistent = %+v, want nil", notFound)
	}
}

func TestMigrationState_RecalculateSummary(t *testing.T) {
	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}
	state := NewMigrationState(src, dst)

	state.UpdateSecret("s1", &Secret{Status: "completed"})
	state.UpdateSecret("s2", &Secret{Status: "completed"})
	state.UpdateSecret("s3", &Secret{Status: "failed"})
	state.UpdateSecret("s4", &Secret{Status: "skipped"})
	state.UpdateSecret("s5", &Secret{Status: "recreated"})

	if state.Summary.Total != 5 {
		t.Errorf("Summary.Total = %d, want 5", state.Summary.Total)
	}
	if state.Summary.Completed != 3 {
		t.Errorf("Summary.Completed = %d, want 3 (completed + recreated)", state.Summary.Completed)
	}
	if state.Summary.Failed != 1 {
		t.Errorf("Summary.Failed = %d, want 1", state.Summary.Failed)
	}
	if state.Summary.Skipped != 1 {
		t.Errorf("Summary.Skipped = %d, want 1", state.Summary.Skipped)
	}
}

func TestMigrationState_SaveUpdatesTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "state.json")

	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}
	state := NewMigrationState(src, dst)

	originalTimestamp := state.Summary.LastUpdatedAt

	err := state.Save(path)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if state.Summary.LastUpdatedAt < originalTimestamp {
		t.Errorf("Save() LastUpdatedAt = %q, should be >= %q", state.Summary.LastUpdatedAt, originalTimestamp)
	}
}

func TestMigrationState_JSONRoundtrip(t *testing.T) {
	src := ClusterInfo{Address: "https://src:8200", Namespace: "admin", Mount: "secret", BasePath: "app"}
	dst := ClusterInfo{Address: "https://dst:8200", Namespace: "admin", Mount: "secret", BasePath: "app2"}
	original := NewMigrationState(src, dst)

	original.UpdateSecret("secret/test", &Secret{
		Status:             "completed",
		SourceVersionCount: 3,
		DestVersionCount:   3,
		VersionHashes:      map[string]string{"1": "sha256:abc"},
		VersionStates:      map[string]string{"1": "active"},
		MetadataChecksum:   "sha256:meta",
		MigratedAt:         "2026-05-01T10:00:00Z",
		RetryCount:         2,
	})

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded MigrationState
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if decoded.Version != original.Version {
		t.Errorf("JSON roundtrip Version mismatch")
	}
	if len(decoded.Secrets) != len(original.Secrets) {
		t.Errorf("JSON roundtrip Secrets count mismatch")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
