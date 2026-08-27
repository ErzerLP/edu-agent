# Knowledge maintenance technical reference

## Change boundary

This change provides the complete knowledge-maintenance surface: the Go application services and PostgreSQL production paths for knowledge proposals and learning-owned evidence carryover, authenticated HTTP and CLI review flows, read/propose-only MCP descriptors, and restricted `user` / `agent` pairing profiles. Callers cannot use maintenance endpoints to bypass raw-import identity rules or directly mutate learning evidence.

Knowledge maintenance owns canonical proposal planning and application. It reads valid accepted learning evidence through a learning-owned port and freezes that impact. An affected apply may atomically create a separate learning-owned carryover proposal through the narrow transactional store port, but it never rewrites, copies, invalidates, or projects source evidence. Carryover approve/reject, event append, and provisional links remain owned by the learning service.

## Application API

`knowledge.Service` exposes these maintenance operations:

```text
Create(CreateProposalCommand) -> Proposal
List(ProposalListCommand) -> ProposalPage
Get(proposalID) -> Proposal
Decide(ProposalDecisionCommand) -> Proposal
CreateRollback(CreateRollbackCommand) -> Proposal
```

Create input contains only:

- request UUID;
- explicit base revision UUID;
- one or more structured source records;
- a complete candidate snapshot, including zero documents for a destructive empty snapshot;
- identity review receipt and necessary document/node resolutions;
- actor identity supplied by trusted composition, never decoded from request JSON.

Rollback input contains a request UUID, current base, unredacted ancestor target, sources, and trusted actor identity. Decision input contains an operation UUID, proposal UUID, `approve` or `reject`, a non-empty reason, and trusted actor identity.

All request DTOs use closed JSON decoding. There are no input fields for diff, risk, impact, basis, status, decision actor, planned revision identity, or applied revision identity.

Create and decision operations are globally idempotent by operation UUID. The stored request hash commits to all caller-controlled input. Exact replay returns the first proposal with `replayed=true`; changed input returns `idempotency_conflict`.

## Canonical planning

Candidate planning uses the existing `Service.Import` implementation through a no-write planning store. The planner therefore reuses:

- canonical Markdown normalization and indexing;
- document and node identity ownership checks;
- identity review receipts and locators;
- rewrite/split/merge cardinality validation;
- deterministic document/node revision IDs;
- lineage materialization.

The temporary planner has two private maintenance modes that cannot be injected through `ImportCommand` JSON:

- complete snapshot replacement instead of raw import's incremental upsert;
- explicit restoration of a historical document/node identity when ownership still matches.

A proposal freezes the resulting prepared revision, canonical Markdown, stable identities, lineage, revision UUID, revision number, and manifest hash. Approval applies this exact plan; it does not rerun random identity allocation or rebase onto a newer head.

An empty candidate is planned directly as a complete empty snapshot because raw import requires at least one document. It remains destructive and can never auto-apply.

If ordinary planning returns `identity_review_required` and the proposal request does not contain a matching receipt and complete resolutions, the maintenance-only planner performs bounded server-internal review rounds with conservative `new` resolutions. The resulting exact plan is marked `IdentityImpact.Uncertain`, remains open, and cannot auto-apply. Human approval explicitly accepts that frozen delete/add identity outcome; a reviewer who wants preserve, rewrite, split, or merge semantics submits a new proposal with the returned review receipt and explicit resolutions. The ordinary raw-import path remains unchanged and continues to return identity review instead of applying a conservative plan.

## Deterministic analysis

`knowledge-diff-v1` compares the base and candidate by stable document ID, then stable node ID. Ordering is path then stable ID. It reports:

- document add, delete, move, edit, and move+edit;
- preserved, added, removed, and moved document identities;
- preserved, added, and removed node identities;
- node local-body, title/ancestor, and structural changes;
- rewrite/split/merge lineage produced by canonical planning;
- move, delete, restore, and rollback provenance flags;
- old node revision IDs whose accepted evidence may be affected.

Human-readable document diffs use the repository's existing deterministic unified-diff dependency. Inputs above 512 KiB, more than 10,000 lines, or output above 256 KiB are explicitly marked truncated. UTF-8 output is never cut inside a rune.

Accepted evidence impact is read from learning as:

```text
count
[evidence_id, node_revision_id, knowledge_revision_id] sorted by evidence ID
learning privacy generation
accepted-evidence-impact-v1 fingerprint
```

