# Fake Chat Completions server

Run from the repository root:

```sh
go run ./contracttests/fakellm/server.go
```

The server listens on `127.0.0.1:18081` and requires `Bearer fake-development-key` by default. Set `FAKE_LLM_MODE` to `success`, `invalid-json`, `schema-mismatch`, `unauthorized`, `rate-limited`, `server-error`, or `timeout`. Connection failure is covered by stopping the server. Address and key can be overridden with `FAKE_LLM_ADDR` and `FAKE_LLM_API_KEY`.
