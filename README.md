# vault-migrate

`vault-migrate` is a utility for migrating (copying) HashiCorp Vault KV v2 secrets between clusters or namespaces with **intelligent state tracking and incremental migration support**.

It is designed to operate one secret at a time, so memory usage stays bounded even for very large KV trees. Secrets are never written to storage.

## Installation

**Prerequisites:**
- Go 1.25.5 or later
- Network access to source and destination Vault clusters
- Valid Vault tokens with appropriate permissions (see Token Requirements below)

**Build from source:**
```bash
git clone <repository-url>
cd vault-migrate
go build
```

This produces a `vault-migrate` binary in the current directory.

## Features

### Core Capabilities
- Recursive walk of a KV v2 tree under a configurable base path
- Replays every version in order to preserve version numbers
- Mirrors deleted and destroyed versions
- Copies KV v2 metadata settings and custom metadata
- Works across Vault Enterprise namespaces
- Can migrate entire mounts or a subtree under a given mount to a destination mount or subtree

### State Tracking & Incremental Migration (NEW)
- **Intelligent state tracking**: Tracks migration progress in a local JSON file
- **Incremental copy**: Only copies new versions that don't exist at destination
- **Resume capability**: Automatically retries failed secrets on re-run
- **Hash verification**: Uses SHA256 hashes to detect changes without storing secret values
- **Progress visibility**: Real-time progress logging (completed/failed/skipped counts)
- **Safe re-runs**: Skips already-migrated secrets to avoid duplication

Paths are always relative to the mount and must not include data/, metadata/, or the mount name (the latter is supplied separately).

## Token Requirements

Both source and destination tokens require policies that allow:

**Source token:**
- `read` permission on `<mount>/metadata/*` (to list and read metadata)
- `read` permission on `<mount>/data/*` (to read secret versions)

**Destination token:**
- `create` and `update` permissions on `<mount>/metadata/*` (to write metadata)
- `create` and `update` permissions on `<mount>/data/*` (to write secret data)
- `delete` permission on `<mount>/metadata/*` and `<mount>/data/*` (to mirror deleted versions and recreate on hash mismatch)
- `destroy` permission on `<mount>/metadata/*` (to mirror destroyed versions, if applicable)

## Limitations

- Destroyed versions cannot be recovered (but their destroyed state is mirrored)
- Source version timestamps cannot be reflected on the destination
- Requires Vault tokens for the source and destination clusters that have attached policies capable of performing the intended actions
- Tokens are not renewed, so TTLs must meet or exceed the utility's run duration
- Designed for KV v2 only
- State file concurrent access is not supported (use different `-stateFile` paths for parallel migrations)

## Usage

### Quick Start

**Basic migration with state tracking (recommended):**
```bash
./vault-migrate \
  -srcAddr https://vault-source.example.com:8200 \
  -srcToken hvs.CAESIGFb... \
  -srcNamespace admin \
  -dstAddr https://vault-dest.example.com:8200 \
  -dstToken hvs.CAESIH9w... \
  -dstNamespace admin
```

The tool will prompt for mount paths and base paths, then:
- Create `.vault-migrate-state.json` to track progress
- Copy all secrets incrementally
- Show progress every 10 secrets
- Skip already-migrated secrets on re-run

**Resume a failed migration:**
```bash
# Just re-run the same command - it will automatically:
# - Load existing state file
# - Skip completed secrets
# - Retry failed secrets
# - Copy any new versions added since last run
./vault-migrate -srcAddr https://vault-source.example.com:8200 ...
```

### Command-Line Flags