Only evidence with a non-null accepted event sequence and no invalidation is included. Existing evidence remains bound to its original knowledge revision and node revision.

## Risk and auto-apply v1

`knowledge-risk-v1` and `knowledge-auto-apply-v1` fail closed. Automatic application is allowed only when all conditions hold:

- every change is a pure document addition or an existing-document local-body edit;
- existing path, title, ancestor chain, heading level, parent, sibling order, document identity, and node identity remain unchanged;
- no document is moved, deleted, restored, or rolled back;
- no rewrite, split, or merge lineage exists;
- no identity uncertainty remains;
- no accepted evidence is affected;
- no diff is truncated;
- at most 3 documents, 20 changed nodes, and 32 KiB of changed body are involved.

Any failed condition leaves the proposal open. Delete, restore, rollback, and explicit lineage are high risk. Other review-required changes are medium risk. Auto-applicable changes are low risk.

## Basis and stale semantics

`knowledge-proposal-basis-v1` commits to:

- proposal ID and kind;
- base and rollback target;
- planned revision ID, number, and manifest;
- knowledge and learning privacy generations;
- normalized sources and candidate hash;
- deterministic diff, identity impact, lineage impact, accepted evidence impact, and risk;
- canonicalizer, identity, diff, risk, and auto-policy versions.

Approval and rejection do not rebase. An open proposal becomes stale when any of these conditions is observed under the decision transaction:

- catalog head no longer equals the proposal base;
- knowledge generation changed;
- learning generation or accepted-evidence fingerprint changed;
- a relevant policy/canonicalizer version changed;
- recomputed stored basis differs from the frozen basis.

A stale decision records the current head and advances no canonical state.

## State machine

```text
open -> applied
open -> rejected
open -> stale
open -> redacted
```

Terminal state and frozen basis are immutable. Auto policy records an immutable `auto -> applied` decision. Human approval records `approve -> applied` or `approve -> stale`. Human rejection records `reject -> rejected` or `reject -> stale`.

Privacy scrub may redact payloads of an already terminal proposal without changing its audit outcome. An open proposal becomes `redacted` and can no longer be decided.

## PostgreSQL model

Migration `000011_knowledge_maintenance.sql` adds:

```text
knowledge_maintenance_proposals
knowledge_maintenance_decisions
knowledge_maintenance_operations
knowledge_revision_origins
```

The proposal row contains indexed status/basis metadata plus two privacy-sensitive JSON payloads:

- `record`: sources, candidate, diff, impacts, risk, and affected node revisions;
- `prepared_commit`: the exact canonical revision plan and canonical Markdown.

Decision, idempotency operation, and revision origin are append-only. Proposal triggers permit only one transition out of open. Every table uses the knowledge owner write gate.

`KnowledgeRevision.Origin` is loaded from `knowledge_revision_origins` when present. Raw-import revisions remain compatible and have no maintenance origin.

## Transaction and lock order

Create and decision use `READ COMMITTED`. Read/list use repeatable privacy reads.

Create order:

```text
knowledge privacy gate
learning privacy gate
create operation advisory lock
idempotency replay check
knowledge catalog FOR UPDATE
SHARE lock learning_evidence, invalidations, and approved carryover links when affected IDs exist
current accepted-evidence fingerprint check
optional outbox generation and affected NoteSync document locks
proposal / revision / lineage / origin / head / outbox / decision / operation writes
commit
```

Decision order:

```text
knowledge privacy gate
learning privacy gate
decision operation advisory lock
idempotency replay check
proposal FOR UPDATE
knowledge catalog FOR UPDATE
SHARE lock learning_evidence, invalidations, and approved carryover links when affected IDs exist
current accepted-evidence fingerprint check
stale decision, or optional outbox locks and exact prepared apply
proposal / revision / lineage / origin / head / outbox / decision / operation writes
commit
```

The knowledge catalog is the cross-owner serialization point and is always locked before learning evidence or carryover tables. The learning table locks conflict with evidence acceptance, invalidation, and carryover-link writes, closing the race between impact calculation and commit. An affected apply may insert only the separate learning-owned carryover proposal in this transaction; it does not append learning events, add provisional links, or alter learning projections.

Two proposals with one base can both remain open, but catalog row serialization and the linear parent constraint allow at most one to apply. The loser records stale with the winner's head.

Any failure after proposal insertion, revision insertion, origin insertion, or outbox enqueue rolls back the complete transaction. There is no import-operation side channel for maintenance applies.

