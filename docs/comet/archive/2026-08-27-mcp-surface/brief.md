# Outcome

在现有 Go 服务的同一监听地址提供受认证的 MCP Streamable HTTP 入口 `/mcp`。MCP 复用 HTTP/CLI 已使用的 identity、knowledge、learning、tutoring 与 memory 应用服务和 PostgreSQL/Nocturne composition，使通用 Agent 看到并修改的是同一份业务真值，同时不能绕过 Assessment 接纳、知识修改审批或 Memory Candidate 准入策略。

# Scope

- 使用官方 `github.com/modelcontextprotocol/go-sdk`，固定版本 `v1.7.0`。
- 在现有 HTTP server 上挂载无状态 Streamable HTTP `/mcp`；每次请求重新认证 Bearer device token，不引入独立进程、端口、数据库、Nocturne client 或 namespace。
- 建立 transport-neutral 的 MCP descriptor catalog 与 invocation gateway。每个 descriptor 固定名称、读写属性、required scope、privacy owner、输入/输出上限和审计名称。
- gateway 在进入 SDK handler 前完成请求体上限、认证、scope、设备限流、descriptor allowlist、privacy read permit 和审计；privacy permit 必须覆盖 SDK handler、结果序列化和实际 HTTP 响应写出。
- actor/device identity 仅从认证 credential 注入，MCP 参数不得接受或覆盖 device ID、token ID、principal 或 namespace。
- MCP 与对应 HTTP 用例共享稳定错误代码和冲突详情；HTTP status 与 MCP JSON-RPC/tool error 只做 transport 映射。
- 暴露以下只读 resources：
  - `edu-agent://knowledge/head`
  - `edu-agent://knowledge/revisions/{revision_id}/tree`
  - `edu-agent://knowledge/revisions/{revision_id}/export`
  - `edu-agent://tutoring/sessions/current`
  - `edu-agent://tutoring/sessions/{session_id}`
  - `edu-agent://learning/nodes/{node_revision_id}`
  - `edu-agent://learning/projections/status`
  - `edu-agent://memory/records/{memory_id}`
  - `edu-agent://memory/export`
- 暴露以下 tools：
  - 只读：`knowledge.retrieve`、`learning.list_timeline`、`learning.list_routes`、`learning.list_evidence`、`learning.list_reviews`、`memory.list_records`
  - 写入：`learning.create_goal`、`tutoring.create_session`、`tutoring.propose`、`tutoring.apply_action`
- MCP descriptor 的 required scope 与对应 OpenAPI 操作保持一致，并由契约测试防止漂移。
- 为 MCP 增加必要的配置、设计文档、单元测试、跨 transport PostgreSQL 测试和部署说明。

# Non-goals

- 不实现 stdio、WebSocket、独立 MCP 监听端口、OAuth 动态客户端注册或 MCP 自有配对流程。
- 不通过 MCP 暴露 `knowledge.import`、知识 proposal 审批或任何直接 canonical revision 写入；这些由 `knowledge-maintenance` child 交付。
- 不暴露 Assessment confirm/override/invalidate、Memory Candidate 决策或直接 admitted 写入、Memory 删除/重放、privacy、设备管理、NoteSync 和 offline 管理工具。
- 不允许 MCP 直接访问 store、pgx pool、Nocturne remote、Outbox 表或 namespace 配置。
- 不把 MCP wire protocol 写入 OpenAPI；OpenAPI 仍是 HTTP 契约，只作为 scope、DTO 与错误语义的对照来源。
- 不新增 Web UI、Rust CLI、多用户或远程公网暴露承诺。

# Acceptance examples

