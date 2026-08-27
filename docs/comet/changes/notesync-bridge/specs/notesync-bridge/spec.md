# Fast Note Sync / Obsidian bridge specification

## Authority and compatibility

`notesync-bridge` publishes accepted canonical knowledge revisions to Fast Note Sync and imports Obsidian changes only through explicit review. PostgreSQL remains authoritative for KnowledgeRevision, stable document/node identity, publication order, review decisions and Outbox. The supported contract is Fast Note Sync Service `3.6.1` at commit `7a6c78792c631f999c8a5f725bba5dd7235d6688` with Obsidian plugin `2.4.0` at commit `f2b15c09d34e621d2d97ad526fdee03460bac151`; production uses the actual `GET /api/version`, `GET /api/health`, `GET /api/vault`, `GET /api/note`, `GET /api/notes` and `POST /api/note` routes with `Authorization: Bearer <token>`, `X-Client: CLI`, and `p:rest c:CLI f:note_r,note_w` plus configured vault restriction. Detailed upstream source evidence and the capability matrix are maintained in `docs/design/notesync-bridge.md`.

## Fail-closed outbound publication

A successful canonical commit atomically enqueues one monotonic, generation-bound Outbox intent per changed stable document, while remote I/O runs after commit. The consumer suppresses stale revision/generation messages, uses create-only for an absent first target, and updates only after exact remote content matches the durable publication base; observed drift, deletion or collision creates review instead of overwriting. Every mutation requires exact readback, and response loss is reconciled by GET before applied/retry/conflict is decided. Fast Note Sync `3.6.1` has no atomic expected-version/CAS, so the upstream GET/POST concurrency window remains a documented limit rather than a false guarantee.

## Explicit inbound review

A bounded user-invoked preview reads real remote notes and freezes base, current canonical local and remote snapshots with source revision, stable identity, SHA-256 and basis hash. Resolution rechecks generation, local head and remote content, rejects stale basis, and allows only accept-remote, keep-canonical or supplied merged Markdown. Accept/merge enter the existing knowledge import with expected parent and identity review to create a new immutable revision; no remote file, path, title, mtime, version or content hash can directly change canonical knowledge.

## Public and privacy boundary

Closed HTTP/OpenAPI and online Go CLI surfaces expose status, preview, review and resolution through existing device authentication, knowledge scopes, limits, audit and generation-stamped read permits; the CLI never receives the Fast Note Sync token or calls the upstream service directly. Publication mapping and review text belong to the knowledge privacy owner, Outbox obeys the generation fence, and old work cannot restore redacted content. Fast Note Sync remains an external copy whose vault, history and backups are not falsely included in the server's physical-erasure receipt. Dependency failure degrades only the notesync component and never blocks canonical knowledge, teaching or queries.

## Delivery and verification

Delivery proceeds serially through S1 pinned client/capability, S2 outbound publication, S3 explicit import/review and S4 candidate integration. Unit and httptest fixtures may falsify local parsing and state logic but cannot establish upstream compatibility; the stable candidate must run once against real Fast Note Sync Service `3.6.1` and verify authentication, vault scope, create-only, read/write/readback, stale Outbox suppression, remote drift, outage/response-loss and explicit import. PostgreSQL, race and independent verification remain bounded to their stable stage, while unrelated Nocturne, offline-sync, Compose and platform evidence is reused unless notesync changes invalidate it.
