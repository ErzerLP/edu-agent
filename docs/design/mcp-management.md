# MCP local management surface

## Outcome

An operator who is already authenticated to the loopback-only `/admin/` console can inspect the active MCP endpoint, copy a bounded client configuration example, create a restricted Agent pairing code, run an authenticated static discovery probe with an existing device token, inspect the descriptor catalog, and review recent redacted MCP invocation metadata without reading server logs.

This is one user result: make the existing always-mounted MCP transport discoverable and operable from the existing local administration console.

## Scope

- Add an `MCP` page to the existing single-page administration UI.
- Add authenticated local-admin API endpoints for the MCP snapshot and discovery probe.
- Derive the displayed resource, resource-template, tool, scope, read/write, privacy-owner, and limit metadata from `server/internal/transport/mcp/descriptor.go`; do not create a second catalog.
- Reuse the existing `agent` pairing profile and one-time pairing-code endpoint.
- Provide a generic Streamable HTTP `mcpServers` example with a `<DEVICE_TOKEN>` placeholder and a bounded pairing-exchange example.
- Run the probe through the real MCP handler with a caller-supplied device token and the static `tools/list` method.
- Keep at most 100 completed MCP invocation summaries in process memory and return at most 50 to the administration UI, newest first.
- Update the MCP and operator design references so they match the current 9-resource and 15-tool catalog.

## Non-goals

- No MCP enable/disable switch or deployment setting.
- No change to `/mcp`, its authentication, scope, privacy, rate-limit, Host, Origin, or application callback behavior.
- No token issuance through the management session and no management-session impersonation of a paired device.
- No bearer-token persistence, echo, browser storage, logging, or insertion into generated configuration.
- No durable audit database, log reader, arbitrary log search, or exposure of MCP inputs and outputs.
- No knowledge approval, Assessment decision, Memory mutation, privacy, device, NoteSync, offline, or Nocturne capability is added to MCP.
- No client-specific claim that one generic configuration schema is accepted by every MCP client.

## Data ownership and security

The MCP descriptor catalog remains the only capability source. The administration adapter receives a read-only management snapshot from the live MCP handler. Recent invocation summaries are process-local observability state owned by the MCP transport and disappear on restart.

Each recent summary contains only completion time, request ID, descriptor audit name, credential-derived device ID, result, stable error code, duration, and peer. It contains no Authorization header, token, JSON-RPC arguments, answer, Markdown, canonical slice, Memory content, or response body.

The MCP page inherits the existing loopback/Host/session protections. The probe endpoint additionally requires exact same-origin `Origin`, the session CSRF token, and the existing management-write rate limit. The supplied bearer token is bounded to 4096 bytes, rejected when it equals the management password, used for one in-process MCP request, and then discarded. The browser clears the probe field after the request and never places the token in page state or browser storage.

## Management contracts

### `GET /admin/api/mcp`

Returns the live management snapshot:

- `available`, MCP endpoint URL, implementation name/version, transport mode, stateless and JSON-response flags, and maximum request size;
- static-resource, resource-template, total-resource, and tool counts;
- the complete descriptor metadata needed by the page;
- generic client configuration inputs with a token placeholder;
- up to 50 recent redacted invocation summaries, newest first.

The endpoint is read-only and requires the existing authenticated admin session.

### `POST /admin/api/mcp/probe`

Accepts only:

```json
{"token":"<existing device token>"}
```

The server invokes `tools/list` through the live MCP handler. The response reports only `ok`, HTTP status, request ID, stable error code, discovered tool count, and duration. It never returns the supplied token or raw MCP response.

### Existing pairing contract

The MCP page calls `POST /admin/api/pairing-codes` with `{"profile":"agent"}`. The resulting one-time code can be exchanged through the existing public pairing endpoint for a device credential with exactly the restricted Agent scopes.

## Page behavior

The MCP page displays:

1. endpoint, implementation version, transport mode, request limit, and catalog counts;
2. a generic remote Streamable HTTP configuration example containing `<DEVICE_TOKEN>` and a copy action;
3. a restricted Agent pairing-code action and a pairing-exchange command that makes the one-time token boundary explicit;
4. a password-style, non-autocompleting probe field and a bounded result summary;
5. the complete catalog grouped by resources and tools;
6. recent invocation metadata with request/device IDs abbreviated in the visual list.

The page participates in the same hash navigation and responsive mobile navigation as the existing pages.

## Vertical delivery batch

Result: an authenticated local operator can bootstrap and diagnose the existing MCP endpoint from `/admin/#mcp` without server-log access or credential persistence.

Scope: MCP transport observation, Admin API adapter, embedded Admin UI, targeted tests, and operator/MCP design documentation.

Deferred: runtime enable/disable configuration, persistent audit storage, client-specific integrations, and remote administration.

Exit criteria:

- the production MCP handler exposes the descriptor-derived snapshot, bounded recent metadata, and authenticated static probe;
- `/admin/#mcp` renders and exercises the snapshot, pairing, config-copy, probe, catalog, and audit paths;
- probe and recent-audit data contain no token or request/response content;
- affected MCP, HTTP admin, and app packages pass tests, vet, diagnostics, build, and diff hygiene checks.

## Acceptance

- A1: the page and API report the mounted `/mcp` endpoint and current `mcp-surface-v1` implementation.
- A2: counts and rows are derived from the live catalog and report 4 static resources, 5 resource templates, 9 total resources, and 15 tools for the current revision.
- A3: every displayed descriptor includes name, kind, URI/template where applicable, required scope, read/write classification, privacy owners, and bounded input/output sizes.
- A4: no second hard-coded descriptor name list exists in the Admin backend or frontend.
- A5: the generated generic config uses the exact public endpoint and a `<DEVICE_TOKEN>` placeholder; it contains no real credential.
- A6: the MCP page creates only the existing restricted `agent` pairing profile.
- A7: the probe requires an authenticated admin session, exact Origin, session CSRF token, and the management-write rate limit.
- A8: an existing valid device token produces a successful `tools/list` result with the current tool count.
- A9: an invalid/revoked token produces a bounded failure summary without echoing the token or raw MCP body.
- A10: the management password cannot be submitted as the MCP probe credential.
- A11: neither frontend state nor browser storage retains the probe token, and the input is cleared after each attempt.
- A12: recent invocation metadata is newest-first, process-local, concurrency-safe, capped at 100, and the UI response is capped at 50.
- A13: recent metadata and normal audit logging contain only the approved descriptor-scoped fields and no input/output content.
- A14: the new page follows the existing authenticated asset, hash-navigation, session-expiry, no-store, CSP, and responsive layout contracts.
- A15: MCP protocol behavior and the forbidden descriptor set remain unchanged.
