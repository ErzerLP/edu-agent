# Operations Candidate Qualification

```text

## Purpose

The repository provides one fail-closed qualification surface for a release candidate. It combines deterministic code checks, real PostgreSQL behavior, production model-adapter behavior through a programmable fake service, fixed external dependency contracts, and resumable evidence without turning historical prose or a successful process exit into proof.

## Candidate identity

A candidate identity is derived from all production source, migrations, public contracts, test fixtures, candidate runners, dependency locks, module dependency files, and explicit test selection that can change the result. Toolchain and platform identities are recorded separately and participate in evidence compatibility.

Changing any behavior-relevant input invalidates only the affected evidence shards. Evidence from a different source digest, runner digest, lock digest, selection, toolchain, platform, or external image cannot be silently reused.

## Result model

Each required gate has one of these states:

- `passed`: required scenarios actually executed and all passed;
- `failed`: a scenario executed and disproved the candidate or the runner violated its contract;
- `blocked`: an external prerequisite or supported platform requirement was unavailable;
- `not-run`: the gate was deliberately not executed and has no reusable evidence;
- `reused`: a complete prior pass evidence matches every relevant input and its log digest is valid.

The aggregate candidate passes only when every required gate is `passed` or validly `reused`. `blocked`, `not-run`, empty selection, all-skipped execution, missing logs, or malformed evidence never count as pass.

## Evidence manifest

Every persisted gate result records:

- schema version and gate/shard identity;
- candidate, runner, fixture, dependency-lock, schema and test-selection digests;
- Go, Docker, Compose and Skopeo versions when applicable;
- host OS and architecture and the locked image platform;
- start/end timestamps, exit status and normalized terminal state;
- the number and names or stable identifiers of matched, executed, passed, failed and skipped target scenarios;
- a SHA-256 of the redacted durable log;
- external image/version/config digests used by the gate.

Resume validates the complete manifest and log hash before reuse. Unknown required fields, truncation, manual or partial markers, missing logs, incompatible schema, or any digest mismatch rejects reuse.

Evidence and logs exclude credentials, pairing codes, bearer tokens, external service secrets, learner content, answers, knowledge payloads and erased data. Redaction happens before durable hashing.

## Test execution proof

A named Go test gate enumerates its expected tests before execution and records actual test events after execution. At least one requested target must match and at least one target must pass. A zero-match regex, `[no tests to run]`, all-skipped package, empty event stream, unexpected package substitution, or missing target event is a failed runner contract and cannot create pass evidence.

Package-wide gates may contain packages with no tests only when the gate is not claiming a named behavioral scenario for those packages; their compile result is recorded separately from executed test evidence.

## Resource and cleanup contract

All heavyweight candidate gates use one host-wide qualification lock. Each candidate gets a unique working/evidence directory, database or schema, Compose project, container names, volumes and dynamically allocated host resources. Gates do not overwrite another candidate's logs or evidence.

Cleanup is bounded and scoped to resources created by the current gate. Interruption preserves the durable log and records an incomplete terminal state; it does not create pass evidence. PostgreSQL shards remain strictly serial.

## Fixed external dependencies

The repository owns a machine-readable approved dependency index that binds each external contract to version, source commit where applicable, image manifest/platform/config digest and its authoritative lock files.

PostgreSQL, Nocturne and Fast Note Sync candidate gates reject an unapproved image override even when it contains a digest. A lock change invalidates prior evidence and requires the affected real contract to run again. Successful testing of one release never implies automatic compatibility with another release.

Fast Note Sync runs the real pinned service contract and production client/store/consumer flow. The Obsidian plugin remains constrained by its pinned version, commit and capability matrix; desktop UI automation is outside this qualification.

Nocturne runs locked supply-chain verification, OCI verification and the real Compose/PostgreSQL contract, including outage/retry, rollback, deletion and generation-fence scenarios.

## Production model contract

The assessment model gate uses the programmable fake service through the production OpenAI-compatible client, production tutor model adapter, learning application service and real PostgreSQL stores.

The fixed scenario corpus proves:

- invalid or drifted schema is rejected without producing authoritative learning facts;
- low-confidence output remains provisional and cannot create accepted evidence;
- transient timeout or rate-limit failure is retried only according to policy and records each attempt category;
- exhausted retries return the defined degraded/unavailable result without mutating authoritative session, decision or evidence state;
- a later successful attempt freezes the final model artifact and its provenance exactly once.

The corpus runs for explicit baseline and candidate model profile/protocol versions. Both profiles must satisfy the same schema, classification, retry, persistence and authority invariants. Profile or protocol changes invalidate prior model evidence. No real provider credential is required or used.

## Deterministic knowledge rebuild

A fixed knowledge corpus includes unchanged documents and add, body edit, heading edit, move, reorder and delete operations. Incremental snapshot construction is compared with an independent fresh rebuild over every input ordering relevant to the contract.

The normalized result includes document and node identities, lineage, synthetic root, parent identity, sibling/preorder positions, heading/local-body/section ranges, canonical slices and hashes, manifests and tombstone/deletion semantics. Comparing only revision IDs is insufficient.

## Deterministic learning replay

A versioned fixed learning corpus covers canonical event families, compensation and redaction events, Offline ingest and Offline evaluation. Incremental reducer/application results are compared with a from-zero replay using canonical sequence ordering and upcasters.

The normalized projection and semantic fingerprint include session/work-item state, accepted/provisional decisions, Evidence, mastery, review, misconception, Offline winner and relevant checkpoints. Response loss, retry, worker restart and replay cannot duplicate Attempt, Assessment, Decision, Evidence, Inbox or Outbox facts.

## Identity and Outbox PostgreSQL boundaries

Real PostgreSQL identity tests prove one-time pairing under concurrency, atomic device/token/code writes, recovery with the same code after injected rollback, and linearization between committed device revoke and concurrent authentication or Offline submission. Offline store tests present a signed/request credential epoch that differs from the locked persisted epoch and prove fail-closed rejection with no writes. Because no production epoch-rotation operation exists, this gate does not simulate one with direct SQL.

Real PostgreSQL Outbox tests prove enqueue identity, claim/lease ownership and terminal compare-and-set behavior. If a consumer commits an idempotent business side effect and fails before Outbox finalize, reclaim converges to one business side effect and one legal terminal state. Faults in claim, attempt, defer, dead, cancel and applied writes leave no partial sibling writes and do not consume the operation identity.

## Candidate gate composition

The aggregate qualification orders checks from cheapest to most expensive:

1. lock/index/schema and runner syntax validation;
2. deterministic unit and package checks;
3. production fake-model vertical;
4. serial real PostgreSQL shards, including deterministic parity and identity/outbox boundaries;
5. Offline real CLI black-box and supported native evidence;
6. Fast Note Sync pinned real-container gate;
7. Nocturne locked OCI and Compose gate;
8. aggregate evidence verification and index generation.

Existing complete typed-record, Offline write-group, Privacy scrub, grant, purge and generation-fence matrices remain authoritative when their evidence matches the current candidate. The aggregate runner does not create duplicate approximate tests merely to increase execution count.

## Operational limits

External registry, Docker daemon, disk or supported platform absence is reported as `blocked`; it does not prove or disprove product behavior. Unrun macOS or Windows native external-service evidence stays `not-run` unless actually produced on that platform.

The gate qualifies repository behavior and pinned integrations. It does not promise availability of external providers, erase data already sent to a model provider, or certify unpinned future releases.

```

