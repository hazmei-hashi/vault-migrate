package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/vault/api"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Helper to convert JSON number to int
func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case int:
		return n
	default:
		return 0
	}
}

const (
	srcAddr   = "http://127.0.0.1:8200"
	srcToken  = "root-token-source"
	dstAddr   = "http://127.0.0.1:8300"
	dstToken  = "root-token-destination"
	mountPath = "secret"
)

// TestMain checks if E2E tests should run
func TestMain(m *testing.M) {
	if os.Getenv("E2E_TESTS") != "1" {
		fmt.Println("Skipping E2E tests (set E2E_TESTS=1 to run)")
		os.Exit(0)
	}

	ctx := context.Background()

	// Wait for Vault instances to be ready (assumes containers already running)
	fmt.Println("Waiting for Vault instances...")
	if err := waitForVault(ctx, srcAddr, srcToken); err != nil {
		fmt.Printf("Source Vault not ready: %v\n", err)
		fmt.Println("Make sure to run: docker-compose up -d")
		os.Exit(1)
	}
	if err := waitForVault(ctx, dstAddr, dstToken); err != nil {
		fmt.Printf("Destination Vault not ready: %v\n", err)
		fmt.Println("Make sure to run: docker-compose up -d")
		os.Exit(1)
	}

	fmt.Println("Vault instances ready")

	// Run tests
	code := m.Run()

	os.Exit(code)
}

func waitForVault(ctx context.Context, addr, token string) error {
	config := api.DefaultConfig()
	config.Address = addr
	client, err := api.NewClient(config)
	if err != nil {
		return err
	}
	client.SetToken(token)

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for Vault at %s", addr)
		case <-ticker.C:
			_, err := client.Sys().Health()
			if err == nil {
				return nil
			}
		}
	}
}

// Helper to create Vault client
func newVaultClient(t *testing.T, addr, token string) *api.Client {
	t.Helper()
	config := api.DefaultConfig()
	config.Address = addr
	client, err := api.NewClient(config)
	if err != nil {
		t.Fatalf("Failed to create Vault client: %v", err)
	}
	client.SetToken(token)
	return client
}

// Helper to write secret
func writeSecret(t *testing.T, client *api.Client, path string, data map[string]interface{}) {
	t.Helper()

	writePath := fmt.Sprintf("%s/data/%s", mountPath, path)
	payload := map[string]interface{}{
		"data": data,
	}

	_, err := client.Logical().Write(writePath, payload)
	if err != nil {
		t.Fatalf("Failed to write secret %s: %v", path, err)
	}
}

// Helper to read secret
func readSecret(t *testing.T, client *api.Client, path string, version int) map[string]interface{} {
	t.Helper()

	readPath := fmt.Sprintf("%s/data/%s", mountPath, path)

	var secret *api.Secret
	var err error
	if version > 0 {
		// Use ReadWithData to pass version as query parameter
		secret, err = client.Logical().ReadWithData(readPath, map[string][]string{
			"version": {fmt.Sprintf("%d", version)},
		})
	} else {
		secret, err = client.Logical().Read(readPath)
	}

	t.Logf("readSecret: attempting to read path=%s version=%d", path, version)
	t.Logf("readSecret: result err=%v, secret=%+v", err, secret)
	if err != nil {
		t.Fatalf("Failed to read secret %s: %v", path, err)
	}
	if secret == nil || secret.Data == nil {
		t.Fatalf("Secret %s not found", path)
	}

	data, ok := secret.Data["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("Invalid secret data format for %s", path)
	}
	return data
}

// Helper to read metadata
func readMetadata(t *testing.T, client *api.Client, path string) map[string]interface{} {
	t.Helper()

	metaPath := fmt.Sprintf("%s/metadata/%s", mountPath, path)
	secret, err := client.Logical().Read(metaPath)
	if err != nil {
		t.Fatalf("Failed to read metadata %s: %v", path, err)
	}
	if secret == nil || secret.Data == nil {
		t.Fatalf("Metadata %s not found", path)
	}
	return secret.Data
}