```bash
  -srcAddr string
        Source cluster API address (default "https://localhost:8200")
  -srcToken string
        Source cluster token
  -srcNamespace string
        Source cluster namespace
  -dstAddr string
        Destination cluster API address (default "https://localhost:8300")
  -dstToken string
        Destination cluster token
  -dstNamespace string
        Destination cluster namespace
  -tlsSkipVerify
        Skip TLS verification of the Vault server certificates
  -logLevel string
        Log level (info or debug) (default "info")
  -mode string
        Mode of operation (default "kvv2")
  -stateFile string
        Path to state file for tracking migration progress (default ".vault-migrate-state.json")
  -noState
        Disable state tracking (legacy mode)
  -forceRecopy
        Re-copy secrets even if hashes match
  -maxRetries int
        Maximum retry attempts for failed secrets (default 3)
```

### Running with Flags

**Standard migration with state tracking:**
```bash
./vault-migrate \
  -srcAddr https://vault-source.example.com:8200 \
  -srcToken hvs.CAESIGFb... \
  -srcNamespace admin \
  -dstAddr https://vault-dest.example.com:8200 \
  -dstToken hvs.CAESIH9w... \
  -dstNamespace admin \
  -logLevel info
```

**Advanced options:**
```bash
# Custom state file location
./vault-migrate -stateFile /path/to/migration-state.json ...

# Force re-copy even if secrets haven't changed
./vault-migrate -forceRecopy ...

# Disable state tracking (legacy behavior)
./vault-migrate -noState ...

# Change max retry attempts for failed secrets
./vault-migrate -maxRetries 5 ...

# Skip TLS verification for development
./vault-migrate -tlsSkipVerify ...

# Enable debug logging
./vault-migrate -logLevel debug ...
```

### Interactive Mode

Run without flags and the tool will prompt for missing values:

```bash
./vault-migrate
```

Example prompts:
```
Source Vault API address: https://vault-source.example.com:8200
Source Vault token: [hidden input]
Source namespace: admin
Destination Vault API address: https://vault-dest.example.com:8200
Destination Vault token: [hidden input]
Destination namespace: admin
Skip TLS verification? (y/n): y
Source KV-V2 mount: secret
Source KV-V2 base path: myapp/
Destination KV-V2 mount: secret
Destination KV-V2 base path: myapp-migrated/
```

**Note:** Tokens are read securely with hidden input. Mount and base paths are **relative to the mount** and should not include `data/`, `metadata/`, or the mount name.

## Behavior and Re-run Considerations

### State Tracking (NEW)

**As of the latest version**, the tool now tracks migration progress in a local state file (`.vault-migrate-state.json` by default). This enables:

- **Incremental migrations**: Only copy new versions that don't exist at the destination
- **Resume capability**: Automatically retry failed secrets on re-run
- **Skip already-migrated secrets**: Avoid redundant copying using SHA256 hash verification
- **Progress visibility**: See what was migrated, failed, or skipped

#### State File Contents

The state file stores (NO actual secret values are saved):
- SHA256 hashes of each version's payload for comparison
- Version states (active/deleted/destroyed)
- Metadata checksums
- Migration status per secret (completed/failed/skipped)
- Timestamps and retry counts

**Example state file structure:**
```json
{
  "version": "1.0",
  "migration_id": "migration_2026-04-30T10:30:00Z",
  "source": {
    "address": "https://vault-src:8200",
    "namespace": "admin",
    "mount": "secret",
    "base_path": "myapp/"
  },
  "destination": {
    "address": "https://vault-dst:8200",
    "namespace": "admin",
    "mount": "secret",
    "base_path": "myapp-migrated/"
  },
  "secrets": {
    "myapp/db/password": {
      "status": "completed",
      "source_version_count": 5,
      "dest_version_count": 5,
      "version_hashes": {
        "1": "sha256:abc123...",
        "2": "sha256:def456..."
      },
      "version_states": {
        "1": "active",
        "2": "deleted"
      },
      "metadata_checksum": "sha256:xyz789...",
      "migrated_at": "2026-04-30T10:31:15Z"
    }
  },
  "summary": {
    "total": 250,
    "completed": 248,
    "failed": 1,
    "skipped": 1,
    "started_at": "2026-04-30T10:30:00Z",
    "last_updated_at": "2026-04-30T10:45:00Z"
  }
}
```