- A1：使用有效 device token 调用 `/mcp` 可以发现且只能发现本 brief 列出的 resources/tools；`knowledge.import`、Assessment decision、Memory admission/delete、privacy、device、NoteSync、offline 管理和直接 Nocturne 能力不出现在 surface 中，未知名称 fail closed。
- A2：缺失、错误、过期或已吊销 token 在进入应用 handler 前失败；scope 不足返回稳定的权限错误；连续认证失败和连续 invocation 分别触发既有 IP/device 限流语义。
- A3：MCP 写入参数无法指定 actor/device/principal/namespace。`learning.create_goal`、`tutoring.create_session` 与 `tutoring.apply_action` 收到的操作者恒来自认证 credential，并与同一 device 的 HTTP 审计身份一致。
- A4：真实 PostgreSQL 跨 transport 测试中，HTTP 导入或创建的知识/学习状态可立即由 MCP 读取；MCP 创建 goal/session 并执行 action 后，HTTP 查询返回同一 revision、event sequence 和 projection，不产生第二份业务状态。
- A5：通过 MCP 记录低置信、结构不完整或带风险标志的 Assessment 时，现有确定性策略仍使其保持 provisional；MCP surface 不提供确认、覆盖或作废 Assessment 的工具，也不能通过通用 action 绕过该门禁。
- A6：MCP memory resource 与 list tool 使用 app 已 composition 的同一 `memoryService`/exporter 和同一 Nocturne namespace；不存在第二个 Nocturne client。MCP 不能创建伪装成用户陈述的 Candidate，也不能直接 admit、delete 或 replay memory。
- A7：在 knowledge、learning/tutoring 或 memory 结果生成后、HTTP 响应写出前关闭相应 privacy permit 时，响应不得泄露已序列化业务内容，并返回稳定的 privacy barrier 错误；permit 生命周期覆盖 SDK 写出阶段。
- A8：同一代表性 domain/auth/conflict/privacy 错误经 HTTP 与 MCP 返回相同稳定 `code` 和必要 detail；MCP 审计包含 request ID、transport、descriptor、device ID、结果、耗时和 peer，但不包含 token、参数正文、答案、Markdown 或 Memory 内容。
- A9：`/mcp` 采用 SDK `v1.7.0` 的 stateless Streamable HTTP，启用请求取消传播、有限请求体、localhost DNS rebinding 防护和明确的 cross-origin 拒绝策略；不接受未授权 GET/DELETE 或无限请求体。
- A10：composition 测试证明 HTTP 与 MCP 持有同一批 application service 实例，MCP 包不导入 PostgreSQL adapter 或 Nocturne remote；完整 surface 清单、scope 对照、错误对照与敏感日志测试均可重复通过。
- A11：受影响 Go package、`go vet`、定向 race、全 server 测试和 server build 通过；真实 PostgreSQL 跨 transport 测试覆盖至少一个知识读、一个学习读和一个学习写垂直路径。

# Constraints and invariants

- 调用方向保持 `transport -> application/domain -> ports -> adapters`。MCP 是纯协议适配层，不拥有业务规则或业务数据。
- 使用 SDK `v1.7.0` 的 `StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true}`，请求体上限不得高于现有 API 写入上限。
- `/mcp` 只复用现有 server listener 和 shutdown 生命周期；不改变服务默认 loopback 与显式不安全监听规则。
- Bearer token 每个 HTTP request 重新认证，不以 MCP 会话初始化结果代替认证；stateless 模式不保存 server-side MCP session identity。
- descriptor catalog 是公开 surface 的唯一注册来源；resource/tool handler 不能绕过 gateway 单独注册。
- 对应 HTTP 与 MCP DTO 可以有 transport 外壳差异，但业务字段、revision、幂等、expected version 和错误代码不得漂移。
- MCP 不创建或缓存第二份 canonical knowledge、learning projection、Memory 正文或 namespace。
- privacy、认证和限流失败必须 fail closed；未知 JSON-RPC method、tool、resource URI 或 descriptor mismatch 不得进入应用服务。

# Decisions

- 首批只支持同端口 stateless Streamable HTTP，不支持 stdio。这样 device token 的签发、吊销、scope、限流和审计继续遵守现有远程 transport 合同。
- 固定官方 Go SDK `v1.7.0`，不使用 pre-release；启用 JSON response 以获得确定性响应和可控的响应期隐私门禁。
- 读取能力使用 resources 与 read-only tools 的明确组合；分页列表保持 tool 输入，避免把不受控 query string 编码成开放 resource URI。
- MCP 不开放知识写入或人工裁决类能力。`tutoring.apply_action` 只能调用现有状态机和 Assessment policy，不能增加 transport 特例。
- Memory 首批只读。Agent 来源的长期记忆提议需要独立可信 principal 合同，不能复用把内容标记为用户陈述的公共 Candidate 入口。
- gateway 对 `tools/call` 和 `resources/read` 在 SDK 写出前解析 descriptor 并持有对应 owner 的 privacy permit；list/discovery 只返回静态 descriptor 元数据，不读取业务内容。

# Open questions

无。若实现发现 SDK 无法让外层 gateway 在实际响应写出完成前持有 privacy permit，必须返回 Shape 调整协议策略，不得以 callback 内提前释放 permit 的方式降级该不变量。

# Verification expectations

- 单元测试覆盖 descriptor allowlist、scope、actor 注入、认证/限流、错误映射、audit redaction、body/origin/host 防护和禁止能力清单。
- 使用官方 SDK client 做协议级测试，不手写 JSON 字符串假装 MCP 客户端。
- 真实 PostgreSQL 测试复用项目 db-core harness，证明 HTTP 与 MCP 交叉读写同一知识 revision、learning event/projection。
- privacy 并发测试必须在结果生成与响应写出边界触发 barrier，确认响应体不包含业务数据。
- Build 期间先运行受影响 package 的最小检查；稳定候选再运行 server 全测、`go vet`、定向 race、build 和 `git diff --check`。
