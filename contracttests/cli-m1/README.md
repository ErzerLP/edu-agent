# CLI M1 black-box fixtures

## One-shot post-commit response-loss proxy

Run the independent stdlib-only proxy module:

```sh
cd contracttests/cli-m1
go run ./cmd/response-loss-proxy -upstream http://127.0.0.1:8080
```

The default listen address is `127.0.0.1:18082`. Configure the exact method, path, and canonical lowercase operation ID before the target request:

```sh
curl -fsS -X POST \
  -H 'X-Fixture-Control-Key: response-loss-control-key' \
  -H 'Content-Type: application/json' \
  --data '{"method":"POST","path":"/v1/tutoring/sessions/10000000-0000-4000-8000-000000000001/actions","operation_id":"20000000-0000-4000-8000-000000000001"}' \
  http://127.0.0.1:18082/__fixture/rules
```

For a matching request, the proxy buffers the complete first successful upstream response and then closes the client connection without sending response bytes. It retains the first request body only in private process memory for exact byte comparison. A later request with the same key and identical bytes is forwarded normally; different bytes receive HTTP 409 before reaching upstream.

Control endpoints require `X-Fixture-Control-Key`:

- `POST /__fixture/rules` adds an exact rule.
- `GET /__fixture/rules` returns call, upstream-call, drop, and rejection counts.
- `GET /__fixture/audit` returns method/path/operation ID, byte counts, hashes, statuses, and outcomes. It never returns request or response bodies.
- `POST /__fixture/reset` clears rules, retained comparison bytes, and audit state.
