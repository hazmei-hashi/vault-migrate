package scan

import (
	"context"

	"github.com/hashicorp/vault-client-go"
)

func Auths(srcClient, dstClient *vault.Client) error {
	srcAuthMountsResponse, err := srcClient.Read(context.Background(), "sys/auth")
	if err != nil {
		return err
	}

	var srcAuthMounts []string
	if srcAuthMountsResponse != nil {
		for mount, config := range srcAuthMountsResponse.Data {
			srcAuthMounts = append(srcAuthMounts, mount)
			authMount.Type = config.(map[string]interface{})["type"].(string)
		}
	}

	dstAuthMountsResponse, err := dstClient.Read(context.Background(), "sys/auth")
	if err != nil {
		return err
	}

	return nil
}
