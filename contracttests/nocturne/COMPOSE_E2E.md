# Nocturne Compose E2E gate

Run the isolated A88/A89 deployment gate with an OCI layout that already passed the locked build:

```sh
contracttests/nocturne/run-compose-e2e.sh /absolute/path/to/deploy/nocturne/output/oci-layout
```

The command verifies `supply-chain.lock.json` and every OCI blob against `image.lock.json`, imports the offline image with `skopeo`, creates an isolated Compose project and volumes, generates temporary credentials in a mode-0700 directory, and always removes containers, volumes, the imported tag, and temporary secrets through an EXIT/HUP/INT/TERM trap.

The versioned driver checks all four service health states, bridge and maintenance authorization, the negative route allowlist, and real PostgreSQL-backed Nocturne behavior. The optional `replay` scenario isolates outage recovery: one delivery must converge automatically after Nocturne restarts, while another remains down until the Outbox reaches `dead`, rejects premature replay with `409`, and converges after one explicit replay. Run it with:

```sh
contracttests/nocturne/run-compose-e2e.sh /absolute/path/to/deploy/nocturne/output/oci-layout replay
```

A88 creates history plus a second namespace/path/search alias with a controlled SQL fixture, proves the namespace header is not an authorization boundary and maintenance enumeration is global, unlinks every path, permanently deletes every history ID, and directly verifies zero residual rows in `nodes`, `memories`, `edges`, `paths`, `glossary_keywords`, `search_documents`, and `memory_access_logs`.

A89 uses short delivery/sweep/reconciliation intervals to install a legal sent/unknown attempt after a real remote write. The real expiry worker cancels and scrubs the Outbox, creates a hash/URI-only reconciliation, and the real Purger removes paths, history, search, references, global orphans, and known IDs before the delivery converges to `expired`. The gate also restores the actual encrypted Compose artifact to tmpfs and checks its plaintext begins with `PGDMP`, destroys the generation key through privacy erasure, proves the same artifact remains present but restore fails closed, then waits for ordinary retention prune to remove it.

A84 builds a checked-in failed-forward fixture from the locked old image, publishes it to the isolated registry under its own platform-manifest digest, and records a distinct config digest plus the SHA-256 of the fixture inputs. That forward image transactionally renames the live `nodes` table, replaces live memory/search content with an incompatible forward representation, records its base and fixture digests, and exits as a failed release. The gate proves the old locked image cannot serve the upgraded database, then invokes the supported rollback entry point with the actual pre-upgrade encrypted artifact and new empty database/snapshot volumes. The old image must recover the seed read/search/references and temporary CRUD from the restored volume. The gate then restores the ephemeral original test database from the same encrypted artifact before continuing. Floating images, the original volume, a pre-existing non-empty target, and destroyed keys remain fail-closed. Account isolation, internal-only networking, Nocturne-down/restart degradation, teaching continuity, automatic queued recovery, dead-delivery replay, and SIGTERM worker shutdown remain covered.

Prerequisites are Docker with Compose v2, `skopeo`, Python 3, and Go. The gate intentionally creates its own project name, host port, secrets, and volumes. It must not be pointed at a shared Compose project or database.
