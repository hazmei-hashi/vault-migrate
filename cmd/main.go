package cmd

import (
	"context"
	"fmt"
	"log"
	"vault-migrate/client"
	"vault-migrate/scan"
)

func Init() {
	srcClient, dstClient, err := client.BuildClients()
	if err != nil {
		log.Fatalf("Error building clients: %v", err)
	}
	srcHealth, err := srcClient.System.ReadHealthStatus(context.Background())
	if err != nil {
		log.Fatalf("Error reading source health status: %v", err)
	}
	fmt.Printf("Source cluster health: initialized: %v sealed: %v\n", srcHealth.Data["initialized"], srcHealth.Data["sealed"])
	dstHealth, err := dstClient.System.ReadHealthStatus(context.Background())
	if err != nil {
		log.Fatalf("Error reading destination health status: %v", err)
	}
	fmt.Printf("Destination cluster health: initialized: %v sealed: %v\n", dstHealth.Data["initialized"], dstHealth.Data["sealed"])

	err = scan.Auths(srcClient, dstClient)
}
