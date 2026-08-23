# Nocturne rollback

Nocturne schema migrations have no supported down migration. Rolling a container
back against the upgraded database is forbidden: the old image may start, mutate
an incompatible schema, or appear healthy while serving corrupted state. A
container-only downgrade is not a rollback. A rollback must restore a
pre-upgrade encrypted managed backup into a new database
volume and start the old locked image only against that restored database.

`deploy/nocturne/scripts/rollback.sh` implements that sequence. It never removes
volumes and intentionally leaves the bridge writer stopped after validation.

## Required evidence

Collect these values from the old release before beginning:

- A canonical registry reference in `repository@sha256:<64 lowercase hex>` form.
  Tags, local names, and floating references are rejected.
- The old release platform and image config digest. The script pulls the
  canonical reference, verifies both values with `docker image inspect`, and
  records the platform manifest and config digests in a mode `0600` JSON record.
- The latest expected old `schema_migrations.version` filename.
- A seed node path, a search query that finds it, and the SHA-256 of its exact
  UTF-8 content. This proves the restored database contains pre-upgrade data.
- A managed encrypted backup artifact path present in both
  `managed-inventory.json` and `memory_managed_backup_inventory` with a live
  generation key.
- Two new, unused named-volume names for the rollback database and snapshots.
  The database target must never be the original Nocturne database volume.

The existing Compose project and its main PostgreSQL database must be running so
the operator command can read the managed inventory and unwrap the generation
key. The rollback record directory must be private and on persistent storage.

## Execute

Run from the repository root. Replace every example value with release evidence:

```sh
deploy/nocturne/scripts/rollback.sh \
  --env-file /absolute/private/path/edu-agent.env \
  --project edu-agent \
  --artifact managed-g00000000000000000001-example.backup.enc \
  --old-image registry.example/edu-agent/nocturne@sha256:<old-platform-manifest> \
  --expected-platform linux/amd64 \
  --expected-config-digest sha256:<old-config-digest> \
  --expected-upstream-commit <old-40-character-commit> \
  --expected-compat-revision <old-compat-revision> \
  --expected-schema-version <old-latest-migration.py> \
  --target-volume edu-agent-nocturne-rollback-20260910 \
  --target-snapshot-volume edu-agent-nocturne-rollback-snapshots-20260910 \
  --seed-path edu-agent/<known-seed-title> \
  --seed-search-query '<known seed search text>' \
  --seed-content-sha256 <sha256-of-exact-seed-content> \
  --record /absolute/private/path/rollback-20260910.json
```

The script performs these ordered checks and actions:

1. Resolve the original Nocturne database mount and reject either target volume
   if it is the original volume. Any already existing target volume is rejected.
2. Pull the old canonical image, verify platform/config digest, and create the
   rollback record without storing tokens, passwords, keys, or restored content.
3. Stop `server` before restoration so the bridge writer cannot create new
   Nocturne writes. Stop the current `nocturne` container.
4. Run `edu-agentd nocturne-backup restore` inside a one-off server container
   with `/run/edu-agent-rollback` mounted as tmpfs. The command checks the main
   DB migration set, DB inventory, filesystem manifest/hash, wrapped generation
   key, ciphertext AEAD/terminator/EOF, and output mode `0600`. A destroyed key
   is a hard failure before a target volume is created.
5. Create new labeled database and snapshot volumes. Stop the original
   `nocturne-postgres`, start the isolated rollback PostgreSQL service, verify
   the actual mount source, and require zero application relations in `public`.
6. Restore the artifact again to tmpfs and run `pg_restore --exit-on-error` into
   the new empty database. Plaintext is removed when the one-off container exits
   and is never written to the persistent backup or host filesystem.
7. Start the old canonical Nocturne image with its database URL fixed to the new
   rollback service and its snapshots fixed to the new snapshot volume. The old
   image is not started against the original database.
8. Verify the running container config digest, health, capability commit and
   compatibility revision, exact schema migration version, restored seed read
   and search, complete seed references, and a temporary CRUD/search/references
   cycle.

Success writes `status: validated`. The bridge writer remains stopped pending an
operator decision. Resume it only by deploying an application release whose
Nocturne image contract matches the old digest and whose main database schema is
known to be compatible. Starting the current writer without that release-level
check is not part of rollback.

## Failure isolation

Any failure after writer shutdown stops the rollback Nocturne and rollback
PostgreSQL services. The original database is not modified, and any new target
volumes are retained with `edu-agent.nocturne.rollback=true`. The record is
updated to `failed-isolated` when possible. Do not point any writer at a failed
target.

Use the recorded Compose override to inspect an isolated target. Export the old
image reference only for the command invocation; do not replace the normal
`.env` image value:

```sh
NOCTURNE_ROLLBACK_IMAGE='registry.example/edu-agent/nocturne@sha256:<old-platform-manifest>' \
  docker compose -f deploy/compose.yaml \
  -f /absolute/private/path/rollback-20260910.compose.yaml \
  --env-file /absolute/private/path/edu-agent.env \
  -p edu-agent ps
```

Volume deletion is always manual and only after evidence capture and explicit
confirmation. This example refuses any confirmation other than the exact volume
names:

```sh
TARGET_DB=edu-agent-nocturne-rollback-20260910
TARGET_SNAPSHOTS=edu-agent-nocturne-rollback-snapshots-20260910
printf 'Type DELETE %s %s to remove the isolated rollback volumes: ' "$TARGET_DB" "$TARGET_SNAPSHOTS" >&2
IFS= read -r confirmation
[ "$confirmation" = "DELETE $TARGET_DB $TARGET_SNAPSHOTS" ] || exit 1
docker volume rm -- "$TARGET_DB" "$TARGET_SNAPSHOTS"
```

Never delete the original database volume as part of rollback cleanup.

## A84 same-digest rehearsal boundary

The current real-Compose gate invokes this entry point with an actual encrypted
artifact and real volumes. Because only one independently locked Nocturne release
is available in this change, it treats the running database as the
forward-failed source and uses the same locked digest as the old image.

The rehearsal creates a known seed, waits for a later encrypted artifact, stops
the bridge writer, restores into a newly created empty PostgreSQL volume, and
starts the locked image only against that restored volume. It verifies image and
config digests, the new mount source, schema version, seed read/search/references,
and a temporary CRUD/search/references cycle. It also invokes the same entry
point against a floating image, the original database volume, a pre-existing
non-empty target, and an artifact whose generation key was destroyed. Every
negative case must fail closed; the destroyed-key case must fail before either
target volume is created.

This evidence proves the operator entry point, actual artifact decryption,
tmpfs-only plaintext, new-volume isolation, old-image pinning, restored data, and
failure boundaries. It does not prove that two different Nocturne versions have
compatible forward and rollback migrations. Full A84 release qualification must
repeat the sequence with independently locked pre-upgrade and failed-forward
releases, then make the application writer reactivation decision at the old
release boundary.
