# Nocturne Compose E2E gate

Run the isolated A88/A89 deployment gate with an OCI layout that already passed the locked build:

```sh
contracttests/nocturne/run-compose-e2e.sh /absolute/path/to/deploy/nocturne/output/oci-layout
```

The command verifies `supply-chain.lock.json` and every OCI blob against `image.lock.json`, imports the offline image with `skopeo`, creates an isolated Compose project and volumes, generates temporary credentials in a mode-0700 directory, and always removes containers, volumes, the imported tag, and temporary secrets through an EXIT/HUP/INT/TERM trap.

The versioned driver checks all four service health states, bridge and maintenance authorization, the negative route allowlist, real PostgreSQL-backed Nocturne create/read/update/unlink/orphan/history/permanent-delete behavior and table absence, cross-account database denial, Nocturne-down degraded readiness plus teaching and queued-memory replay, encrypted backup headers, wrapped-key destruction, retention prune, and SIGTERM shutdown. Focused Go restore fixtures run before Compose to prove restore succeeds with a live key and fails after key destruction.

Prerequisites are Docker with Compose v2, `skopeo`, Python 3, and Go. The gate intentionally creates its own project name, host port, secrets, and volumes. It must not be pointed at a shared Compose project or database.
