# vault-migrate

`vault-migrate` is a utility for migrating (copying) HashiCorp Vault KV v2 secrets between clusters or namespaces.

It is designed to operate one secret at a time, so memory usage stays bounded even for very large KV trees. Secrets are never written to storage.

## Features

- Recursive walk of a KV v2 tree under a configurable base path
- Replays every version in order to preserve version numbers
- Mirrors deleted and destroyed versions
- Copies KV v2 metadata settings and custom metadata
- Works across Vault Enterprise namespaces
- Can migrate entire mounts or a subtree under a given mount to a destination mount or subtree.
- Supports “best-effort” mode to continue past unreadable or destroyed versions

Paths are always relative to the mount and must not include data/, metadata/, or the mount name (the latter is supplied separately).

## Limitations

- Destroyed versions cannot be recovered.
- Source version timestamps can not be reflected on the destination.
- Requires Vault tokens for the source and destination clusters that have attached policies capable of performing the intended actions.
- Tokens are not renewed, so TTLs must meet or exceed the utility's run duration.
- Designed for KV v2 only.

## Usage

```bash
  -dstAddr string
        Destination cluster API address (default "https://localhost:8300")
  -dstNamespace string
        Destination cluster namespace
  -dstToken string
        Destination cluster token
  -logLevel string
        Log level (info or debug) (default "info")
  -mode string
        Mode of operation (default "kvv2")
  -srcAddr string
        Source cluster API address (default "https://localhost:8200")
  -srcNamespace string
        Source cluster namespace
  -srcToken string
        Source cluster token
  -tlsSkipVerify
        Skip TLS verification of the Vault server certificates
```