// Helper to run vault-migrate binary
func runMigrate(t *testing.T, workDir string, args ...string) (string, error) {
	t.Helper()

	// Get absolute path to binary
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}
	binaryPath := filepath.Join(cwd, "..", "..", "vault-migrate")

	// Build binary if not exists
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		buildCmd := exec.Command("go", "build", "-o", binaryPath)
		buildCmd.Dir = filepath.Join(cwd, "..", "..")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("Failed to build binary: %v\n%s", err, out)
		}
	}

	// Run migration with all I/O pipes
	cmd := exec.Command(binaryPath, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}

	// Create pipes
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("Failed to create stdin pipe: %v", err)
	}

	// Capture combined output using bytes.Buffer (thread-safe)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	// Start command
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start command: %v", err)
	}

	// Write input to stdin in a goroutine
	go func() {
		defer stdin.Close()
		input := "n\nsecret\n\nsecret\n\n"
		stdin.Write([]byte(input))
	}()

	// Wait for completion
	cmdErr := cmd.Wait()

	output := outBuf.String()

	// Debug: write output to file for inspection
	debugFile := filepath.Join(cmd.Dir, "migration-output.log")
	os.WriteFile(debugFile, outBuf.Bytes(), 0644)

	t.Logf("Command: %s %v", binaryPath, args)
	t.Logf("Working dir: %s", cmd.Dir)
	t.Logf("Command error: %v", cmdErr)
	t.Logf("Exit code: %d", cmd.ProcessState.ExitCode())
	t.Logf("Output length: %d bytes", len(output))
	t.Logf("Debug log: %s", debugFile)

	return output, cmdErr
}

// Helper to clean destination secrets
func cleanDestination(t *testing.T, client *api.Client) {
	t.Helper()

	// Recursively delete all secrets under the mount
	var deleteRecursive func(string)
	deleteRecursive = func(path string) {
		listPath := fmt.Sprintf("%s/metadata/%s", mountPath, path)
		secret, err := client.Logical().List(listPath)
		if err != nil || secret == nil {
			return
		}

		keys, ok := secret.Data["keys"].([]interface{})
		if !ok {
			return
		}

		for _, k := range keys {
			key := k.(string)
			fullKey := key
			if path != "" {
				fullKey = path + "/" + key
			}

			// If key ends with /, it's a folder - recurse
			if strings.HasSuffix(key, "/") {
				deleteRecursive(strings.TrimSuffix(fullKey, "/"))
			}

			// Delete the secret/metadata
			deletePath := fmt.Sprintf("%s/metadata/%s", mountPath, fullKey)
			client.Logical().Delete(deletePath)
		}
	}

	deleteRecursive("")
}

// Helper to setup test data
func setupTestSecrets(t *testing.T, client *api.Client) {
	t.Helper()

	// Create test secrets with multiple versions
	writeSecret(t, client, "app/db/password", map[string]interface{}{
		"username": "admin",
		"password": "secret123",
	})
	writeSecret(t, client, "app/db/password", map[string]interface{}{
		"username": "admin",
		"password": "secret456",
	})
	writeSecret(t, client, "app/db/password", map[string]interface{}{
		"username": "admin",
		"password": "secret789",
	})

	writeSecret(t, client, "app/api/key", map[string]interface{}{
		"key": "api-key-v1",
	})
	writeSecret(t, client, "app/api/key", map[string]interface{}{
		"key": "api-key-v2",
	})

	writeSecret(t, client, "app/config", map[string]interface{}{
		"host": "localhost",
		"port": 5432,
	})
}

func TestE2E_FullMigration(t *testing.T) {
	srcClient := newVaultClient(t, srcAddr, srcToken)
	dstClient := newVaultClient(t, dstAddr, dstToken)

	// Clean both source and destination before starting
	cleanDestination(t, srcClient)
	cleanDestination(t, dstClient)

	// Schedule cleanup to always run at end, even if test fails
	t.Cleanup(func() {
		cleanDestination(t, dstClient)
		cleanDestination(t, srcClient)
	})

	// Setup: Create test secrets in source
	setupTestSecrets(t, srcClient)

	// Run migration
	workDir := t.TempDir()
	stateFile := filepath.Join(workDir, "state.json")
	output, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)

	if err != nil {
		t.Logf("Migration output:\n%s", output)
		t.Fatalf("Migration failed: %v", err)
	}

	t.Logf("Migration succeeded!")
	t.Logf("Output preview (first 500 chars):\n%s", output[:min(500, len(output))])

	// Verify: Check secrets exist at destination
	dstData := readSecret(t, dstClient, "app/db/password", 0) // Read latest version
	if dstData["password"] != "secret789" {
		t.Errorf("app/db/password latest = %v, want secret789", dstData["password"])
	}

	dstMeta := readMetadata(t, dstClient, "app/db/password")
	currentVersion := toInt(dstMeta["current_version"])
	if currentVersion < 3 {
		t.Errorf("app/db/password version count = %d, want >= 3", currentVersion)
	}

	// Verify state file
	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("Failed to read state file: %v", err)
	}

	var state map[string]interface{}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Failed to parse state file: %v", err)
	}

	summary := state["summary"].(map[string]interface{})
	if toInt(summary["completed"]) != 3 {
		t.Errorf("State summary completed = %v, want 3", summary["completed"])
	}
	if toInt(summary["failed"]) != 0 {
		t.Errorf("State summary failed = %v, want 0", summary["failed"])
	}
}