#### Migration Behavior with State Tracking

When state tracking is enabled (default):

**1. New Secret (destination doesn't exist)**
- Performs full copy of all versions
- Records hashes and states in state file

**2. Destination has fewer versions than source**
- Compares existing version hashes
- If hashes match: Copies only NEW versions incrementally (e.g., if source has v1-v7 and dest has v1-v5, only copies v6-v7)
- If hashes mismatch: Destroys destination and performs full copy from source

**3. Same version count**
- Compares all version hashes
- If all match: Skips (already migrated)
- If any differ: Destroys destination and performs full copy from source

**4. Destination ahead of source**
- Logs error (manual review needed)
- Skips the secret

#### Disabling State Tracking

To use legacy behavior (unconditional overwrite):

```bash
./vault-migrate -noState ...
```

#### Multiple Migrations

**IMPORTANT**: Running multiple migrations with the same state file is unsupported. Use different `-stateFile` paths for parallel or separate migrations:

```bash
./vault-migrate -stateFile .state-migration-1.json ...
./vault-migrate -stateFile .state-migration-2.json ...
```

### Overwrite Behavior (Legacy Mode)

When state tracking is disabled with `-noState`:

**WARNING:** This tool does NOT check if secrets already exist at the destination. It will **unconditionally overwrite** any existing secrets at the destination path.

- Running the tool multiple times will re-migrate all secrets
- Existing secrets at the destination with the same path will be overwritten
- This is **not idempotent** - each run creates new versions

### Version Handling

#### With State Tracking (Default)

The tool intelligently handles versions based on what exists at the destination:

- **Incremental copy**: If destination has v1-v5 and source has v1-v7, only copies v6-v7
- **Version preservation**: Version numbers always match between source and destination
- **Hash verification**: Compares SHA256 hashes to detect if existing versions match

#### Without State Tracking (Legacy Mode with `-noState`)

The tool replays every version from the source in sequential order (v1, v2, v3, etc.). 

**If running on an existing destination:**
- Each write creates a **new version** at the destination
- Version numbers will NOT match the source if the destination secret already exists
- Example: If destination already has v1-v5, a re-run will create v6-v10 (not replace v1-v5)

**For a fresh destination:**
- Version numbers will match the source (v1 at source = v1 at destination)

### Metadata Settings

The tool overwrites the destination's metadata settings with those from the source:
- `cas_required` - Check-and-Set requirement
- `max_versions` - Maximum number of versions to keep
- `delete_version_after` - Automatic deletion duration
- `custom_metadata` - User-defined metadata fields

Any existing metadata configuration at the destination will be replaced.

### Re-running the Tool

#### With State Tracking (Default)

**✅ Safe to re-run**: The tool automatically:
- Skips secrets that haven't changed (hash verification)
- Copies only new versions incrementally
- Retries failed secrets (up to `-maxRetries` attempts)
- Shows progress summary (completed/failed/skipped counts)

**Common scenarios:**
- **Network failure mid-migration**: Re-run to resume from state file
- **Source updated after migration**: Re-run to copy new versions incrementally
- **Failed secrets**: Automatically retried on re-run

**Force re-copy**: Use `-forceRecopy` to re-migrate even if hashes match

#### Without State Tracking (Legacy Mode)

**⚠️ Warning:** Re-running the migration on the same source/destination is generally **not recommended** because:

1. It will create duplicate versions at the destination
2. It wastes API calls and time re-migrating already-copied secrets
3. It may cause confusion about which versions correspond to source versions

**When to re-run:**
- After a failed migration (if some secrets were not copied)
- When migrating to a different destination base path
- When source secrets have been updated and you want to capture new versions

**Recommendation:** Use different destination base paths for different migration runs, or manually clean up the destination before re-running.