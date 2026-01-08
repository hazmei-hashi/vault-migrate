package client

import (
	"fmt"
	"log"
	"time"

	"vault-migrate/config"

	"github.com/hashicorp/vault/api"
	"golang.org/x/term"
)

func BuildClients(c config.VaultClientConfig, setFlags config.SetFlags) (*api.Client, *api.Client, error) {

	logger := config.SetupLogger(c.LogLevel)

	if !setFlags.Has("srcAddr") {
		fmt.Print("Source Vault API address: ")
		fmt.Scan(&c.SrcAddr)
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
		logger.Debug("Found flag", "srcToken", c.SrcToken)
	}

	if !setFlags.Has("srcNamespace") {
		fmt.Printf("\nSource namespace: ")
		fmt.Scan(&c.SrcNamespace)
	} else {
		logger.Debug("Found flag", "srcNamespace", c.SrcNamespace)
	}

	if !setFlags.Has("dstAddr") {
		fmt.Printf("Destination Vault API address: ")
		fmt.Scan(&c.DstAddr)
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
		logger.Debug("Found flag", "dstToken", c.DstToken)
	}

	if !setFlags.Has("dstNamespace") {
		fmt.Printf("\nDestination namespace: ")
		fmt.Scan(&c.DstNamespace)
	} else {
		logger.Debug("Found flag", "dstNamespace", c.DstNamespace)
	}

	if !setFlags.Has("tlsSkipVerify") {
		fmt.Print("Skip TLS verification? (y/n): ")
		var skipTLS string
		fmt.Scan(&skipTLS)
		if skipTLS == "y" {
			c.TlsSkipVerify = true
		}
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

	clientConfig.ConfigureTLS(&api.TLSConfig{Insecure: skipVerify})
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
	} else {
		lookup, err := client.Auth().Token().LookupSelf()
		if err != nil {
			log.Fatalf("Token lookup failed for %s", address)
		} else {
			ttl, _ := lookup.TokenTTL()
			log.Printf("Found initialized/unsealed cluster %s (Token TTL: %b)\n", health.ClusterID, ttl)
		}
	}

	// wait to set namespace until health checks completed
	client.SetNamespace(namespace)

	return client, nil
}
