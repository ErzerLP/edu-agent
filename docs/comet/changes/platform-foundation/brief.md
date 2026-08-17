# Outcome

建立教学 Agent 的可运行 Go 服务基础，使后续知识、学习、记忆和客户端 child 能在稳定的 PostgreSQL、认证、HTTP、模型与 Outbox 契约上实现。

# Scope

本 child 负责仓库和 Go 服务骨架、配置与启动生命周期、PostgreSQL 连接和 migration、健康检查、一次性配对码、设备令牌、统一认证与限流、OpenAI-compatible 模型 profile、fake model server、事务 Outbox 基础设施、OpenAPI 基础契约、Dockerfile 和 Compose 开发部署。

# Non-goals

本 child 不实现 Markdown 导入与树索引、教学状态机、Learner Model、Nocturne 业务适配、完整 CLI、离线同步、Fast Note Sync 或 MCP。它只提供后续模块需要的稳定端口和基础设施。

# Acceptance examples

- Parent A1：本 child 交付可启动 Go 服务与 PostgreSQL 的部署实现、默认 loopback 与非 loopback 安全策略、分层健康接口和静态契约；本机缺少 Docker 时明确记录未运行，真实 Compose 联合启动证据保留为 Supervisor 最终 A1 验收门槛。
- Parent A2：本地管理命令可生成高熵、短时、一次性配对码；设备换取独立令牌后可认证，配对码不能重放，令牌只存哈希并可按设备吊销和限流。
- Parent A3：fake server 可验证明确的 OpenAI-compatible profile，不兼容端点返回可诊断错误，模型密钥不进入日志、导出或客户端响应。
- Parent A25：本 child 不引入 Rust CLI、Web UI、PDF/网页解析、多用户或高级个性化调度。
- Parent A26：代码按业务能力与 Ports/Adapters 组织，transport 只负责协议转换、认证、校验和错误映射。
- Parent A27：`identity` 与平台基础设施拥有明确数据边界，服务保持单 Go 进程且不引入 RPC、服务发现、分布式事务或消息中间件。
- Parent A31：PostgreSQL Outbox 提供至少一次投递所需的幂等键、单调 revision、generation、重试与死信状态基础。
- Parent A60：OpenAPI 是 HTTP 契约，模型 profile 明确消息、结构化 JSON、错误、超时和上下文要求，工具调用不是必需能力。
- Parent A71：服务默认监听 loopback，非 loopback 部署需要 HTTPS public URL，显式不安全开关产生持续告警。
- Parent A72：配对码具有高熵、短 TTL、一次性和尝试限流，设备令牌仅签发一次并在数据库保存哈希、身份、作用域和吊销状态。
- Parent A73：模型和外部服务凭据只保存在服务端配置中，HTTP 进入应用用例前执行统一认证、授权、速率限制与审计。

# Constraints and invariants

Go 服务使用 PostgreSQL 作为唯一生产数据库方言。数据库操作使用显式 SQL/pgx，不引入跨业务万能 Repository。HTTP handler 不直接访问数据库，应用端口由消费模块定义。

随机密钥由 `crypto/rand` 生成。配对码和设备令牌不以明文持久化。日志使用结构化字段和集中脱敏，不记录 Authorization、API key、配对码或令牌。

# Decisions

Go module 使用单仓库 `server` 模块。HTTP 使用 `net/http` 与 `chi`，PostgreSQL 使用 `pgx/v5`，migration 使用版本化 SQL。配置来自环境变量并在启动时完成严格校验。

模型 profile 基于 Chat Completions 风格 `/v1/chat/completions`。原生 JSON Schema 和流式输出通过 capability 协商；结构化 JSON、超时、错误分类和最低上下文配置是必需契约。

# Open questions

无。范围严格继承已确认的 Supervisor Shape。

# Verification expectations

运行 Go 格式化、静态检查、单元测试、race test 和 HTTP/fake model 契约测试。若本机没有 Docker，则记录 Compose 容器启动检查未运行，并对 Compose、Dockerfile、migration 和配置进行静态检查；不能把未运行描述为通过。