# Acceptance scenarios

## Scenario: Fail-closed evidence

A clean-worktree candidate run records complete input identities, environment, actual target test events and redacted log hashes. Missing prerequisites, zero matches, all skips, malformed evidence, changed inputs or missing logs cannot produce or reuse a pass.

## Scenario: Fixed dependencies and isolated resources

PostgreSQL, Nocturne and Fast Note Sync accept only approved exact locks and share a host qualification lock while retaining isolated work, database, container and evidence resources. A lock change invalidates the affected evidence and requires its real contract to run again.

## Scenario: Production model vertical

The fixed corpus runs baseline and candidate profiles through the programmable fake service, production model client/adapter, learning service and real PostgreSQL. Schema drift, low confidence, timeout, rate limit, retry exhaustion and later success preserve the defined authority, provenance and retry invariants.

## Scenario: Knowledge rebuild parity

For unchanged, add, edit, move, reorder and delete corpus operations in relevant input orders, incremental snapshot construction and an independent fresh rebuild produce the same normalized full tree, identities, lineage, ordering, ranges, slices, hashes, manifests and deletion semantics.

## Scenario: Learning replay parity

A versioned corpus covering canonical, compensation, redaction and Offline events produces the same normalized projection and semantic fingerprint under incremental reduction and from-zero replay. Response loss, retry and restart do not duplicate authoritative facts.

## Scenario: Identity and Outbox atomicity

Real PostgreSQL proves one-time pairing concurrency, rollback, committed-revoke fences and zero-write rejection of signed/request credential-epoch mismatches, plus Outbox reclaim after a committed idempotent side effect but failed finalize. Injected write failures leave no partial siblings and converge to one legal terminal result.

## Scenario: Aggregate qualification

The aggregate index lists every required deterministic, PostgreSQL, Offline, fake-model, Fast Note Sync and Nocturne gate as passed, failed, blocked, not-run or validly reused with its evidence key. The candidate passes only when every required gate is passed or validly reused.

## Scenario: Operational limits and privacy

Unavailable registries, daemons, disk or platforms remain blocked/not-run and never imply pass. Durable logs and evidence exclude credentials and learner or knowledge content; Obsidian desktop automation and real model-provider calls remain outside qualification.
