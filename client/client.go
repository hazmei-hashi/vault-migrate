package client

import (
	"fmt"
	"log"
	"strings"
	"time"

	"vault-migrate/config"

	"github.com/hashicorp/vault/api"
	"golang.org/x/term"
)

func BuildClients(c config.VaultClientConfig, setFlags config.SetFlags) (*api.Client, *api.Client, error) {

	logger := config.SetupLogger(c.LogLevel)

	if !setFlags.Has("srcAddr") {
		v, err := config.Prompt("Source Vault API address: ")
		if err != nil {
			return nil, nil, fmt.Errorf("read source address: %w", err)
		}
		c.SrcAddr = v
	} else {
		logger.Debug("Found flag", "srcAddr", c.SrcAddr)
	}

	if !setFlags.Has("srcToken") {
		fmt.Print("Source Vault token: ")
		data, err := term.ReadPassword(0)
		if err != nil {
			log.Fatalln("Error reading source token")
		}
		c.SrcToken = string(data)
	} else {
		logger.Debug("Found flag", "srcToken", "[redacted]")
	}

	if !setFlags.Has("srcNamespace") {
		v, err := config.Prompt("\nSource namespace: ")
		if err != nil {
			return nil, nil, fmt.Errorf("read source namespace: %w", err)
		}
		c.SrcNamespace = v
	} else {
		logger.Debug("Found flag", "srcNamespace", c.SrcNamespace)
	}

	if !setFlags.Has("dstAddr") {
		v, err := config.Prompt("Destination Vault API address: ")
		if err != nil {
			return nil, nil, fmt.Errorf("read destination address: %w", err)
		}
		c.DstAddr = v
	} else {
		logger.Debug("Found flag", "dstAddr", c.DstAddr)
	}

	if !setFlags.Has("dstToken") {
		fmt.Print("Destination Vault token: ")
		data, err := term.ReadPassword(0)
		if err != nil {
			log.Fatalln("Error reading destination token")
		}
		c.DstToken = string(data)
	} else {
		logger.Debug("Found flag", "dstToken", "[redacted]")
	}

	if !setFlags.Has("dstNamespace") {
		v, err := config.Prompt("\nDestination namespace: ")
		if err != nil {
			return nil, nil, fmt.Errorf("read destination namespace: %w", err)
		}
		c.DstNamespace = v
	} else {
		logger.Debug("Found flag", "dstNamespace", c.DstNamespace)
	}

	if !setFlags.Has("tlsSkipVerify") {
		v, err := config.Prompt("Skip TLS verification? (y/n): ")
		if err != nil {
			return nil, nil, fmt.Errorf("read tls skip verify: %w", err)
		}
		c.TlsSkipVerify = strings.EqualFold(v, "y") || strings.EqualFold(v, "yes")
	} else {
		logger.Debug("Found flag", "tlsSkipVerify", c.TlsSkipVerify)
	}

	logger.Debug("Building source client...")
	srcClient, err := getClient(c.SrcAddr, c.SrcToken, c.SrcNamespace, c.TlsSkipVerify)
	if err != nil {
		return nil, nil, err
	}

	logger.Debug("Building destination client...")
	dstClient, err := getClient(c.DstAddr, c.DstToken, c.DstNamespace, c.TlsSkipVerify)
	if err != nil {
		return nil, nil, err
	}

	return srcClient, dstClient, nil
}

func getClient(address string, token string, namespace string, skipVerify bool) (*api.Client, error) {
	clientConfig := &api.Config{
		Address: address,
	}

	if err := clientConfig.ConfigureTLS(&api.TLSConfig{Insecure: skipVerify}); err != nil {
		return nil, fmt.Errorf("configure TLS for %s: %w", address, err)
	}
	client, err := api.NewClient(clientConfig)
	if err != nil {
		log.Fatal(err)
	}
	client.SetReadYourWrites(true)
	client.SetClientTimeout(3 * time.Second)
	client.SetToken(token)

	health, err := client.Sys().Health()
	if err != nil {
		log.Fatalf("Health check failed for %s: %v", address, err)
	} else if !health.Initialized {
		log.Fatalf("%s is not initialized, aborting.", address)
	} else if health.Sealed {
		log.Fatalf("%s is sealed, aborting.", address)
	}

	// Namespace-scoped Enterprise tokens need the namespace set before
	// LookupSelf, otherwise lookup is fatal against the wrong namespace.
	client.SetNamespace(namespace)

	lookup, err := client.Auth().Token().LookupSelf()
	if err != nil {
		log.Fatalf("Token lookup failed for %s (namespace %q): %v", address, namespace, err)
	} else {
		ttl, _ := lookup.TokenTTL()
		log.Printf("Found initialized/unsealed cluster %s (Token TTL: %v)\n", health.ClusterID, ttl)
	}

	return client, nil
}
