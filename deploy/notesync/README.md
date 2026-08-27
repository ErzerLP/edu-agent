# NoteSync candidate gate

Run the real Fast Note Sync compatibility gate from the repository root:

```bash
make notesync-candidate
```

The gate starts a fresh container from the immutable image:

```text
haierkeys/fast-note-sync-service@sha256:15833f15e83cee05794c3fe6028c7e41fd36c787f0d651415cad556579fc379f
```

It binds a dynamic localhost port, stores both upstream data and configuration on container tmpfs, waits at most 90 seconds for health, and removes the container on exit. A host-wide `flock` prevents concurrent candidate containers from sharing machine resources.

The bootstrap uses the real WebGUI registration and login routes to create one unique user and one exact vault. It then creates a manual token restricted to `p:rest c:CLI f:note_r,note_w` and that vault. The candidate drives the production Go `Client`, `Consumer`, and `ReviewService` against the real `3.6.1` routes and verifies:

- version, health, vault, note read/write/list, Bearer authentication, CLI client identity, business envelopes, and exact vault restriction;
- first create-only publication, duplicate create-only conflict, exact readback, update, and list metadata;
- real remote Markdown drift, preview, and explicit `accept_remote` import;
- a committed upstream write whose proxy response is dropped, followed by exact-GET reconciliation without a duplicate write;
- capability/outage dependency handling and stale Outbox suppression through `TestPublicationConsumerCapabilityDependencyAndStaleSuppression`.

The login token and restricted API token remain in shell variables and are passed only to the test process environment. They are not written to project configuration, files, or gate output. Use only a disposable test vault when running the Go test directly:

```bash
cd server
NOTESYNC_REAL_BASE_URL=http://127.0.0.1:9000 \
NOTESYNC_REAL_API_TOKEN='<restricted-test-token>' \
NOTESYNC_REAL_VAULT='<exact-test-vault>' \
  go test -count=1 -v -run '^TestRealUpstreamCandidate$' ./internal/integrations/notesync
```
