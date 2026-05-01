#!/bin/bash
set -e

cd "$(dirname "$0")"

echo "Starting E2E tests..."

# Build binary
echo "Building vault-migrate binary..."
(cd ../.. && go build -o vault-migrate)

# Run E2E tests
echo "Running E2E tests with Docker Vault..."
E2E_TESTS=1 go test -v -timeout=5m .

echo "E2E tests completed successfully!"
