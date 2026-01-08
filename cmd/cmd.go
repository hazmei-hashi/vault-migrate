package cmd

import (
	"flag"
	"log"
	"vault-migrate/client"
	"vault-migrate/config"
	"vault-migrate/kvv2"
)

func Init() {
	var c config.VaultClientConfig

	flag.StringVar(&c.SrcAddr, "srcAddr", "https://localhost:8200", "Source cluster API address")
	flag.StringVar(&c.SrcToken, "srcToken", "", "Source cluster token")
	flag.StringVar(&c.SrcNamespace, "srcNamespace", "", "Source cluster namespace")
	flag.StringVar(&c.DstAddr, "dstAddr", "https://localhost:8300", "Destination cluster API address")
	flag.StringVar(&c.DstToken, "dstToken", "", "Destination cluster token")
	flag.StringVar(&c.DstNamespace, "dstNamespace", "", "Destination cluster namespace")
	flag.BoolVar(&c.TlsSkipVerify, "tlsSkipVerify", false, "Skip TLS verification of the Vault server certificates")
	flag.StringVar(&c.Mode, "mode", "kvv2", "Mode of operation")
	flag.StringVar(&c.LogLevel, "logLevel", "info", "Log level (info or debug)")
	flag.Parse()

	setFlags := make(config.SetFlags)
	flag.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	logger := config.SetupLogger(c.LogLevel)
	logger.Debug("debug logging enabled")

	srcClient, dstClient, err := client.BuildClients(c, setFlags)
	if err != nil {
		log.Fatalf("Error building clients: %v", err)
	}

	switch c.Mode {
	case "kvv2":
		err = kvv2.Init(srcClient, dstClient, c.LogLevel)
	default:
		log.Fatal("Supported modes: kvv2")
	}

	if err != nil {
		log.Fatalf("%v", err)
	}
}
