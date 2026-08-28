# Operations candidate coordinator

This stdlib-only Go module is the evidence coordinator behind `scripts/test-operations-candidate.sh`. It reuses the repository's real PostgreSQL, Fast Note Sync, and Nocturne gates; it does not contain substitute business tests.

## Entry point

```bash
# List every required lane without probing Docker or external dependencies.
scripts/test-operations-candidate.sh --list

# Run the complete qualification. Nocturne is fail-closed without this input.
scripts/test-operations-candidate.sh \
  --evidence-dir /absolute/evidence \
  --nocturne-oci-layout /absolute/verified-oci-layout

# Use an explicit Runtime/Verifier key file. It must be outside both the
# repository and evidence directory.
scripts/test-operations-candidate.sh \
  --attestation-key-file /private/state/operations-attestation.key \
  --evidence-dir /absolute/evidence \
  --nocturne-oci-layout /absolute/verified-oci-layout

# Reuse only same-key passed manifests whose signature, strict schema, durable
# log hash, and reparsed coverage all validate.
scripts/test-operations-candidate.sh \
  --resume \
  --evidence-dir /absolute/evidence \
  --nocturne-oci-layout /absolute/verified-oci-layout

# Independently recompute current inputs, manifest keys, signatures, log
# coverage, index bindings, and aggregate status.
scripts/test-operations-candidate.sh verify \
  --evidence-dir /absolute/evidence \
  --nocturne-oci-layout /absolute/verified-oci-layout
```

`--dry-run` performs lane preflight and test enumeration. Missing tools, daemon access, locked platform requirements, or the explicit Nocturne OCI layout are `blocked`; available but unexecuted lanes are `not-run`. Neither state is a pass. The model and Offline black-box lanes additionally require a `psql` client on `PATH`; its version is bound into their evidence keys. Every heavyweight runner clears inherited `GOFLAGS` and emits required `go test -json` events explicitly, so host Go flags cannot alter test discovery or runner helper commands.

## Attestation and schemas

Lane manifests use `edu-agent.operations.evidence/v3`; candidate indexes use `edu-agent.operations.candidate-index/v2`. Both contain a required HMAC-SHA256 attestation. Evidence and index signatures use separate domain strings, and each signature payload excludes only its own signature bytes. A public SHA-256 evidence key, log hash, or qualification key is not an attestation and cannot make a hand-written file reusable.

The default key is `$XDG_STATE_HOME/edu-agent/operations/attestation.key`, or `~/.local/state/edu-agent/operations/attestation.key` when `XDG_STATE_HOME` is unset. The first 32-byte key is published atomically and must remain a regular `0600` file in a private `0700` directory. `--attestation-key-file` lets Runtime and Verifier share an explicit existing path. Key paths inside the repository or evidence tree are rejected. The secret key is never written to a manifest, index, durable log, evidence directory, or repository; only its SHA-256-derived key identifier is recorded.

Every strict read, resume, and verification path verifies the appropriate signature before trusting content. Runtime creates the key when running; verifier mode only loads an existing key and fails closed when it is missing, has unsafe permissions, belongs to another key, or has an invalid signature.

## Evidence binding

Each manifest records the lane attempt, candidate fingerprint, lane/scenario, exact argv/cwd, terminal status and reason, exit state, timestamps, OS/arch/runtime/kernel, applicable tool versions, pinned inputs, selected/executed/passed/failed/skipped targets, required external assertions, and the redacted durable log's SHA-256 and byte count. Writes use a same-directory temporary file, `fsync`, atomic rename, and mode `0600`.

The evidence key binds candidate source, lane/scenario, command, platform, toolchain, approved dependency index, runner digest, canonical host lock path/protocol, image/version/commit/config/OCI inputs, test selection, expected Go targets, external targets, and output assertions. The Fast Note Sync lane additionally binds the SHA-256 of `docs/comet/specs/notesync-bridge/spec.md`; dependency loading rejects any service/plugin version or commit that differs from the single promoted contract in that authority file.

The PostgreSQL runner still executes each configured shard in full. When invoked by the coordinator, it receives a candidate-bound expected-target list separately from the discovered shard selection: every required target must emit its own RUN/PASS event, while an unrelated conditional test may skip without satisfying or invalidating a different external lane. A required target that skips always fails. A standalone explicit `--run` first enumerates matching tests and executes only package import paths that contain a match; an empty selection fails before PostgreSQL starts, while the default shard path remains complete.

A candidate index is independently attested. For every lane, verifier requires the index `evidence_key` to equal both the current recomputed key and the referenced manifest key. Passed/reused entries require a complete attested passed manifest and valid durable log. Unknown fields, truncation, missing logs, hash changes, source/runner/lock/OCI drift, empty selection, all-skip, `[no tests to run]`, or missing RUN/PASS events fail closed.

## Input drift boundary

The coordinator recomputes the candidate fingerprint and lane runner, dependency-lock, host-lock, toolchain, and OCI inputs immediately before each execution, immediately after each execution, around evidence reuse, and once more before signing the final index. Any mismatch changes the lane/index result to `failed`; it cannot leave an aggregate `passed` or `reused` conclusion.

These checks do not create an immutable filesystem snapshot. A malicious process running as the same OS user may change an input and restore the exact bytes between observations. Preventing that requires an external isolation boundary such as a read-only snapshot, separate trusted user, or immutable build sandbox; this coordinator does not claim such isolation.

## Heavyweight lock and cleanup

The coordinator and standalone PostgreSQL, Fast Note Sync, and Nocturne runners all use `OPERATIONS_CANDIDATE_LOCK_FILE`, defaulting to `/tmp/edu-agent-operations-candidate.lock`. A standalone runner acquires the lock itself. When the coordinator already holds it, the locked descriptor is inherited with `OPERATIONS_CANDIDATE_LOCK_FD` and `OPERATIONS_CANDIDATE_LOCK_PROTOCOL=inherited-fd-v1`; runners verify the descriptor path and flock state instead of recursively opening the lock. Fast Note Sync passes the same inherited lock to its nested PostgreSQL shard.

Each coordinator lane receives a unique temporary work directory. Standalone Nocturne defaults its gate and Compose logs to its unique `mktemp` directory and removes that directory deterministically during cleanup; explicit log path overrides remain caller-owned.

## Privacy and external proof limits

Subprocess output is redacted before durable write and hashing. Bearer credentials and labeled token, password, pairing-code, answer, knowledge, content, payload, vault, and secret values are replaced as complete quoted, multiword, or JSON values. Valid JSON is recursively sanitized by key. When a value boundary cannot be determined safely, the entire line becomes `[REDACTED unsafe log line]`, the redactor marks it unsafe, and the lane fails.

The Fast Note Sync runner proves the exact pinned service image and observed service version through the real container contract. The promoted service commit and Obsidian plugin version/commit are compatibility contracts bound from the authoritative spec. The container run does not prove an Obsidian plugin binary and must not be described as doing so.
