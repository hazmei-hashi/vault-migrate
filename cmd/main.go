package cmd

import (
	"log"
	"vault-migrate/client"
	"vault-migrate/kvv2"
)

func Init() {
	srcClient, dstClient, err := client.BuildClients()
	if err != nil {
		log.Fatalf("Error building clients: %v", err)
	}

	err = kvv2.Copy(srcClient, dstClient)
}
