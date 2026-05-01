package state

import (
	"testing"
)

func TestHashPayload(t *testing.T) {
	tests := []struct {
		name      string
		data      map[string]any
		wantErr   bool
		wantEmpty bool
	}{
		{
			name:      "nil data",
			data:      nil,
			wantEmpty: true,
		},
		{
			name:      "empty map",
			data:      map[string]any{},
			wantEmpty: false,
		},
		{
			name: "simple data",
			data: map[string]any{
				"password": "secret123",
				"username": "admin",
			},
			wantEmpty: false,
		},
		{
			name: "nested data",
			data: map[string]any{
				"db": map[string]any{
					"host": "localhost",
					"port": 5432,
				},
			},
			wantEmpty: false,
		},
		{
			name: "with special characters",
			data: map[string]any{
				"key": "value with 特殊字符 and émojis 🔒",
			},
			wantEmpty: false,
		},
		{
			name: "with numbers and bools",
			data: map[string]any{
				"count":   42,
				"enabled": true,
				"ratio":   3.14,
			},
			wantEmpty: false,
		},
		{
			name: "with null value",
			data: map[string]any{
				"key": nil,
			},
			wantEmpty: false,
		},
		{
			name: "large data",
			data: map[string]any{
				"certificate": string(make([]byte, 10000)),
			},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HashPayload(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("HashPayload() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantEmpty && got != "" {
				t.Errorf("HashPayload() = %q, want empty", got)
			}
			if !tt.wantEmpty && got == "" && !tt.wantErr {
				t.Errorf("HashPayload() = empty, want non-empty hash")
			}
			if got != "" && len(got) < 10 {
				t.Errorf("HashPayload() = %q, hash too short", got)
			}
			if got != "" && got[:7] != "sha256:" {
				t.Errorf("HashPayload() = %q, want sha256: prefix", got)
			}
		})
	}
}

func TestHashPayload_OrderIndependence(t *testing.T) {
	data1 := map[string]any{
		"a": "first",
		"b": "second",
		"c": "third",
	}
	data2 := map[string]any{
		"c": "third",
		"a": "first",
		"b": "second",
	}

	hash1, err1 := HashPayload(data1)
	hash2, err2 := HashPayload(data2)

	if err1 != nil || err2 != nil {
		t.Fatalf("HashPayload() errors: %v, %v", err1, err2)
	}
	if hash1 != hash2 {
		t.Errorf("HashPayload() order dependence: %q != %q", hash1, hash2)
	}
}

func TestHashPayload_Deterministic(t *testing.T) {
	data := map[string]any{
		"password": "secret123",
		"username": "admin",
	}

	hash1, _ := HashPayload(data)
	hash2, _ := HashPayload(data)

	if hash1 != hash2 {
		t.Errorf("HashPayload() not deterministic: %q != %q", hash1, hash2)
	}
}

func TestHashPayload_Different(t *testing.T) {
	data1 := map[string]any{"key": "value1"}
	data2 := map[string]any{"key": "value2"}

	hash1, _ := HashPayload(data1)
	hash2, _ := HashPayload(data2)

	if hash1 == hash2 {
		t.Errorf("HashPayload() collision: %q == %q", hash1, hash2)
	}
}

func TestHashMetadata(t *testing.T) {
	tests := []struct {
		name               string
		casRequired        bool
		maxVersions        int
		deleteVersionAfter string
		customMetadata     map[string]string
		wantEmpty          bool
	}{
		{
			name:               "all defaults",
			casRequired:        false,
			maxVersions:        0,
			deleteVersionAfter: "",
			customMetadata:     nil,
			wantEmpty:          false,
		},
		{
			name:               "cas required",
			casRequired:        true,
			maxVersions:        10,
			deleteVersionAfter: "1h",
			customMetadata:     nil,
			wantEmpty:          false,
		},
		{
			name:               "with custom metadata",
			casRequired:        false,
			maxVersions:        5,
			deleteVersionAfter: "",
			customMetadata: map[string]string{
				"owner": "team-platform",
				"env":   "production",
			},
			wantEmpty: false,
		},
		{
			name:               "full metadata",
			casRequired:        true,
			maxVersions:        20,
			deleteVersionAfter: "24h",
			customMetadata: map[string]string{
				"owner":   "team-security",
				"project": "vault-migrate",
			},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HashMetadata(tt.casRequired, tt.maxVersions, tt.deleteVersionAfter, tt.customMetadata)
			if err != nil {
				t.Errorf("HashMetadata() error = %v", err)
				return
			}
			if tt.wantEmpty && got != "" {
				t.Errorf("HashMetadata() = %q, want empty", got)
			}
			if !tt.wantEmpty && got == "" {
				t.Errorf("HashMetadata() = empty, want non-empty hash")
			}
			if got != "" && got[:7] != "sha256:" {
				t.Errorf("HashMetadata() = %q, want sha256: prefix", got)
			}
		})
	}
}

func TestHashMetadata_Deterministic(t *testing.T) {
	hash1, _ := HashMetadata(true, 10, "1h", map[string]string{"owner": "team"})
	hash2, _ := HashMetadata(true, 10, "1h", map[string]string{"owner": "team"})

	if hash1 != hash2 {
		t.Errorf("HashMetadata() not deterministic: %q != %q", hash1, hash2)
	}
}

func TestHashMetadata_Different(t *testing.T) {
	hash1, _ := HashMetadata(true, 10, "1h", nil)
	hash2, _ := HashMetadata(false, 10, "1h", nil)

	if hash1 == hash2 {
		t.Errorf("HashMetadata() collision: %q == %q", hash1, hash2)
	}
}

func TestSortedMap(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]any
	}{
		{
			name: "empty map",
			m:    map[string]any{},
		},
		{
			name: "single key",
			m:    map[string]any{"a": 1},
		},
		{
			name: "multiple keys",
			m: map[string]any{
				"z": "last",
				"a": "first",
				"m": "middle",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sortedMap(tt.m)
			if len(got) != len(tt.m) {
				t.Errorf("sortedMap() len = %d, want %d", len(got), len(tt.m))
			}
			for k, v := range tt.m {
				if got[k] != v {
					t.Errorf("sortedMap() key %q = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}
