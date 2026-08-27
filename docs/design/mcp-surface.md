# MCP surface technical reference

## Runtime shape

The server exposes one MCP endpoint at `POST /mcp` on the existing `edu-agentd` listener. It does not create another process, port, database, namespace, PostgreSQL pool, or Nocturne client. The app composition constructs HTTP and MCP from the same `httpapi.Options` service instances, rate limiters, logger, and `privacy.ReadPermitManager`.

The protocol implementation is fixed to `github.com/modelcontextprotocol/go-sdk v1.7.0` and uses:

```go
mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{
    Stateless:                    true,
    JSONResponse:                 true,
    PropagateRequestCancellation: true,
    MaxRequestBodyBytes:          1 << 20,
})
```

Stateless mode keeps no server-side MCP identity session. Every HTTP request is independently authenticated. GET and DELETE are authenticated first and then rejected by the SDK with `405 Method Not Allowed`; successful responses do not carry `Mcp-Session-Id`.

The one MiB request limit is no greater than the existing learning write limit. The gateway reads at most that amount, restores the exact body for the SDK, and rejects JSON-RPC batches. The SDK applies the same limit again while decoding.

## Descriptor catalog

`server/internal/transport/mcp/descriptor.go` is the only public surface source. The server registration loop derives SDK resources, resource templates, tools, schema, annotations, scope, privacy owner, audit name, and input/output limits from this catalog.

### Resources

| MCP resource | Kind | HTTP operation | Scope | Privacy owner | Output limit |
| --- | --- | --- | --- | --- | ---: |
| `edu-agent://knowledge/head` | static | `getKnowledgeHead` | `knowledge:read` | knowledge | 4 MiB |
| `edu-agent://knowledge/revisions/{revision_id}/tree` | template | `getKnowledgeRevisionTree` | `knowledge:read` | knowledge | 16 MiB |
| `edu-agent://knowledge/revisions/{revision_id}/export` | template | `exportKnowledgeRevision` | `knowledge:read` | knowledge | 16 MiB |
| `edu-agent://tutoring/sessions/current` | static | `getCurrentTutoringSession` | `learning:read` | learning, tutoring | 4 MiB |
| `edu-agent://tutoring/sessions/{session_id}` | template | `getTutoringSession` | `learning:read` | learning, tutoring | 4 MiB |
| `edu-agent://learning/nodes/{node_revision_id}` | template | `getLearningNode` | `learning:read` | learning, tutoring | 4 MiB |
| `edu-agent://learning/projections/status` | static | `getLearningProjectionStatus` | `learning:read` | learning, tutoring | 4 MiB |
| `edu-agent://memory/records/{memory_id}` | template | `getMemoryRecord` | `memory:read` | memory | 4 MiB |
| `edu-agent://memory/export` | static | `exportMemoryRecords` | `memory:read` | memory | 16 MiB |

`memory.export` returns the first bounded export page and includes `next_cursor` when more data exists. Pagination of admitted record metadata is available through `memory.list_records`.

### Tools

| MCP tool | HTTP operation | Scope | Privacy owner | Read-only | Input / output limit |
| --- | --- | --- | --- | --- | ---: |
| `knowledge.retrieve` | `retrieveKnowledge` | `knowledge:read` | knowledge | yes | 256 KiB / 16 MiB |
| `learning.list_timeline` | `listLearningTimeline` | `learning:read` | learning, tutoring | yes | 256 KiB / 4 MiB |
| `learning.list_routes` | `listLearningRoutes` | `learning:read` | learning, tutoring | yes | 256 KiB / 4 MiB |
| `learning.list_evidence` | `listLearningEvidence` | `learning:read` | learning, tutoring | yes | 256 KiB / 4 MiB |
| `learning.list_reviews` | `listLearningReviews` | `learning:read` | learning, tutoring | yes | 256 KiB / 4 MiB |
| `memory.list_records` | `listMemoryRecords` | `memory:read` | memory | yes | 256 KiB / 4 MiB |
| `learning.create_goal` | `createLearningGoal` | `learning:write` | learning, tutoring | no | 1 MiB / 4 MiB |
| `tutoring.create_session` | `createTutoringSession` | `learning:write` | learning, tutoring | no | 1 MiB / 4 MiB |
| `tutoring.propose` | `proposeTutoringArtifact` | `learning:write` | learning, tutoring | no | 1 MiB / 16 MiB |
| `tutoring.apply_action` | `applyTutoringAction` | `learning:write` | learning, tutoring | no | 1 MiB / 4 MiB |

The catalog deliberately has no knowledge import or proposal approval, Assessment decision, Memory Candidate/admission/delete/replay, privacy, device, NoteSync, offline, or direct Nocturne descriptor. Unknown method, tool name, or resource URI fails before any application callback.

## Gateway order

The outer handler performs these steps before the SDK can invoke an application callback:

