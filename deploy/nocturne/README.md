# Nocturne 2.5.6 compatibility image

This overlay builds only upstream commit
`54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254` from its SHA-256 verified source
archive. Generated source contexts, test environments, and OCI layouts live under
the ignored `output/` directory. The fetch command accepts only marked child
directories below that root; it rejects the output root itself, repository paths,
external absolute paths, unmarked existing directories, and symlink components.

## Supply-chain locks

`supply-chain.lock.json` is the input lock. Its enforced boundary is the source
archive hash, the `linux/amd64` base platform manifest, the pinned BuildKit image
digest and version, the provisioned Buildx binary URL/hash/version, a signed
Debian snapshot with exact explicitly installed package versions, and hash-locked
Python build/runtime requirements. Debian Release signatures are checked by apt
against the keyring in the pinned base image; unsigned repositories and
`trusted=yes` are forbidden.

`image.lock.json` is the output lock. It is bound to the exact input-lock hash and
records the offline OCI index hash, platform manifest digest, and config digest.
`registry_digest` is intentionally `null`: this workflow does not push or derive
a registry manifest. A `null` value is not a deployable registry claim and must
never be substituted for the locked `platform_manifest_digest`; registry
publication requires a separate verified registry digest record.

`scripts/build-oci.sh` downloads and SHA-256 verifies Buildx into an isolated
`DOCKER_CONFIG`, creates a temporary docker-container builder from the locked
BuildKit image digest, verifies its driver image and reported BuildKit version,
builds the OCI layout, checks every OCI blob, and requires exact equality with
`image.lock.json` before succeeding.

Run:

```sh
deploy/nocturne/scripts/run-contract-tests.sh
deploy/nocturne/scripts/tool.py verify-lock
deploy/nocturne/scripts/build-oci.sh
deploy/nocturne/scripts/tool.py verify-oci
```

## Managed backup protocol

`edu-agent-maintenance-v1` provides reference enumeration, node-scoped review
reference cleanup, and managed backup inventory/prune. The fixed backup root may
contain only these control files and flat artifact filenames:

- `.edu-agent-backup.lock`
- `managed-inventory.json`
- artifact names listed exactly once in the manifest

Nested paths, hard links, symlinks, directories, missing artifacts, and any
manifest-external entry make inventory and prune fail closed with HTTP 409. A
missing manifest is valid only when the root contains no non-control entry.
Prune never deletes an unregistered name.

The backup producer and maintenance extension share this publication protocol:

1. Open `.edu-agent-backup.lock` without following symlinks and hold an exclusive
   `flock` across artifact creation and manifest publication.
2. Create a new, globally unique flat artifact filename. Published filenames and
   bytes are immutable and must never be reused, replaced, or modified.
3. Publish records matching `backup-inventory.schema.json`. Before replacing the
   manifest, compare the previously read manifest's device, inode, size, and
   SHA-256 (or confirm it is still absent); abort on any mismatch.
4. Write and fsync a same-directory temporary manifest, repeat the CAS and root
   identity checks, atomically replace `managed-inventory.json`, fsync the backup
   root, then release the lock.

Inventory uses a shared lock. Prune uses an exclusive lock, rereads the manifest
inside that lock, opens artifacts relative to the backup-root dirfd with
`O_NOFOLLOW`, and immediately rechecks device, inode, size, and SHA-256 before
`unlinkat`. Parent/root identity and manifest CAS failures abort the operation.

The Go backup controller now streams `pg_dump` directly into chunked AES-GCM
envelope encryption, atomically publishes ciphertext plus strict inventory,
stores wrapped generation keys in PostgreSQL, verifies restore while the key is
live, and proves restore failure after privacy-barrier key destruction. No
plaintext dump is written to the persistent backup volume. The maintenance
overlay validates and prunes the same locked inventory; migration startup accepts
only a fresh encrypted artifact bound to the current upgrade window.

Compose mounts three independent named volumes: `nocturne-postgres-data` for the
Nocturne database, `nocturne-backups` for encrypted managed backups, and
`nocturne-snapshots` for `/app/snapshots`. The Nocturne service remains exposed
only on the internal Compose network.

## Rollback

There is no supported down migration. Never point an old Nocturne container at
an upgraded database or treat a container-only downgrade as rollback. Use the
local `edu-agentd nocturne-backup restore` operator command through the supported
new-volume procedure in [ROLLBACK.md](ROLLBACK.md). It verifies the DB inventory,
live generation key, manifest/hash, encrypted stream, tmpfs destination, and
mode `0600` without printing keys or restored content.

## Repeatable Compose gate

`contracttests/nocturne/run-compose-e2e.sh` consumes an already verified offline
OCI layout, imports it without rebuilding, creates temporary secrets and an
isolated Compose project, and automatically covers the A88/A89 real-container
contract plus the A84 same-digest new-volume rollback preparation. The gate uses
real PostgreSQL state, the real restore command, real expiry/Purger workers, and
a real encrypted artifact. Same-digest rollback evidence does not establish
cross-version schema compatibility; full release qualification still requires
two independently locked releases. See `contracttests/nocturne/COMPOSE_E2E.md`
for prerequisites and the single command. The gate always removes its project,
rollback containers and labeled external volumes, imported tag, and temporary
secret directory.
