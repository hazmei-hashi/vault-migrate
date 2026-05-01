package cmd

import (
	"testing"
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
				SrcAddr:    "https://vault-src.example.com:8200",
				DstAddr:    "https://vault-dst.example.com:8200",
				LogLevel:   "info",
				MaxRetries: 3,
				StateFile:  ".vault-migrate-state.json",
				NoState:    false,
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
				SrcAddr:   "http://localhost:8200",
				DstAddr:   "http://localhost:8300",
				LogLevel:  "info",
				StateFile: "state.json",
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
				SrcAddr:   "https://vault-src.example.com:8200",
				DstAddr:   "https://vault-dst.example.com:8200",
				LogLevel:  "debug",
				StateFile: "state.json",
			},
			wantErr: false,
		},
		{
			name: "warn log level",
			config: config.VaultClientConfig{
				SrcAddr:   "https://vault-src.example.com:8200",
				DstAddr:   "https://vault-dst.example.com:8200",
				LogLevel:  "warn",
				StateFile: "state.json",
			},
			wantErr: false,
		},
		{
			name: "error log level",
			config: config.VaultClientConfig{
				SrcAddr:   "https://vault-src.example.com:8200",
				DstAddr:   "https://vault-dst.example.com:8200",
				LogLevel:  "error",
				StateFile: "state.json",
			},
			wantErr: false,
		},
		{
			name: "case insensitive log level",
			config: config.VaultClientConfig{
				SrcAddr:   "https://vault-src.example.com:8200",
				DstAddr:   "https://vault-dst.example.com:8200",
				LogLevel:  "INFO",
				StateFile: "state.json",
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
				SrcAddr:    "https://vault-src.example.com:8200",
				DstAddr:    "https://vault-dst.example.com:8200",
				LogLevel:   "info",
				MaxRetries: 0,
				StateFile:  "state.json",
			},
			wantErr: false,
		},
		{
			name: "empty stateFile with state tracking enabled",
			config: config.VaultClientConfig{
				SrcAddr:   "https://vault-src.example.com:8200",
				DstAddr:   "https://vault-dst.example.com:8200",
				LogLevel:  "info",
				StateFile: "",
				NoState:   false,
			},
			wantErr: true,
			errMsg:  "stateFile cannot be empty when state tracking is enabled",
		},
		{
			name: "empty stateFile with noState flag",
			config: config.VaultClientConfig{
				SrcAddr:   "https://vault-src.example.com:8200",
				DstAddr:   "https://vault-dst.example.com:8200",
				LogLevel:  "info",
				StateFile: "",
				NoState:   true,
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
