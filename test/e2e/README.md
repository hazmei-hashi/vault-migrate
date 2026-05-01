# End-to-End Tests

E2E suite runs `vault-migrate` against real Vault dev servers in Docker.

## What It Covers

- Full migration of multi-version secrets
- Incremental migration after new source versions
- Dry-run mode (no destination writes)

## Test Topology

Defined in `test/e2e/docker-compose.yml`:

- Source Vault: `http://127.0.0.1:8200` (`root-token-source`)
- Destination Vault: `http://127.0.0.1:8300` (`root-token-destination`)
- Mount under test: `secret` (KV v2)
- Vault image: `hashicorp/vault:1.18.5`

## Prerequisites

- Docker
- Docker Compose (`docker-compose` or `docker compose`)
- Go 1.25.5+

## Run Tests (Manual)

From repository root:

```bash
docker-compose -f test/e2e/docker-compose.yml up -d
E2E_TESTS=1 go test -v -timeout=5m ./test/e2e
docker-compose -f test/e2e/docker-compose.yml down -v
```

Notes:

- E2E tests are skipped unless `E2E_TESTS=1`.
- Tests assume Vault containers are already running.
- Each test uses a temp working directory and state file (`state.json`).

## Run Helper Script

From `test/e2e`:

```bash
./run-e2e.sh
```

`run-e2e.sh` builds binary and runs tests, but does not start/stop Docker containers.

## Troubleshooting

Containers unhealthy or not reachable:

```bash
docker-compose -f test/e2e/docker-compose.yml ps
docker-compose -f test/e2e/docker-compose.yml logs
```

Port conflicts (8200/8300):

- Adjust host ports in `test/e2e/docker-compose.yml`.
- Update constants in `test/e2e/e2e_test.go` if ports change.

Cleanup:

```bash
docker-compose -f test/e2e/docker-compose.yml down -v --remove-orphans
```
