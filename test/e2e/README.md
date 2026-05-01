# End-to-End Tests

E2E tests for vault-migrate using Docker Vault instances.

## Prerequisites

- Docker and Docker Compose installed
- Go 1.25.5+

## Running E2E Tests

**Manual approach (recommended):**
```bash
# 1. Start Vault containers
docker-compose up -d

# 2. Wait for containers to be ready
sleep 5

# 3. Build binary
cd ../..
go build -o vault-migrate

# 4. Run tests (from e2e directory)
cd test/e2e
E2E_TESTS=1 go test -v -timeout=5m .

# 5. Cleanup
docker-compose down -v
```

**Quick run script:**
```bash
./run-e2e.sh
```
*Note: Automated script has container lifecycle issues, use manual approach if script fails.*

## Test Coverage

E2E tests cover:
- ✅ **Full migration** - Copy all secrets with multiple versions
- ✅ **Incremental migration** - Add new versions and re-run
- ✅ **Dry-run mode** - Verify no writes to destination

## Architecture

- **Source Vault**: `http://127.0.0.1:8200` (token: `root-token-source`)
- **Destination Vault**: `http://127.0.0.1:8300` (token: `root-token-destination`)
- **Mount**: `secret` (KV v2)

## Troubleshooting

**Containers not starting:**
```bash
docker-compose logs
```

**Port conflicts:**
Edit `docker-compose.yml` to use different ports.

**Tests fail to connect:**
Ensure containers are healthy:
```bash
docker-compose ps
```

**Cleanup stuck containers:**
```bash
docker-compose down -v --remove-orphans
```
