package kvv2

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/vault/api"
)

func TestTrimSlashes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", ""},
		{"no slashes", "path", "path"},
		{"leading slash", "/path", "path"},
		{"trailing slash", "path/", "path"},
		{"both slashes", "/path/", "path"},
		{"multiple leading", "///path", "path"},
		{"multiple trailing", "path///", "path"},
		{"both multiple", "///path///", "path"},
		{"nested path", "/a/b/c/", "a/b/c"},
		{"only slashes", "///", ""},
		{"with spaces", "  /path/  ", "path"},
		{"spaces and slashes", "  ///path///  ", "path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimSlashes(tt.input)
			if got != tt.want {
				t.Errorf("trimSlashes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestJoinRel(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{"both empty", "", "", ""},
		{"a empty", "", "path", "path"},
		{"b empty", "path", "", "path"},
		{"simple join", "a", "b", "a/b"},
		{"a with trailing slash", "a/", "b", "a/b"},
		{"b with leading slash", "a", "/b", "a/b"},
		{"both with slashes", "a/", "/b", "a/b"},
		{"multiple slashes", "a///", "///b", "a/b"},
		{"nested paths", "a/b", "c/d", "a/b/c/d"},
		{"with spaces", "  a  ", "  b  ", "a/b"},
		{"complex nested", "/a/b/", "/c/d/", "a/b/c/d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinRel(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("joinRel(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestIsMetadataNotFound covers B17's structural rewrite of isNotFound
// (renamed isMetadataNotFound): only errMetadataNotFound (via errors.Is) or
// an *api.ResponseError with StatusCode 404 (via errors.As) count as
// not-found. Plain errors that merely *mention* "404" or "not found" in
// their message text -- what the old substring matcher keyed on -- must now
// return false, since that substring matching is exactly what silently
// swallowed real errors (403, 400, 500, timeouts) at call sites.
func TestIsMetadataNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		// Inverted from the old (buggy) substring-matching expectations:
		// message text alone no longer implies not-found.
		{"404 error (message text only, not a ResponseError)", errors.New("status code 404"), false},
		{"not found message", errors.New("secret not found"), false},
		{"no handler for route", errors.New("no handler for route"), false},
		{"unsupported path", errors.New("unsupported path"), false},
		{"404 in message", errors.New("API returned 404 not found"), false},
		// Unaffected by the rewrite - never matched, still don't match.
		{"permission denied", errors.New("permission denied"), false},
		{"403 forbidden (message text only)", errors.New("status code 403"), false},
		{"500 error", errors.New("status code 500"), false},
		{"connection error", errors.New("connection refused"), false},
		{"generic error", errors.New("something went wrong"), false},
		// New structural coverage (B17): the only two ways a genuine
		// not-found reaches this function.
		{"errMetadataNotFound sentinel", errMetadataNotFound, true},
		{"api.ResponseError 404", &api.ResponseError{StatusCode: http.StatusNotFound}, true},
		{"api.ResponseError 403", &api.ResponseError{StatusCode: http.StatusForbidden}, false},
		{"api.ResponseError 500", &api.ResponseError{StatusCode: http.StatusInternalServerError}, false},
		{
			"wrapped api.ResponseError 404",
			fmt.Errorf("wrapped: %w", &api.ResponseError{StatusCode: http.StatusNotFound}),
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMetadataNotFound(tt.err)
			if got != tt.want {
				t.Errorf("isMetadataNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsMetadataNotFound_Sentinel covers the errMetadataNotFound sentinel
// matched structurally via errors.Is, independent of message text, and
// closes out B17's previously-documented "path containing 404" gap: a
// genuine 500 whose *path* happens to contain the substring "404" (e.g. key
// "app/error-404") is no longer misread as not-found now that message-text
// substring matching has been deleted entirely.
func TestIsMetadataNotFound_Sentinel(t *testing.T) {
	sentinelErr := fmt.Errorf("%w: empty metadata response for %q", errMetadataNotFound, "secret/metadata/app/x")
	if !isMetadataNotFound(sentinelErr) {
		t.Errorf("isMetadataNotFound(%v) = false, want true (errMetadataNotFound sentinel)", sentinelErr)
	}

	// Genuine 500, no ResponseError involved - just a key path that
	// contains the digits "404". B17 fixed: this is correctly NOT
	// classified as not-found now that substring matching is gone.
	pathContaining404Err := errors.New("500 Internal Server Error: failed to read secret/metadata/app/error-404")
	if got := isMetadataNotFound(pathContaining404Err); got {
		t.Errorf("isMetadataNotFound(%v) = %v, want false — B17 fixed the substring-match misread of a genuine 500 as not-found", pathContaining404Err, got)
	}
}

func TestDstKeyFor(t *testing.T) {
	tests := []struct {
		name      string
		srcBase   string
		dstBase   string
		srcRelKey string
		want      string
	}{
		{
			name:      "empty base paths",
			srcBase:   "",
			dstBase:   "",
			srcRelKey: "secret/path",
			want:      "secret/path",
		},
		{
			name:      "empty src base",
			srcBase:   "",
			dstBase:   "dest",
			srcRelKey: "secret/path",
			want:      "dest/secret/path",
		},
		{
			name:      "exact match src base",
			srcBase:   "myapp",
			dstBase:   "newapp",
			srcRelKey: "myapp",
			want:      "newapp",
		},
		{
			name:      "under src base",
			srcBase:   "myapp",
			dstBase:   "newapp",
			srcRelKey: "myapp/db/password",
			want:      "newapp/db/password",
		},
		{
			name:      "not under src base",
			srcBase:   "myapp",
			dstBase:   "newapp",
			srcRelKey: "other/secret",
			want:      "newapp/other/secret",
		},
		{
			name:      "nested src base",
			srcBase:   "myapp/prod",
			dstBase:   "newapp/staging",
			srcRelKey: "myapp/prod/db/password",
			want:      "newapp/staging/db/password",
		},
		{
			name:      "with slashes",
			srcBase:   "/myapp/",
			dstBase:   "/newapp/",
			srcRelKey: "/myapp/secret/",
			want:      "newapp/secret",
		},
		{
			name:      "empty dst base",
			srcBase:   "myapp",
			dstBase:   "",
			srcRelKey: "myapp/secret",
			want:      "secret",
		},
		{
			name:      "partial prefix match",
			srcBase:   "app",
			dstBase:   "new",
			srcRelKey: "application/secret",
			want:      "new/application/secret",
		},
		{
			name:      "multi-slash base and key, empty dst base",
			srcBase:   "//app//",
			dstBase:   "",
			srcRelKey: "//app//db//",
			want:      "db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Migrator{
				Src: KVV2Cluster{BasePath: tt.srcBase},
				Dst: KVV2Cluster{BasePath: tt.dstBase},
			}
			got := m.dstKeyFor(tt.srcRelKey)
			if got != tt.want {
				t.Errorf("dstKeyFor(%q) = %q, want %q (srcBase=%q, dstBase=%q)",
					tt.srcRelKey, got, tt.want, tt.srcBase, tt.dstBase)
			}
		})
	}
}

func TestGetMaxVersion(t *testing.T) {
	tests := []struct {
		name     string
		versions map[string]struct {
			DeletionTime string `json:"deletion_time"`
			Destroyed    bool   `json:"destroyed"`
		}
		want int
	}{
		{
			name: "empty versions",
			versions: map[string]struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			}{},
			want: 0,
		},
		{
			name: "single version",
			versions: map[string]struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			}{
				"1": {},
			},
			want: 1,
		},
		{
			name: "sequential versions",
			versions: map[string]struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			}{
				"1": {},
				"2": {},
				"3": {},
			},
			want: 3,
		},
		{
			name: "non-sequential versions",
			versions: map[string]struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			}{
				"1": {},
				"5": {},
				"3": {},
			},
			want: 5,
		},
		{
			name: "invalid version strings ignored",
			versions: map[string]struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			}{
				"1":       {},
				"invalid": {},
				"3":       {},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := &kv2MetadataResp{}
			meta.Data.Versions = tt.versions

			got := getMaxVersion(meta)
			if got != tt.want {
				t.Errorf("getMaxVersion() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMapToStruct(t *testing.T) {
	type TestStruct struct {
		Name    string `json:"name"`
		Age     int    `json:"age"`
		Enabled bool   `json:"enabled"`
	}

	tests := []struct {
		name    string
		input   map[string]any
		wantErr bool
	}{
		{
			name: "valid mapping",
			input: map[string]any{
				"name":    "test",
				"age":     42,
				"enabled": true,
			},
			wantErr: false,
		},
		{
			name:    "empty map",
			input:   map[string]any{},
			wantErr: false,
		},
		{
			name:    "nil map",
			input:   nil,
			wantErr: false,
		},
		{
			name: "extra fields ignored",
			input: map[string]any{
				"name":  "test",
				"age":   42,
				"extra": "ignored",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result TestStruct
			err := mapToStruct(tt.input, &result)

			if (err != nil) != tt.wantErr {
				t.Errorf("mapToStruct() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && tt.input != nil {
				if name, ok := tt.input["name"].(string); ok && result.Name != name {
					t.Errorf("mapToStruct() Name = %q, want %q", result.Name, name)
				}
				if age, ok := tt.input["age"].(int); ok && result.Age != age {
					t.Errorf("mapToStruct() Age = %d, want %d", result.Age, age)
				}
			}
		})
	}
}