1. Obtain or generate a request ID and install the MCP audit completion record.
2. Enforce localhost Host-header DNS rebinding protection.
3. Apply Go cross-origin protection; cross-origin unsafe requests are rejected.
4. Read the POST body with the global one MiB limit and restore it byte-for-byte.
5. Parse one JSON-RPC envelope and reject batches, unknown protocol methods, and malformed descriptor parameters.
6. Apply the shared authentication-failure limiter by peer IP.
7. Parse one Bearer token and call the existing identity service for this request. Revoked credentials therefore fail immediately even after a prior MCP discovery request.
8. Apply the shared device invocation limiter using the credential-derived device ID.
9. Resolve `tools/call` or `resources/read` through the catalog, enforce its input limit and required scope, and fail closed on a mismatch.
10. Acquire one read permit covering every descriptor privacy owner and place only the credential-derived identity in context.
11. Run the SDK into a buffered response, then commit the complete response through the permit's ordered owner gates. Privacy closure and actual response writing therefore have one atomic ordering boundary.

Discovery and list methods are authenticated and rate limited but read only static SDK/catalog metadata, so they do not acquire a business privacy permit. Application callbacks never receive a token and never accept device ID, token ID, principal, actor, or namespace input fields.

## Privacy permit lifecycle

The permit is acquired outside the SDK handler after descriptor resolution. Its context becomes the HTTP request context used by the stateless SDK session and application callback. The callback result and SDK JSON-RPC serialization complete in memory. Final response writing uses `ReadPermit.CommitResponse`, which locks the covered owner gates in stable order and revalidates the permit before writing any bytes. `CloseAndDrain` uses the same gates before marking owners closed and cancelling active permits. If response commit wins, closure begins only after the complete response write; if privacy closure wins, commit observes the closed/cancelled permit and writes no business bytes. Gates are owner-scoped, so a slow knowledge response does not serialize unrelated memory closure.

If a privacy barrier wins after application data was produced but before the response commit:

1. `CloseAndDrain` marks the owner closed and cancels the permit context.
2. The gateway discards the complete SDK response buffer.
3. The gateway writes only the stable `content_redacted` problem.
4. The deferred permit release lets `CloseAndDrain` complete.

Tests exercise the close-wins boundary independently for knowledge, learning+tutoring, and memory, and exercise the response-wins ordering at the permit layer. The recorded HTTP body contains the stable error code and none of the previously serialized owner content when privacy closure wins.

## Application callbacks and identity

The MCP package defines consumer-side application interfaces only. It imports no PostgreSQL store, pgx package, or Nocturne integration. Resource and tool callbacks call the same objects passed to HTTP composition:

- knowledge resources and `knowledge.retrieve` call the existing knowledge service;
- learning/tutoring resources and tools call the existing learning service and state machine;
- memory resources and list tool call the existing memory service/exporter composed by the app.

Write actor IDs are read from the authenticated invocation context. The gateway schema and strict decoder reject caller-supplied identity fields. `tutoring.apply_action` maps only existing state-machine actions. Recording an Assessment therefore retains the application's deterministic acceptance result; MCP adds no confirm, override, or invalidate path.

## Error mapping

`server/internal/transport/problem` is shared by HTTP and MCP. It maps domain errors to a stable code, message, HTTP status, and optional top-level detail such as:

- knowledge `current_revision_id` and `identity_review`;
- learning `conflict` and `current_disposition`;
- memory `candidate_conflict`.

HTTP serializes the shared problem as its existing JSON envelope. MCP tools return the same envelope in `structuredContent`, set `isError`, and include the JSON fallback as text. Resource callbacks return a JSON-RPC error whose `data` contains the same envelope. Authentication, scope, rate, Host, Origin, descriptor, and privacy failures occur before the SDK and use the same stable envelope as an HTTP transport error.

MCP audit records contain only request ID, transport, catalog audit descriptor, credential-derived device ID, result, stable error code, duration, and peer. They never contain Authorization, token material, JSON-RPC parameters, answers, Markdown, canonical slices, or Memory content. The existing outer HTTP audit continues to record only method/path/status metadata.

## Verification and environment boundary

Protocol tests use the official SDK client and `StreamableClientTransport` for discovery, tool calls, resource reads, stateless behavior, revocation, cancellation propagation, privacy write-barrier behavior, and cross-transport calls.

A real PostgreSQL cross-transport test uses the existing isolated-schema app harness and covers:

- HTTP knowledge import followed by MCP head/tree reads of the same revision;
- HTTP learning write followed by MCP projection read at the same event high water;
- MCP goal/session/action writes followed by HTTP session projection read at the same aggregate version and event sequence.

The test runs when `TEST_DATABASE_URL` is configured. Without that variable it reports a skip and is not evidence of a PostgreSQL pass. A separate application-level stateful fake test always runs and proves the production composition helper supplies the same service instances to both transports.

No deployment configuration change is required: `/mcp` follows the existing listener, loopback default, explicit insecure non-loopback warning, reverse-proxy TLS guidance, and server shutdown lifecycle.
