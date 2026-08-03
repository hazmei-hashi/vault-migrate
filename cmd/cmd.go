package cmd

import (
	"flag"
	"fmt"
	"log"
	"strings"
	"time"
	"vault-migrate/client"
	"vault-migrate/config"
	"vault-migrate/kvv2"
)

func Init() {
	var c config.VaultClientConfig

	flag.StringVar(&c.SrcAddr, "srcAddr", "", "Source cluster API address")
	flag.StringVar(&c.SrcToken, "srcToken", "", "Source cluster token")
	flag.StringVar(&c.SrcNamespace, "srcNamespace", "", "Source cluster namespace")
	flag.StringVar(&c.DstAddr, "dstAddr", "", "Destination cluster API address")
	flag.StringVar(&c.DstToken, "dstToken", "", "Destination cluster token")
	flag.StringVar(&c.DstNamespace, "dstNamespace", "", "Destination cluster namespace")
	flag.BoolVar(&c.TlsSkipVerify, "tlsSkipVerify", false, "Skip TLS verification of the Vault server certificates")
	flag.StringVar(&c.Mode, "mode", "kvv2", "Mode of operation")
	flag.StringVar(&c.LogLevel, "logLevel", "info", "Log level (info or debug)")
	flag.StringVar(&c.StateFile, "stateFile", ".vault-migrate-state.json", "Path to state file for tracking migration progress")
	flag.BoolVar(&c.NoState, "noState", false, "Disable state tracking (legacy mode)")
	flag.BoolVar(&c.ForceRecopy, "forceRecopy", false, "Re-copy secrets even if hashes match")
	flag.IntVar(&c.MaxRetries, "maxRetries", 3, "Maximum retry attempts for failed secrets")
	flag.DurationVar(&c.ClientTimeout, "clientTimeout", 60*time.Second, "HTTP client timeout for Vault API requests")
	flag.BoolVar(&c.ContinueOnError, "continueOnError", false, "Continue migration even if individual secrets fail")
	flag.BoolVar(&c.DryRun, "dryRun", false, "Preview migration without making changes")
	flag.Parse()

	setFlags := make(config.SetFlags)
	flag.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	logger := config.SetupLogger(c.LogLevel)
	logger.Debug("debug logging enabled")

	if err := validateConfig(c); err != nil {
		log.Fatalf("Configuration validation failed: %v", err)
	}

	srcClient, dstClient, err := client.BuildClients(c, setFlags)
	if err != nil {
		log.Fatalf("Error building clients: %v", err)
	}

	switch c.Mode {
	case "kvv2":
		err = kvv2.Init(srcClient, dstClient, c)
	default:
		log.Fatal("Supported modes: kvv2")
	}

	if err != nil {
		log.Fatalf("%v", err)
	}
}

func validateConfig(c config.VaultClientConfig) error {
	if strings.TrimSpace(c.SrcAddr) == "" {
		return fmt.Errorf("source address (srcAddr) cannot be empty")
	}

	if strings.TrimSpace(c.DstAddr) == "" {
		return fmt.Errorf("destination address (dstAddr) cannot be empty")
	}

	if !strings.HasPrefix(c.SrcAddr, "http://") && !strings.HasPrefix(c.SrcAddr, "https://") {
		return fmt.Errorf("source address must start with http:// or https://")
	}

	if !strings.HasPrefix(c.DstAddr, "http://") && !strings.HasPrefix(c.DstAddr, "https://") {
		return fmt.Errorf("destination address must start with http:// or https://")
	}

	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}
	if !validLogLevels[strings.ToLower(c.LogLevel)] {
		return fmt.Errorf("invalid log level: %s (must be debug, info, warn, or error)", c.LogLevel)
	}

	if c.MaxRetries < 0 {
		return fmt.Errorf("maxRetries must be >= 0")
	}

	if c.ClientTimeout <= 0 {
		return fmt.Errorf("clientTimeout must be > 0")
	}

	if c.StateFile == "" && !c.NoState {
		return fmt.Errorf("stateFile cannot be empty when state tracking is enabled")
	}

	return nil
}
