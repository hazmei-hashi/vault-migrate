package cmd

import (
	"testing"
	"time"
	"vault-migrate/config"
)

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  config.VaultClientConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				MaxRetries:    3,
				ClientTimeout: 60 * time.Second,
				StateFile:     ".vault-migrate-state.json",
				NoState:       false,
			},
			wantErr: false,
		},
		{
			name: "empty source address",
			config: config.VaultClientConfig{
				SrcAddr:  "",
				DstAddr:  "https://vault-dst.example.com:8200",
				LogLevel: "info",
			},
			wantErr: true,
			errMsg:  "source address (srcAddr) cannot be empty",
		},
		{
			name: "empty destination address",
			config: config.VaultClientConfig{
				SrcAddr:  "https://vault-src.example.com:8200",
				DstAddr:  "",
				LogLevel: "info",
			},
			wantErr: true,
			errMsg:  "destination address (dstAddr) cannot be empty",
		},
		{
			name: "source address without protocol",
			config: config.VaultClientConfig{
				SrcAddr:  "vault-src.example.com:8200",
				DstAddr:  "https://vault-dst.example.com:8200",
				LogLevel: "info",
			},
			wantErr: true,
			errMsg:  "source address must start with http:// or https://",
		},
		{
			name: "destination address without protocol",
			config: config.VaultClientConfig{
				SrcAddr:  "https://vault-src.example.com:8200",
				DstAddr:  "vault-dst.example.com:8200",
				LogLevel: "info",
			},
			wantErr: true,
			errMsg:  "destination address must start with http:// or https://",
		},
		{
			name: "http protocol allowed",
			config: config.VaultClientConfig{
				SrcAddr:       "http://localhost:8200",
				DstAddr:       "http://localhost:8300",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "invalid log level",
			config: config.VaultClientConfig{
				SrcAddr:  "https://vault-src.example.com:8200",
				DstAddr:  "https://vault-dst.example.com:8200",
				LogLevel: "verbose",
			},
			wantErr: true,
			errMsg:  "invalid log level",
		},
		{
			name: "debug log level",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "debug",
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "warn log level",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "warn",
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "error log level",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "error",
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "case insensitive log level",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "INFO",
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "negative maxRetries",
			config: config.VaultClientConfig{
				SrcAddr:    "https://vault-src.example.com:8200",
				DstAddr:    "https://vault-dst.example.com:8200",
				LogLevel:   "info",
				MaxRetries: -1,
			},
			wantErr: true,
			errMsg:  "maxRetries must be >= 0",
		},
		{
			name: "zero maxRetries allowed",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				MaxRetries:    0,
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "empty stateFile with state tracking enabled",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "",
				NoState:       false,
			},
			wantErr: true,
			errMsg:  "stateFile cannot be empty when state tracking is enabled",
		},
		{
			name: "empty stateFile with noState flag",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "",
				NoState:       true,
			},
			wantErr: false,
		},
		{
			name: "whitespace-only source address",
			config: config.VaultClientConfig{
				SrcAddr:  "   ",
				DstAddr:  "https://vault-dst.example.com:8200",
				LogLevel: "info",
			},
			wantErr: true,
			errMsg:  "source address (srcAddr) cannot be empty",
		},
		{
			name: "valid clientTimeout",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 30 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: false,
		},
		{
			name: "zero clientTimeout rejected",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 0,
				StateFile:     "state.json",
			},
			wantErr: true,
			errMsg:  "clientTimeout must be > 0",
		},
		{
			name: "negative clientTimeout rejected",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: -5 * time.Second,
				StateFile:     "state.json",
			},
			wantErr: true,
			errMsg:  "clientTimeout must be > 0",
		},
		// rollback-specific validation
		{
			name: "rollback with empty stateFile and noState false gets specific message",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "",
				NoState:       false,
				Rollback:      true,
			},
			wantErr: true,
			errMsg:  "-rollback requires -stateFile",
		},
		{
			name: "rollback with empty stateFile and noState true gets noState message",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "",
				NoState:       true,
				Rollback:      true,
			},
			wantErr: true,
			errMsg:  "-rollback and -noState are incompatible",
		},
		{
			name: "rollback with valid stateFile is accepted",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "state.json",
				NoState:       false,
				Rollback:      true,
			},
			wantErr: false,
		},
		{
			name: "non-rollback empty stateFile still gets generic message",
			config: config.VaultClientConfig{
				SrcAddr:       "https://vault-src.example.com:8200",
				DstAddr:       "https://vault-dst.example.com:8200",
				LogLevel:      "info",
				ClientTimeout: 60 * time.Second,
				StateFile:     "",
				NoState:       false,
				Rollback:      false,
			},
			wantErr: true,
			errMsg:  "stateFile cannot be empty when state tracking is enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !contains(err.Error(), tt.errMsg) {
					t.Errorf("validateConfig() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
