# CLI M1 black-box gate

## Candidate PostgreSQL scenarios

The `blackbox` package builds and executes the actual `edu-agent`, `edu-agentd`, strict fake LLM, and response-loss proxy binaries. Each test creates a random PostgreSQL schema, random loopback ports, and two independent CLI configuration homes. Both CLI homes pair through one-time codes created by the real server command. Long-running processes receive SIGTERM and a bounded kill fallback during cleanup.

Run the candidate gate exactly once. It first runs the strict fake LLM and response-loss proxy contracts, then requires PostgreSQL and runs the real black-box scenarios serially:

```sh
TEST_DATABASE_URL='postgres://...' make cli-m1-blackbox
```

The target exits nonzero when `TEST_DATABASE_URL` is absent, after the independent fixture contracts pass. The package-level no-database check remains available with `cd contracttests/cli-m1 && env -u TEST_DATABASE_URL go test ./blackbox`; its PostgreSQL scenarios explicitly skip before binaries are built. With a DSN, the target uses `go test -p=1 -count=1 -v ./blackbox`. PostgreSQL 18 `psql` must be available on `PATH` for isolated schema setup and metadata-only assertions.

Independent scenarios cover:

- accepted pair/import/goal/route/explanation/activity/assessment completion;
- multiline answer input with a non-default allowed help level;
- provisional feedback surviving CLI exit, followed by confirm and override from the second paired CLI;
- free answer, exact attached quiz ownership, feedback return, and explicit resume;
- due review presentation, provisional and void non-advancement, then accepted review Evidence advancement;
- model failure preserving authoritative state and a later successful retry;
- capture-next response loss, exact-body automatic replay, and Inbox/Evidence uniqueness plus the exact four-event `record_assessment` batch;
- accepted Evidence followed by an Agent HTTP knowledge proposal, user CLI approval, provisional evidence carryover, Agent decision rejection, user CLI carryover approval, and one shared HTTP/MCP/CLI terminal state without rewriting the original Evidence.

Harness failures report only process names, exit codes, stable error codes, counts, hashes, IDs, and other metadata. They do not print stdin, Markdown, model output, HTTP raw bodies, answers, tokens, or pairing codes.

The due-review scenario ages one accepted Evidence record and its canonical event payload inside the isolated schema. It then runs a helper built with the `cli_m1_contract` tag to replay canonical events and rematerialize the active projection under the normal event-clock and projection-head locks. The helper is not linked into `edu-agentd` or release binaries; it exists only because direct projection-row edits would invalidate the generation fingerprint and would not prove replay behavior.

## One-shot post-commit response-loss proxy

Run the independent stdlib-only proxy module:

```sh
cd contracttests/cli-m1
go run ./cmd/response-loss-proxy -upstream http://127.0.0.1:8080
```

The default listen address is `127.0.0.1:18082`. For a generated operation ID, arm a safe one-shot capture before the target request. The first matching method/path request atomically binds its canonical lowercase operation ID:

```sh
curl -fsS -X POST \
  -H 'X-Fixture-Control-Key: response-loss-control-key' \
  -H 'Content-Type: application/json' \
  --data '{"method":"POST","path":"/v1/tutoring/sessions/10000000-0000-4000-8000-000000000001/actions"}' \
  http://127.0.0.1:18082/__fixture/capture-next
```

An exact operation ID can also be configured before the target request:

```sh
curl -fsS -X POST \
  -H 'X-Fixture-Control-Key: response-loss-control-key' \
  -H 'Content-Type: application/json' \
  --data '{"method":"POST","path":"/v1/tutoring/sessions/10000000-0000-4000-8000-000000000001/actions","operation_id":"20000000-0000-4000-8000-000000000001"}' \
  http://127.0.0.1:18082/__fixture/rules
```

For a matching request, the proxy buffers the complete first successful upstream response and then closes the client connection without sending response bytes. It retains the first request body only in private process memory for exact byte comparison. A later request with the same key and identical bytes is forwarded normally; different bytes receive HTTP 409 before reaching upstream.

Control endpoints require `X-Fixture-Control-Key`:

- `POST /__fixture/capture-next` arms one method/path capture; `GET` lists pending captures.
- `POST /__fixture/rules` adds an exact rule.
- `GET /__fixture/rules` returns call, upstream-call, drop, and rejection counts.
- `GET /__fixture/audit` returns method/path/operation ID, byte counts, hashes, statuses, and outcomes. It never returns request or response bodies.
- `POST /__fixture/reset` clears rules, retained comparison bytes, and audit state.

Capture-next and exact rules are mutually exclusive for the same normalized method/path. Whichever is configured first owns the path until reset or, for capture-next, until the first matching request atomically binds its operation ID.
