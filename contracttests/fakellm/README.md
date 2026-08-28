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

Request kinds are `capability_probe`, `route`, `activity`, `assessment`, `free_answer`, and `explanation`. Scenario kinds are `accepted`, `provisional`, `risk`, `malformed`, `malformed_envelope`, `schema_mismatch`, `rate_limited`, `http_error`, `timeout`, `unauthorized`, and `no_native_schema`. `http_error` requires `status_code` in the 500-599 range. Activity scenarios may set `activity_type` and `allowed_help`; assessment scenarios may set `assessment_conclusion`. Route scenarios may set `route_step_limit` from 1 to 1000 to return a stable prefix of the canonical node revision IDs supplied by the server; zero keeps all supplied IDs.

Every proposal independently and strictly decodes `go-cli-context-v1`; the fixture does not call a production request validator. It binds proposal IDs to the work-item goal, route step, activity, attempt, free question, and free answer authorities as applicable. Retrieval must contain exactly the complete `node_revision_ids` set with no duplicate node authority. Every hit must carry canonical lowercase knowledge, document, node, and node-revision IDs, a valid UTF-8 nonempty canonical slice, an exact lowercase SHA-256, and a nonempty byte range whose length equals the slice byte length. Missing, duplicate, stale, hash-mismatched, or out-of-range context fails closed before consuming a programmed scenario.

Route intent/conditions, activity prompt/references, assessment quotes, free answers, and explanations are all generated from the validated canonical references. Returned model references include the validated node revision ID, source range, and slice hash. Assessment output additionally requires the work-item activity reference set and rubric to match the validated retrieval exactly.

Control endpoints:

- `PUT /__fixture/scenarios/{request_kind}` installs a sequence.
- `GET /__fixture/scenarios` returns configured programs.
- `GET /__fixture/audit` returns request metadata and hashes, never message/input text, authorization tokens, or request headers. Valid Chat Completions requests include `protocol_profile=openai-chat-completions-v1`, the requested model ID, structured response format, request kind/ID, scenario, status, byte count, and request SHA-256. A timeout cancelled by the production client records status `0`.
- `POST /__fixture/reset` clears scenarios, cursors, and audit state.

The real PostgreSQL production vertical consuming this control surface is exactly `TestBlackBoxProductionFakeModelVerticalPostgreSQL` in `contracttests/cli-m1/blackbox`. It runs the same schema mismatch, confidence, retry, exhaustion, and later-success corpus for baseline `operations-baseline-v1` and candidate `operations-candidate-v2`; absence of its PostgreSQL prerequisite is a skip and never pass evidence.
