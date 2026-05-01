package config

import (
	"testing"
)

func TestSetFlags_Has(t *testing.T) {
	tests := []struct {
		name     string
		flags    SetFlags
		checkKey string
		want     bool
	}{
		{
			name:     "flag exists",
			flags:    SetFlags{"srcAddr": true, "dstAddr": true},
			checkKey: "srcAddr",
			want:     true,
		},
		{
			name:     "flag does not exist",
			flags:    SetFlags{"srcAddr": true},
			checkKey: "dstAddr",
			want:     false,
		},
		{
			name:     "empty flags",
			flags:    SetFlags{},
			checkKey: "srcAddr",
			want:     false,
		},
		{
			name:     "nil flags",
			flags:    nil,
			checkKey: "srcAddr",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.flags.Has(tt.checkKey)
			if got != tt.want {
				t.Errorf("Has(%q) = %v, want %v", tt.checkKey, got, tt.want)
			}
		})
	}
}

func TestSetupLogger(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{"debug level", "debug"},
		{"info level", "info"},
		{"warn level", "warn"},
		{"error level", "error"},
		{"uppercase DEBUG", "DEBUG"},
		{"mixed case Info", "Info"},
		{"unknown defaults to info", "unknown"},
		{"empty defaults to info", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := SetupLogger(tt.level)
			if logger == nil {
				t.Error("SetupLogger() returned nil")
			}
		})
	}
}
