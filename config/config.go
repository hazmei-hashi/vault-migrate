package config

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

type VaultClientConfig struct {
	SrcAddr         string
	SrcToken        string
	SrcNamespace    string
	DstAddr         string
	DstToken        string
	DstNamespace    string
	TlsSkipVerify   bool
	Mode            string
	LogLevel        string
	StateFile       string
	NoState         bool
	ForceRecopy     bool
	MaxRetries      int
	ClientTimeout   time.Duration
	ContinueOnError bool
	DryRun          bool
}

type SetFlags map[string]bool

func (s SetFlags) Has(name string) bool { return s[name] }

func SetupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}