func TestE2E_IncrementalMigration(t *testing.T) {
	srcClient := newVaultClient(t, srcAddr, srcToken)
	dstClient := newVaultClient(t, dstAddr, dstToken)

	// Clean both source and destination before starting
	cleanDestination(t, srcClient)
	cleanDestination(t, dstClient)

	// Schedule cleanup to always run at end, even if test fails
	t.Cleanup(func() {
		cleanDestination(t, dstClient)
		cleanDestination(t, srcClient)
	})

	// Setup: Create initial secrets
	writeSecret(t, srcClient, "app/secret", map[string]interface{}{
		"value": "v1",
	})
	writeSecret(t, srcClient, "app/secret", map[string]interface{}{
		"value": "v2",
	})

	workDir := t.TempDir()
	stateFile := filepath.Join(workDir, "state.json")

	// First migration
	output1, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)
	if err != nil {
		t.Fatalf("First migration failed: %v\nOutput: %s", err, output1)
	}
	t.Logf("First migration output:\n%s", output1)

	// Add new version at source
	writeSecret(t, srcClient, "app/secret", map[string]interface{}{
		"value": "v3",
	})

	// Verify v3 was written
	srcMeta := readMetadata(t, srcClient, "app/secret")
	srcVersion := toInt(srcMeta["current_version"])
	if srcVersion != 3 {
		t.Fatalf("Source should have 3 versions after writing v3, got %d", srcVersion)
	}

	// Second migration (incremental)
	output2, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)
	if err != nil {
		t.Fatalf("Second migration failed: %v\nOutput: %s", err, output2)
	}
	t.Logf("Second migration output:\n%s", output2)

	// Brief wait for Vault to be consistent
	time.Sleep(100 * time.Millisecond)

	// Debug: Use vault CLI to verify secret exists
	debugCmd := exec.Command("curl", "-s", "http://127.0.0.1:8300/v1/secret/metadata/app/secret", "-H", "X-Vault-Token: root-token-destination")
	debugOut, debugErr := debugCmd.CombinedOutput()
	t.Logf("Debug curl output: %s", string(debugOut))
	if debugErr != nil {
		t.Logf("Debug curl error: %v", debugErr)
	}

	// Debug: verify destination has secret before assertions
	debugMeta, err := dstClient.Logical().Read(fmt.Sprintf("%s/metadata/app/secret", mountPath))
	t.Logf("Debug API client read: err=%v, meta=%+v", err, debugMeta)
	if err != nil || debugMeta == nil {
		t.Fatalf("Debug check: app/secret metadata not found in destination after migration: err=%v, meta=%v", err, debugMeta)
	}
	t.Logf("Debug: Destination metadata: %+v", debugMeta.Data)

	// Verify: Destination has all 3 versions
	dstMeta := readMetadata(t, dstClient, "app/secret")
	currentVersion := toInt(dstMeta["current_version"])
	if currentVersion != 3 {
		t.Errorf("app/secret version count = %d, want 3", currentVersion)
	}

	dstData := readSecret(t, dstClient, "app/secret", 3)
	if dstData["value"] != "v3" {
		t.Errorf("app/secret v3 = %v, want v3", dstData["value"])
	}
}

