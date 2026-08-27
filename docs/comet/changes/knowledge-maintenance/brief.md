# Agent-governed knowledge maintenance

# User outcome

A restricted Agent can submit sourced knowledge-maintenance proposals against the current canonical knowledge revision. The service, not the Agent, computes the bounded diff, identity and lineage impact, accepted-learning-evidence impact, risk, and applicable policy. Small additions and local low-risk edits can be applied automatically; moves, major rewrites, splits, merges, deletions, and rollbacks require explicit human approval. Every application appends a new canonical revision and preserves immutable history. Any carryover of learning evidence across lineage is a separate learning-owned approval and remains provisional.

# Requirements

1. Proposal creation accepts an idempotent request, an explicit base revision, one or more structured source records, and either a candidate canonical snapshot or an unredacted historical rollback target. Caller-supplied risk, diff, lineage impact, evidence impact, decision state, actor identity, or target revision identifiers are rejected.
2. The server deterministically canonicalizes the candidate and computes document and node changes, stable-identity effects, lineage effects, affected valid accepted evidence, bounded human-readable diffs, a basis hash, and versioned risk and auto-apply decisions. Source records are provenance supplied by the caller; the service validates their shape and hashes but does not claim to verify an external URL's truth.
3. The versioned auto-apply policy is fail closed. It permits only bounded pure additions or bounded local content edits whose path, structure, titles, stable identities, and accepted-evidence bindings remain unchanged. Truncated analysis, uncertain identity, structural change, or affected valid evidence requires review.
4. Move or rename, major rewrite, split, merge, deletion, restore, and rollback proposals cannot auto-apply. A proposal is approved or rejected as one atomic unit; partial approval is not supported.
5. Proposal state is append-only and terminal: `open` becomes exactly one of `applied`, `rejected`, `stale`, or `redacted`. An approval applies the exact frozen proposal; there is no automatic rebase. A changed head, privacy generation, proposal basis, or relevant policy/canonicalizer version makes it stale.
6. Auto-apply and approved apply use the existing canonical knowledge construction and identity rules. The proposal, decision, new knowledge revision, lineage, origin metadata, knowledge head, idempotency result, privacy fence, and any NoteSync publication intents commit atomically, or none of them do.
7. Applied rewrite, split, and merge effects produce the existing canonical lineage records with valid cardinality. Maintenance impact also preserves observable move, delete, restore, and rollback provenance without mutating historical revisions.
8. Rollback is represented as a destructive proposal targeting an unredacted ancestor of the current head. Applying it creates a new revision whose parent is the current head and whose content restores the target snapshot; it never moves the head pointer backwards or deletes intervening history.
9. Knowledge application never rewrites, copies, invalidates, or silently transfers learning evidence. Existing evidence remains bound to its original knowledge and node revision.
10. If an applied lineage affects valid accepted evidence, the service creates a separate learning-owned carryover proposal. It requires a distinct operation, `learning:approve` authority, explicit reason, and independent approve/reject decision. Approval appends a replayable provisional carryover record/event; rejection has no projection effect. Provisional carryover cannot increase accepted-evidence counts, set retained mastery, or advance review scheduling.
11. Proposal and carryover reads expose their frozen basis, sources, bounded diff or migration recommendation, risk, policy versions, decisions, current status, and applied revision or event references. Pagination and filtering are deterministic and do not expose redacted payloads.
12. Pairing supports an explicit restricted Agent profile. Agent credentials can read knowledge and learning state and submit proposals, but cannot call raw knowledge import, approve/reject knowledge changes, create rollback proposals, approve evidence carryover, make Assessment decisions, or use Memory admission/deletion capabilities. Existing user pairing remains backward compatible.
13. HTTP exposes proposal create/list/detail/decision and rollback creation, plus independent evidence-carryover list/detail/decision. MCP exposes proposal submission and read-only proposal inspection only; it exposes no approval, rollback, raw import, evidence-carryover decision, Assessment decision, or Memory admission descriptor. The Go CLI provides the human proposal and carryover review commands.
14. Knowledge and learning privacy barriers fail closed for proposal creation, reads, decisions, application, and carryover. Privacy clearing redacts proposal Markdown, diffs, source excerpts, decision reasons, and carryover payloads while preserving only non-sensitive terminal audit metadata required by existing erasure contracts.

# Acceptance examples

- A1：Replaying the same proposal operation with the same base, sources, candidate, and requested identity resolutions returns the same proposal; changing any bound input returns a machine-readable idempotency conflict.
- A2：A created proposal reports the service-computed base, bounded diff, identity and lineage impact, valid-evidence impact, risk, basis hash, and policy versions. Closed schemas reject attempts to inject computed or actor fields.
- A3：A bounded pure addition and a bounded local body edit auto-apply and create a new canonical head. A path/title/tree/stable-identity change, truncated analysis, uncertain identity, or affected valid evidence remains open.
- A4：Move, major rewrite, split, merge, delete, restore, and rollback stay open until a `knowledge:approve` decision. Missing scope, stale basis, or invalid structural resolution cannot change the head.
- A5：Auto-apply or approve commits proposal state, decision, revision, lineage/origin, idempotency result, head, privacy generation check, and NoteSync intents atomically. Injected failure leaves no partial state.
- A6：Two proposals based on the same head cannot both advance it. At most one applies; the other becomes deterministically stale and reports the current revision without automatic rebase.
- A7：Applied rewrite/split/merge lineage has valid existing cardinality; move preserves stable identity; delete/restore/rollback impacts remain queryable from the proposal and revision origin.
- A8：Rollback creates a new revision ID and revision number with the previous head as parent and the historical target manifest/content. The target and intervening revisions remain readable and unchanged.
- A9：Applying a knowledge proposal does not write learning evidence, learning events, or learning projections, and old evidence remains exactly bound to the original revision.
- A10：An affected lineage creates a separate carryover proposal. Knowledge approval cannot approve it. `learning:approve` approval appends a provisional carryover without mutating source evidence; rejection creates no carryover effect.
- A11：Incremental and full replay expose the same provisional carryover state and fingerprint. Carryover never increases accepted evidence, retained mastery, or review advancement.
- A12：Restricted Agent credentials can submit/read proposals but receive 403 from raw import, knowledge decision, rollback, carryover decision, Assessment decision, and Memory admission/deletion paths. Existing user credentials retain their current compatible scopes.
- A13：HTTP, MCP propose/read, and CLI review flows use the same application proposal records, scopes, errors, privacy permits, rate limits, and audit policy. MCP discovery contains none of the forbidden approval or direct-write descriptors.
- A14：Closing the knowledge or learning privacy generation during a proposal or carryover response prevents content leakage. After erasure, candidate Markdown, diffs, source excerpts, reasons, and carryover payloads cannot be retrieved.

# Non-goals

- Fetching or parsing web pages, PDFs, or external repositories.
- A server-side autonomous model worker that invents proposal content.
- Automatic proposal rebasing, partial application, or silent conflict resolution.
- Web UI or Rust CLI.
- Automatic accepted-evidence transfer or weighted mastery credit.
- MCP approval, rollback, raw knowledge import, Assessment decision, or Memory admission/deletion.
