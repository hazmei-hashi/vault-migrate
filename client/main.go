package client

import (
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/vault-client-go"
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

func BuildClients() (*vault.Client, *vault.Client, error) {

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

func getClient(address string, token string, namespace string, skipVerify bool) (*vault.Client, error) {
	tls := vault.TLSConfiguration{}
	tls.InsecureSkipVerify = skipVerify

	client, err := vault.New(
		vault.WithAddress(address),
		vault.WithRequestTimeout(10*time.Second),
		vault.WithRetryConfiguration(vault.RetryConfiguration{}),
		vault.WithTLS(tls),
	)
	if err != nil {
		return nil, fmt.Errorf("error initializing client for %s: %w", address, err)
	}
	client.SetToken(token)
	client.SetNamespace(namespace)

	return client, nil
}
