package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type MigrationState struct {
	Version     string             `json:"version"`
	MigrationID string             `json:"migration_id"`
	Source      ClusterInfo        `json:"source"`
	Destination ClusterInfo        `json:"destination"`
	Secrets     map[string]*Secret `json:"secrets"`
	Summary     Summary            `json:"summary"`
}

type ClusterInfo struct {
	Address   string `json:"address"`
	Namespace string `json:"namespace"`
	Mount     string `json:"mount"`
	BasePath  string `json:"base_path"`
}

type Secret struct {
	Status             string            `json:"status"`
	SourceVersionCount int               `json:"source_version_count"`
	DestVersionCount   int               `json:"dest_version_count"`
	VersionHashes      map[string]string `json:"version_hashes"`
	VersionStates      map[string]string `json:"version_states"`
	MetadataChecksum   string            `json:"metadata_checksum"`
	MigratedAt         string            `json:"migrated_at,omitempty"`
	Error              string            `json:"error,omitempty"`
	RetryCount         int               `json:"retry_count"`
}

type Summary struct {
	Total         int    `json:"total"`
	Completed     int    `json:"completed"`
	Failed        int    `json:"failed"`
	Skipped       int    `json:"skipped"`
	StartedAt     string `json:"started_at"`
	LastUpdatedAt string `json:"last_updated_at"`
}

func NewMigrationState(src, dst ClusterInfo) *MigrationState {
	now := time.Now().UTC().Format(time.RFC3339)
	return &MigrationState{
		Version:     "1.0",
		MigrationID: fmt.Sprintf("migration_%s", now),
		Source:      src,
		Destination: dst,
		Secrets:     make(map[string]*Secret),
		Summary: Summary{
			StartedAt:     now,
			LastUpdatedAt: now,
		},
	}
}

func Load(path string) (*MigrationState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}

	var state MigrationState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}

	return &state, nil
}

func (s *MigrationState) Save(path string) error {
	s.Summary.LastUpdatedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// ponytail: temp-file + rename guards against a killed process leaving a
	// truncated/partial state file; it does not guard against power loss
	// (no fsync). Add fsync before rename if that ceiling matters.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp state file: %w", err)
	}

	return nil
}

func (s *MigrationState) Validate(src, dst ClusterInfo) error {
	if s.Source.Address != src.Address ||
		s.Source.Namespace != src.Namespace ||
		s.Source.Mount != src.Mount ||
		s.Source.BasePath != src.BasePath {
		return fmt.Errorf(
			"state file source mismatch:\n  State: %s/%s (mount: %s, base: %s)\n  Current: %s/%s (mount: %s, base: %s)",
			s.Source.Address, s.Source.Namespace, s.Source.Mount, s.Source.BasePath,
			src.Address, src.Namespace, src.Mount, src.BasePath,
		)
	}

	if s.Destination.Address != dst.Address ||
		s.Destination.Namespace != dst.Namespace ||
		s.Destination.Mount != dst.Mount ||
		s.Destination.BasePath != dst.BasePath {
		return fmt.Errorf(
			"state file destination mismatch:\n  State: %s/%s (mount: %s, base: %s)\n  Current: %s/%s (mount: %s, base: %s)",
			s.Destination.Address, s.Destination.Namespace, s.Destination.Mount, s.Destination.BasePath,
			dst.Address, dst.Namespace, dst.Mount, dst.BasePath,
		)
	}

	return nil
}

func (s *MigrationState) UpdateSecret(key string, secret *Secret) {
	s.Secrets[key] = secret
	s.recalculateSummary()
}

func (s *MigrationState) recalculateSummary() {
	s.Summary.Total = len(s.Secrets)
	s.Summary.Completed = 0
	s.Summary.Failed = 0
	s.Summary.Skipped = 0

	for _, secret := range s.Secrets {
		switch secret.Status {
		case "completed", "recreated":
			s.Summary.Completed++
		case "failed":
			s.Summary.Failed++
		case "skipped":
			s.Summary.Skipped++
		}
	}
}

func (s *MigrationState) GetSecret(key string) *Secret {
	return s.Secrets[key]
}
