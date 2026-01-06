package client

import (
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/vault/api"
	"golang.org/x/term"
)

type VaultClientConfig struct {
	SrcAddr       string
	SrcToken      string
	SrcNamespace  string
	DstAddr       string
	DstToken      string
	DstNamespace  string
	TlsSkipVerify bool
}

func BuildClients() (*api.Client, *api.Client, error) {

	var c VaultClientConfig

	fmt.Print("Source Vault API address: ")
	fmt.Scan(&c.SrcAddr)
	fmt.Print("Source Vault token: ")
	data, err := term.ReadPassword(0)
	if err != nil {
		log.Fatalln("Error reading source token")
	}
	c.SrcToken = string(data)
	fmt.Printf("\nSource namespace: ")
	fmt.Scan(&c.SrcNamespace)
	fmt.Printf("Destination Vault API address: ")
	fmt.Scan(&c.DstAddr)
	fmt.Print("Destination Vault token: ")
	data, err = term.ReadPassword(0)
	if err != nil {
		log.Fatalln("Error reading destination token")
	}
	c.DstToken = string(data)
	fmt.Printf("\nDestination namespace: ")
	fmt.Scan(&c.DstNamespace)
	fmt.Print("Skip TLS verification? (y/n): ")
	var skipTLS string
	fmt.Scan(&skipTLS)
	if skipTLS == "y" {
		c.TlsSkipVerify = true
	}

	srcClient, err := getClient(c.SrcAddr, c.SrcToken, c.SrcNamespace, c.TlsSkipVerify)
	if err != nil {
		return nil, nil, err
	}

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
		log.Fatalf("Health check failed for %s: %v\n", address, err)
	} else if !health.Initialized {
		log.Fatalf("%s is not initialized, aborting.\n", address)
	} else if health.Sealed {
		log.Fatalf("%s is sealed, aborting.\n", address)
	} else {
		lookup, err := client.Auth().Token().LookupSelf()
		if err != nil {
			log.Fatalf("\nToken lookup failed for %s\n", address)
		} else {
			ttl, _ := lookup.TokenTTL()
			log.Printf("Found initialized/unsealed cluster %s (Token TTL: %b)\n", health.ClusterID, ttl)
		}
	}

	// wait to set namespace until health checks completed
	client.SetNamespace(namespace)

	return client, nil
}