## Rollback

A rollback target must be an unredacted strict ancestor of the current head. Its immutable snapshot documents are reused in a new prepared revision. Applying rollback:

- allocates a new revision UUID and next revision number;
- uses the current head as parent;
- restores the target manifest and canonical document revisions;
- records rollback origin and target;
- leaves target and intervening revisions unchanged and readable.

Rollback is always high risk and requires approval.

## Privacy

Knowledge content scrub removes or overwrites:

- candidate Markdown and prepared canonical Markdown;
- sources and source excerpts;
- human-readable diffs and impact payloads;
- request and basis hashes;
- decision reasons.

Open proposals become redacted. Applied/rejected/stale outcomes, stable IDs, timestamps, origin relation, and minimal terminal audit metadata remain. Reads of a redacted proposal return no candidate, source, diff, evidence IDs, reason, or basis hash. `VerifyRedacted` includes all maintenance payloads.

Knowledge and learning generation gates fail closed before proposal reads, creation, decision, or evidence impact reads. PostgreSQL proposal create, replay, list, get, and decision transactions acquire persistent owner gates in the common `knowledge -> learning -> operation -> catalog -> learning tables` order, so a close in another process cannot race planning, persistence, or readback. Ordinary knowledge-only reads retain their knowledge-only gate. Final HTTP and MCP response commit first acquires persistent owner gates in the canonical `identity -> knowledge -> learning -> tutoring -> memory -> outbox` order, then the process-local response-write gates in that same order, and holds both through the complete buffered flush. This matches barrier commit's persistent-exclusive-gate-before-local-drain sequence and avoids cross-process leakage or same-process deadlock. If privacy close wins, the private buffer is discarded and the route emits only the generic `503 content_redacted` envelope.

## Evidence carryover

An applied rewrite, split, or merge that affects valid accepted evidence creates one learning-owned carryover proposal per source evidence binding. The proposal freezes the source binding, target knowledge revision, candidate target node revisions, privacy generations, policy version, and basis fingerprints. Existing source evidence remains unchanged.

Carryover approval revalidates the knowledge target under the catalog lock, revalidates source evidence and any earlier approved-link binding under learning table locks, then appends one `EvidenceCarryoverApproved` event and provisional links. Rejection appends `EvidenceCarryoverRejected` and creates no links. Exact operation replay returns the first terminal result; conflicting replay fails. Incremental and full replay derive the same provisional carryover projection without changing accepted-evidence counts, retained state, mastery, or review scheduling.

Knowledge maintenance planning recognizes accepted source evidence either at its original node revision or through an approved carryover link. Invalidated source evidence is excluded in both cases, so consecutive maintenance revisions preserve approved carryover semantics without reviving invalid evidence.

## Authorization and transports

The `user` pairing profile freezes both `knowledge:approve` and `learning:approve`; migration `000011` adds both scopes to legacy tokens only while the token and its owning device remain active, leaving revoked credentials unchanged. The restricted `agent` profile contains neither approval scope nor `memory:write`, and profile construction fails closed if any is introduced. Knowledge proposal approve/reject and rollback require `knowledge:approve`. Raw knowledge import requires both `knowledge:write` and `knowledge:approve`. Evidence carryover approve/reject require the independent `learning:approve` scope; online and offline Assessment decisions require both `learning:write` and `learning:approve`. Neither approval scope authorizes the other owner's decision path, and Memory admission/deletion remains guarded by `memory:write`.

HTTP exposes proposal create/read/decision operations and carryover read/decision operations according to those scopes; OpenAPI records combination gates in `x-required-scopes`. MCP exposes proposal submit/read and carryover read descriptors only, with no proposal decision, rollback, carryover decision, raw import, Assessment decision, or Memory admission capability in either its catalog or service interface. The CLI uses the same HTTP DTOs and derives decision actors exclusively from the authenticated credential.

## Focused verification

The batch includes deterministic unit coverage for closed input, idempotency, diff/risk, auto policy, evidence fail-closed behavior, same-base stale, rollback, scope compatibility, and response commit ordering. The PostgreSQL vertical tests cover non-zero accepted evidence impact, auto apply, approval concurrency, rollback, revision origin, NoteSync outbox atomicity, injected auto/approved failure rollback, learning zero writes, privacy scrub, active-only legacy scope migration, and cross-store knowledge/learning generation closure for fresh proposal writes, replay, list, get, and decision.