func TestE2E_DryRun(t *testing.T) {
	srcClient := newVaultClient(t, srcAddr, srcToken)
	dstClient := newVaultClient(t, dstAddr, dstToken)

	// Clean both source and destination before starting
	cleanDestination(t, srcClient)
	cleanDestination(t, dstClient)

	// Schedule cleanup to always run at end, even if test fails
	t.Cleanup(func() {
		cleanDestination(t, dstClient)
		cleanDestination(t, srcClient)
	})

	// Setup
	writeSecret(t, srcClient, "app/dryrun", map[string]interface{}{
		"test": "value",
	})

	// Run dry-run migration
	workDir := t.TempDir()
	output, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-dryRun",
	)
	if err != nil {
		t.Fatalf("Dry-run failed: %v\nOutput: %s", err, output)
	}

	// Verify: Destination should be empty
	readPath := fmt.Sprintf("%s/data/app/dryrun", mountPath)
	secret, err := dstClient.Logical().Read(readPath)
	if err == nil && secret != nil {
		t.Error("Dry-run wrote to destination (should not write)")
	}
}

// setMountCASRequired tunes the destination mount's cas_required option
// (`vault secrets tune -cas-required=true <mount>/`) via the raw sys/mounts
// tune API, restoring it to false via t.Cleanup so it never leaks into a
// later test in the same suite run.
func setMountCASRequired(t *testing.T, client *api.Client, mount string, required bool) {
	t.Helper()

	tunePath := fmt.Sprintf("sys/mounts/%s/tune", mount)
	_, err := client.Logical().Write(tunePath, map[string]interface{}{
		"options": map[string]interface{}{
			"cas_required": fmt.Sprintf("%t", required),
		},
	})
	if err != nil {
		t.Fatalf("failed to tune mount %s cas_required=%t: %v", mount, required, err)
	}
}

// setSecretCASRequired sets a per-secret cas_required
// (`vault kv metadata put -cas-required=true <mount>/<path>`).
func setSecretCASRequired(t *testing.T, client *api.Client, path string, required bool) {
	t.Helper()

	metaPath := fmt.Sprintf("%s/metadata/%s", mountPath, path)
	_, err := client.Logical().Write(metaPath, map[string]interface{}{
		"cas_required": required,
	})
	if err != nil {
		t.Fatalf("failed to set cas_required=%t on %s: %v", required, path, err)
	}
}

// TestE2E_CASRequiredMountDestination is the ONLY real-Vault proof of the
// mount-level OR at path_data.go:286 -- the destination MOUNT is tuned
// cas_required=true (not the per-secret metadata), and a migration into it
// must succeed via B19's reactive CAS retry in kv2WriteData instead of
// failing on version 1 with "check-and-set parameter required for this
// call".
func TestE2E_CASRequiredMountDestination(t *testing.T) {
	srcClient := newVaultClient(t, srcAddr, srcToken)
	dstClient := newVaultClient(t, dstAddr, dstToken)

	cleanDestination(t, srcClient)
	cleanDestination(t, dstClient)
	setMountCASRequired(t, dstClient, mountPath, true)

	t.Cleanup(func() {
		setMountCASRequired(t, dstClient, mountPath, false)
		cleanDestination(t, dstClient)
		cleanDestination(t, srcClient)
	})

	writeSecret(t, srcClient, "app/cas-mount", map[string]interface{}{"value": "v1"})
	writeSecret(t, srcClient, "app/cas-mount", map[string]interface{}{"value": "v2"})
	writeSecret(t, srcClient, "app/cas-mount", map[string]interface{}{"value": "v3"})

	workDir := t.TempDir()
	stateFile := filepath.Join(workDir, "state.json")
	output, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)
	if err != nil {
		t.Fatalf("migration into a cas_required MOUNT failed: %v\nOutput: %s", err, output)
	}

	dstData := readSecret(t, dstClient, "app/cas-mount", 0)
	if dstData["value"] != "v3" {
		t.Errorf("app/cas-mount latest value = %v, want v3", dstData["value"])
	}

	dstMeta := readMetadata(t, dstClient, "app/cas-mount")
	if toInt(dstMeta["current_version"]) != 3 {
		t.Errorf("app/cas-mount current_version = %v, want 3", dstMeta["current_version"])
	}
}

