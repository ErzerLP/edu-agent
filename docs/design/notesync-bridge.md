# NoteSync bridge technical reference

## Verified upstream baseline

The production adapter is designed against these exact source revisions:

- Fast Note Sync Service `3.6.1`, commit [`7a6c78792c631f999c8a5f725bba5dd7235d6688`](https://github.com/haierkeys/fast-note-sync-service/tree/7a6c78792c631f999c8a5f725bba5dd7235d6688).
- Obsidian Fast Note Sync plugin `2.4.0`, commit [`f2b15c09d34e621d2d97ad526fdee03460bac151`](https://github.com/haierkeys/obsidian-fast-note-sync/tree/f2b15c09d34e621d2d97ad526fdee03460bac151).

The server registers the REST group and WebSocket upgrade in [`internal/routers/router_api.go`](https://github.com/haierkeys/fast-note-sync-service/blob/7a6c78792c631f999c8a5f725bba5dd7235d6688/internal/routers/router_api.go). The bridge uses only the REST routes whose source contract has been reviewed:

| Capability | Upstream route | Required permission | Bridge use |
| --- | --- | --- | --- |
| Service/version | `GET /api/version` | none | Exact version compatibility |
| Health | `GET /api/health` | none | Dependency health and database readiness only |
| Vault list | `GET /api/vault` | `note_r` | Configured vault presence/read authorization |
| Note read | `GET /api/note` | `note_r` | Exact preflight and readback |
| Note list | `GET /api/notes` | `note_r` | Bounded explicit preview discovery |
| Note write | `POST /api/note` | `note_w` | Create-only first publish or reviewed update |

Authentication behavior comes from [`internal/middleware/user_auth_token.go`](https://github.com/haierkeys/fast-note-sync-service/blob/7a6c78792c631f999c8a5f725bba5dd7235d6688/internal/middleware/user_auth_token.go): the API accepts `Authorization: Bearer <token>`, derives protocol `rest`, checks `X-Client` before query `client`, verifies `note_r/note_w`, and applies the token's vault restriction. The bridge sends `X-Client: CLI`, fixed client name/version headers and a fixed User-Agent. The operator creates a token restricted to the configured vault with scope `p:rest c:CLI f:note_r,note_w`.

The upstream HTTP response wrapper is defined in [`pkg/app/app.go`](https://github.com/haierkeys/fast-note-sync-service/blob/7a6c78792c631f999c8a5f725bba5dd7235d6688/pkg/app/app.go). Business failures commonly remain HTTP 200, so the adapter validates both transport status and the JSON `status/code/data` envelope.

## Why the bridge does not use WebSocket writes

The official plugin uses `ws(s)://<origin>/api/user/sync`, authenticates with the text frame `Authorization|<token>`, then sends `ClientInfo|{...}`. `NoteModify` supports `baseHash`, and `offlineSyncStrategy:"manualMerge"` can produce a `NoteSyncNeedPush` conflict response with code 530.

That protocol is insufficient as a bridge CAS:

1. The handler reads and checks the old server hash before entering the per-path write lock in [`internal/routers/websocket_router/ws_note.go`](https://github.com/haierkeys/fast-note-sync-service/blob/7a6c78792c631f999c8a5f725bba5dd7235d6688/internal/routers/websocket_router/ws_note.go).
2. The keyed lock is acquired later inside `ModifyOrCreate` in [`internal/service/note_service.go`](https://github.com/haierkeys/fast-note-sync-service/blob/7a6c78792c631f999c8a5f725bba5dd7235d6688/internal/service/note_service.go).
3. Two writers can therefore pass the same base check and execute their writes serially; the last write wins.
4. `isConflictResolved:true` bypasses the conflict check rather than performing a second expected-hash comparison.
5. Ack correlation is path-based and there is no operation ID or idempotency key.

The public plugin protocol document also omits several of these frames and fields. The REST API has the same overwrite limitation but a smaller, source-reviewed surface. The confirmed product contract therefore uses REST plus fail-closed preflight and durable review, and retains the upstream concurrent-write limitation explicitly.

## Configuration

Planned server settings use the existing config loader and secret redaction conventions:

```text
EDU_AGENT_NOTESYNC_ENABLED=false
EDU_AGENT_NOTESYNC_BASE_URL=
EDU_AGENT_NOTESYNC_API_TOKEN=
EDU_AGENT_NOTESYNC_VAULT=
EDU_AGENT_NOTESYNC_PATH_PREFIX=edu-agent
EDU_AGENT_NOTESYNC_HTTP_TIMEOUT=10s
EDU_AGENT_NOTESYNC_WORKER_INTERVAL=3s
EDU_AGENT_NOTESYNC_WORKER_BATCH=20
EDU_AGENT_NOTESYNC_SCAN_PAGE_SIZE=100
EDU_AGENT_NOTESYNC_SCAN_MAX_PAGES=20
```

Exact names may be adjusted to existing config naming style during S1, but the fields and security semantics are fixed. Empty or disabled configuration must not construct a partially working client. Non-loopback HTTP is rejected unless the existing explicit insecure-server setting permits it. URL validation rejects embedded credentials, query and fragment. Redirect following is disabled.

Compatibility policy:

| Observed version | Read probe | Automatic write | Status |
| --- | --- | --- | --- |
| `3.6.1` with expected envelopes/routes | allowed | allowed after preflight | healthy |
| Missing/invalid version | no | no | degraded `version_unavailable` |
| Older version | no | no | degraded `version_unsupported` |
| Newer/other version | bounded diagnostic only | no | degraded `version_untested` |
| Correct version but vault/scope failure | no | no | degraded `capability_unavailable` |

There is no `allow untested` escape hatch in the first implementation. Supporting another version requires updating this matrix and passing the real contract gate.

## Upstream request and response rules

All requests set `Accept: application/json`, a fixed User-Agent and the upstream client identity. JSON bodies have deterministic field encoding. The adapter uses one total request deadline and a bounded response body. It rejects redirects, HTML error pages, multiple JSON values, unknown required fields where a closed local DTO is expected, and a response vault/path that differs from the request.

The write body follows the upstream `dto.Note` fields used by `CreateOrUpdateNote`:

```json
{
  "vault": "ConfiguredVault",
  "path": "edu-agent/topic/note.md",
  "content": "...",
  "ctime": 1700000000000,
  "mtime": 1700000000000,
  "createOnly": true
}
```

The upstream server overwrites `mtime` with server time and increments its row version. Neither value enters canonical ordering. First publication sets `createOnly:true`. Reviewed updates use `createOnly:false` only after exact remote preflight.

A note-not-found response is recognized by the actual status/code envelope, not only by HTTP status. Successful write and read responses must contain the requested vault/path. Exact returned content is compared byte-for-byte after normal JSON decoding; local SHA-256 is stored for efficient equality checks but does not replace exact equality at trust boundaries.

## Package ownership

Planned boundaries:

```text
server/internal/knowledge
  owns publication mapping, review snapshots, resolution validation,
  current canonical export and privacy redaction

server/internal/integrations/notesync
  owns the pinned upstream HTTP client, capability probe,
  Outbox consumer and remote error classification

server/internal/platform/outbox
  remains generic: lease, retry, attempts, dead-letter

server/internal/transport/httpapi
  exposes closed status/preview/review/resolution application APIs

clients/cli-go
  calls only those public APIs; it never receives the upstream token
```

The integration package defines consumer-side ports for loading current publication work and committing observed remote results. PostgreSQL implementations remain in the knowledge owner. The knowledge store may depend on the generic Outbox persistence helper to enqueue an intent in its caller-owned transaction; the integration client never writes knowledge tables directly.

## Persistence model

The migration adds knowledge-owned tables conceptually equivalent to:

```text
knowledge_notesync_publications
  document_id PK
  remote_vault
  remote_path
  published_knowledge_revision_id
  published_document_revision_id
  published_revision_no
  base_markdown
  base_sha256
  remote_version
  remote_last_time
  generation
  created_at / updated_at

knowledge_notesync_reviews
  review_id PK
  document_id nullable
  remote_vault / remote_path
  kind / reason_code / status
  base_revision_id nullable
  local_revision_id nullable
  base_markdown nullable
  local_markdown nullable
  remote_markdown nullable
  base_sha256 / local_sha256 / remote_sha256
  basis_hash
  generation
  resolution fields
  created_at / resolved_at
```

Exact SQL names and normalization are implementation details, but the invariants are fixed:

- stable document ID owns one current mapping;
- remote vault/path is unique among current mappings;
- revision numbers are positive and monotonic;
- full text and hashes are bounded and consistent;
- a review snapshot is immutable;
- only one open review exists for the same document/path/basis;
- every row carries learner generation and participates in knowledge redaction;
- remote token is never stored in these tables.

The current generic Outbox payload remains narrow:

```json
{
  "schema_version": 1,
  "document_id": "uuid",
  "knowledge_revision_id": "uuid",
  "document_revision_id": "uuid",
  "publication_reason": "canonical_revision|review_keep_canonical",
  "review_id": "optional-uuid"
}
```

The Outbox row itself carries business type, aggregate ID, deterministic idempotency key, monotonic revision and generation. Markdown is rendered from the current authoritative revision when the consumer runs.

## Outbound state machine

`CanApply` performs only deterministic/local checks and returns no-op for stale revision or generation. `Apply` performs the remote preflight and mutation. The business states are derived from generic Outbox plus knowledge publication/review state:

```text
queued
  -> superseded                 stale revision/generation, no remote call
  -> already_applied            remote exact target
  -> review_required            observed remote drift/missing/collision
  -> mutation_unknown           remote call may have committed
  -> applied                    write followed by exact readback
  -> retryable                  confirmed base unchanged or dependency unavailable
  -> dead                       bounded retries exhausted or permanent contract error
```

An unknown result is never retried blind. Reconciliation first reads remote:

```text
remote == target -> applied
remote == base   -> retryable write
remote == other  -> review_required
read unavailable -> remain unknown/retryable under generic bounded policy
```

The consumer serializes work per document through Outbox aggregate ordering and a knowledge mapping lock/CAS. Before committing applied state it rechecks generation and current document revision. A privacy barrier or newer canonical revision invalidates the old attempt even if the remote write occurred; the remote observation is retained only as bounded audit metadata and cannot restore old local content.

## Three-way preview and resolution

Preview input is either one managed remote path or the configured prefix with bounded pagination. Results are deterministic and sorted by normalized remote path. Each candidate becomes one of:

```text
in_sync
remote_unchanged
remote_changed
local_changed
both_changed
remote_missing
remote_moved
unbased_remote
path_occupied
invalid_remote_markdown
```

The basis hash commits to review kind, generation, document ID, mapping, local revision ID and all three content hashes. Resolution recalculates the same facts before use. Any mismatch returns `stale_notesync_review` and leaves canonical state unchanged.

Resolution behavior:

- `accept_remote`: pass the frozen remote Markdown to the existing knowledge import with current expected parent.
- `merged`: read canonical UTF-8 Markdown supplied through request body or CLI stdin/file, then use the same knowledge import.
- `keep_canonical`: close the review and enqueue a reviewed publication intent for the current canonical document revision.

If knowledge import returns identity review, the notesync review remains linked and unresolved until the existing identity decision succeeds. A successful accept/merge creates a new KnowledgeRevision; its normal publication Outbox then exports the resolved canonical content back to the managed remote path.

## API sketch

Final paths follow existing HTTP naming conventions and remain subject to OpenAPI review. The intended use cases are:

```text
GET  /v1/knowledge/notesync/status
POST /v1/knowledge/notesync/previews
GET  /v1/knowledge/notesync/reviews
GET  /v1/knowledge/notesync/reviews/{review_id}
POST /v1/knowledge/notesync/reviews/{review_id}/resolutions
```

Schemas are closed and use stable enums. Preview limits are explicit. Detail endpoints that return Markdown hold the knowledge read permit through response commit. Resolution requests carry operation ID, review basis hash, resolution type and optional merged Markdown. Existing request idempotency and error envelope conventions apply.

Likely stable errors include:

```text
notesync_not_configured
notesync_unavailable
notesync_version_unsupported
notesync_version_untested
notesync_capability_unavailable
notesync_remote_changed
notesync_review_required
stale_notesync_review
notesync_path_conflict
notesync_result_unknown
content_redacted
privacy_clear_in_progress
```

The implementation should reuse an existing generic error when its semantics are already exact instead of multiplying equivalent codes.

## CLI sketch

```text
edu-agent knowledge notesync status
edu-agent knowledge notesync preview [--path REMOTE_PATH]
edu-agent knowledge notesync reviews
edu-agent knowledge notesync show REVIEW_ID
edu-agent knowledge notesync resolve REVIEW_ID --accept-remote
edu-agent knowledge notesync resolve REVIEW_ID --keep-canonical
edu-agent knowledge notesync resolve REVIEW_ID --merged-file FILE
```

Merged content may also be read from stdin. Markdown never belongs in argv. The CLI displays compact base/local/remote unified diffs, stable reason codes and source revision metadata. Non-TTY output remains deterministic. It does not store a local sync database or call Fast Note Sync directly.

## Privacy and external-copy statement

The knowledge privacy owner redacts local publication bases and review snapshots. The Outbox generation fence stops all old-generation publication. Any request or worker that acquired old content before the barrier must fail response/commit permit checks and discard it.

Fast Note Sync and Obsidian are independently administered external stores. The current server privacy receipt cannot prove deletion from those systems, their backups, local vault history or filesystem snapshots. When bridge state is redacted, operator-facing status must state that external cleanup of the configured vault/prefix may still be required. No code path may mark those copies physically erased without a future verified remote-erasure protocol.

## Test evidence plan

### S1 checks

- strict upstream envelope fixtures extracted from pinned source behavior;
- raw Authorization and `CLI` client configuration;
- URL, TLS, redirect, body limit and redaction;
- exact version/vault capability matrix;
- malformed, false-status, unknown-code, 401/403/404/429/5xx and timeout classification.

### S2 checks

- knowledge import commits Outbox intent atomically;
- rollback leaves neither revision-only side effect nor orphan intent;
- stale revision and generation make zero remote writes;
- initial create-only and existing-base update;
- remote drift/missing/path collision generate one review;
- response loss reconciles by GET;
- applied state advances only after exact readback;
- one named real PostgreSQL vertical scenario.

### S3 checks

- bounded note pagination and path filtering;
- stable document ID mapping and unmanaged remote note;
- base/local/remote diff categories;
- stale local, remote and generation basis rejection;
- accept-remote, merged and keep-canonical;
- existing knowledge identity review continuation;
- OpenAPI/handler/CLI closed contract and no secret exposure;
- privacy response cancellation and local redaction.

### S4 real upstream gate

The candidate gate starts the exact Fast Note Sync Service `3.6.1` source/image with its required database, creates a restricted test token and vault, and drives the actual Go adapter. It proves:

1. exact version/health/vault probe;
2. first create-only publication and plugin-visible Markdown metadata;
3. current revision update and exact readback;
4. duplicate and old Outbox no-op;
5. remote Obsidian edit creates review without automatic overwrite;
6. explicit accept/merge produces a new canonical revision and republishes;
7. outage queues while core knowledge commit succeeds;
8. response loss is reconciled rather than blindly retried;
9. incompatible version/capability disables writes;
10. cleanup removes only test vault data and does not claim production erasure.

The fixture must record upstream tag/commit or verified image identity. A floating latest image or a fake server is not valid compatibility evidence.
