# Programmable strict Chat Completions fixture

Run the standalone fixture:

```sh
cd contracttests/fakellm
go run .
```

It listens on `127.0.0.1:18081`, requires `Bearer fake-development-key`, and exposes `/v1/chat/completions`. The command has no server production-code imports; the module's server dependency is used only by adapter contract tests.

`FAKE_LLM_MODE` keeps the existing `success`, `invalid-json`, `schema-mismatch`, `unauthorized`, `rate-limited`, `server-error`, `timeout`, and `no-native-schema` modes. It also accepts `accepted`, `provisional`, `malformed`, and `risk:<risk_flag>`. `FAKE_LLM_ADDR`, `FAKE_LLM_API_KEY`, `FAKE_LLM_CONTROL_KEY`, and `FAKE_LLM_TIMEOUT_MS` override command defaults.

For multi-step black-box scenarios, configure a sticky sequence per request kind. The last scenario repeats after the sequence is consumed:

```sh
curl -fsS -X PUT \
  -H 'X-Fixture-Control-Key: fake-control-key' \
  -H 'Content-Type: application/json' \
  --data '{"sequence":[{"kind":"rate_limited","retry_after":"0"},{"kind":"accepted"}]}' \
  http://127.0.0.1:18081/__fixture/scenarios/assessment
```

Request kinds are `capability_probe`, `route`, `activity`, `assessment`, `free_answer`, and `explanation`. Scenario kinds are `accepted`, `provisional`, `risk`, `malformed`, `malformed_envelope`, `schema_mismatch`, `rate_limited`, `http_error`, `timeout`, `unauthorized`, and `no_native_schema`. `http_error` requires `status_code` in the 500-599 range. Activity scenarios may set `activity_type` and `allowed_help`; assessment scenarios may set `assessment_conclusion`.

Control endpoints:

- `PUT /__fixture/scenarios/{request_kind}` installs a sequence.
- `GET /__fixture/scenarios` returns configured programs.
- `GET /__fixture/audit` returns request metadata and hashes, not message content.
- `POST /__fixture/reset` clears scenarios, cursors, and audit state.