// TestE2E_CASRequiredPerSecretDestination proves the per-secret trigger
// (`vault kv metadata put -cas-required=true`) independent of the mount
// tunable: the destination secret itself carries cas_required=true before
// the first write ever happens, so kv2WriteData's retry must fire on
// version 1.
func TestE2E_CASRequiredPerSecretDestination(t *testing.T) {
	srcClient := newVaultClient(t, srcAddr, srcToken)
	dstClient := newVaultClient(t, dstAddr, dstToken)

	cleanDestination(t, srcClient)
	cleanDestination(t, dstClient)

	t.Cleanup(func() {
		cleanDestination(t, dstClient)
		cleanDestination(t, srcClient)
	})

	writeSecret(t, srcClient, "app/cas-secret", map[string]interface{}{"value": "v1"})
	writeSecret(t, srcClient, "app/cas-secret", map[string]interface{}{"value": "v2"})

	// Pre-create the destination secret with per-secret cas_required=true,
	// exactly as an operator would before a migration lands, with no other
	// destination-side setup.
	setSecretCASRequired(t, dstClient, "app/cas-secret", true)

	workDir := t.TempDir()
	stateFile := filepath.Join(workDir, "state.json")
	output, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)
	if err != nil {
		t.Fatalf("migration into a per-secret cas_required destination failed: %v\nOutput: %s", err, output)
	}

	dstMeta := readMetadata(t, dstClient, "app/cas-secret")
	if toInt(dstMeta["current_version"]) != 2 {
		t.Errorf("app/cas-secret current_version = %v, want 2", dstMeta["current_version"])
	}

	dstData := readSecret(t, dstClient, "app/cas-secret", 2)
	if dstData["value"] != "v2" {
		t.Errorf("app/cas-secret v2 = %v, want v2", dstData["value"])
	}
}

// TestE2E_SourceCASRequiredIncremental locks Task 6's finding: a SOURCE
// secret with cas_required=true self-inflicts B19 onto the destination,
// because kv2WriteMetadataSettings copies cas_required from source metadata
// unconditionally, after the version-write loop. Run 1 succeeds (dest had
// no cas_required yet) and stamps cas_required=true onto the dest as its
// last step; run 2's incremental write must still succeed via B19's fix,
// with zero destination-side operator action anywhere in this test.
func TestE2E_SourceCASRequiredIncremental(t *testing.T) {
	srcClient := newVaultClient(t, srcAddr, srcToken)
	dstClient := newVaultClient(t, dstAddr, dstToken)

	cleanDestination(t, srcClient)
	cleanDestination(t, dstClient)

	t.Cleanup(func() {
		cleanDestination(t, dstClient)
		cleanDestination(t, srcClient)
	})

	writeSecret(t, srcClient, "app/src-cas", map[string]interface{}{"value": "v1"})
	setSecretCASRequired(t, srcClient, "app/src-cas", true)

	workDir := t.TempDir()
	stateFile := filepath.Join(workDir, "state.json")

	output1, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)
	if err != nil {
		t.Fatalf("run 1 failed: %v\nOutput: %s", err, output1)
	}

	// Destination now has cas_required=true stamped on it by run 1's own
	// kv2WriteMetadataSettings call -- no operator action, no explicit
	// destination tuning anywhere in this test.
	dstMetaAfterRun1 := readMetadata(t, dstClient, "app/src-cas")
	if casReq, _ := dstMetaAfterRun1["cas_required"].(bool); !casReq {
		t.Fatalf("expected destination cas_required=true after run 1 (source-side stamp), got %v", dstMetaAfterRun1["cas_required"])
	}

	// The SOURCE secret itself now requires check-and-set too (we set it
	// above so kv2WriteMetadataSettings has something to copy), so this
	// test's own write to source must supply cas -- unrelated to the
	// product code path under test, which is the migration's write to the
	// DESTINATION.
	srcMeta := readMetadata(t, srcClient, "app/src-cas")
	writePath := fmt.Sprintf("%s/data/app/src-cas", mountPath)
	if _, err := srcClient.Logical().Write(writePath, map[string]interface{}{
		"data":    map[string]interface{}{"value": "v2"},
		"options": map[string]interface{}{"cas": toInt(srcMeta["current_version"])},
	}); err != nil {
		t.Fatalf("failed to write v2 to cas_required source secret: %v", err)
	}

	output2, err := runMigrate(t, workDir,
		"-srcAddr", srcAddr,
		"-srcToken", srcToken,
		"-srcNamespace", "",
		"-dstAddr", dstAddr,
		"-dstToken", dstToken,
		"-dstNamespace", "",
		"-stateFile", stateFile,
		"-logLevel", "debug",
	)
	if err != nil {
		t.Fatalf("run 2 (incremental, self-inflicted cas_required) failed: %v\nOutput: %s", err, output2)
	}

	dstData := readSecret(t, dstClient, "app/src-cas", 2)
	if dstData["value"] != "v2" {
		t.Errorf("app/src-cas v2 = %v, want v2", dstData["value"])
	}
}
