# 平台基础完整规格

## 服务边界

`platform-foundation` 提供单进程 Go 服务、PostgreSQL、HTTP、设备身份、模型网关和 Outbox 基础。后续业务包通过端口使用这些能力，不绕过应用层直接依赖 transport 或数据库实现。

## 目录

```text
server/
├── cmd/edu-agentd/
├── internal/
│   ├── app/
│   ├── identity/
│   ├── integrations/llm/
│   ├── transport/httpapi/
│   └── platform/
│       ├── config/
│       ├── observability/
│       ├── outbox/
│       └── postgres/
├── api/openapi.yaml
└── migrations/
deploy/compose.yaml
contracttests/fakellm/
```

业务端口由消费方定义。`identity` 拥有配对码、设备和令牌。`platform/postgres` 只提供连接、事务和 migration 支持。`platform/outbox` 拥有外部写入意图的通用投递状态，不拥有具体业务 payload 语义。`integrations/llm` 实现模型调用契约。`transport/httpapi` 只完成 HTTP 映射。

## 配置与启动

服务从环境变量读取监听地址、public base URL、PostgreSQL URL、模型 base URL、模型名、模型密钥、上下文窗口、超时和安全开关。缺失必需配置、URL 无效、上下文窗口低于 profile 要求或监听策略不安全时，启动返回可诊断错误。

默认监听 `127.0.0.1:8080`。配置非 loopback 地址时，public base URL 必须是 HTTPS。仅当显式启用开发用途的不安全开关时允许非 loopback HTTP，服务在启动日志和健康状态中持续暴露安全告警。

服务按顺序加载配置、日志、PostgreSQL、migration、应用服务和 HTTP server。关闭时停止接收请求，等待有界时间内的在途操作并关闭数据库连接。

## PostgreSQL 与 migration

第一版只维护 PostgreSQL 方言。migration 是按版本排序、不可重写的 SQL 文件。应用启动可以检查 migration 状态，开发 Compose 可以执行 migration；生产不会静默忽略失败 migration。

基础 schema 包含设备、配对码和 Outbox 所需表。时间使用 UTC。关键幂等约束由数据库唯一索引保证。敏感 payload 与最小审计元数据使用不同字段，为后续 redaction 留出边界。

## 健康状态

`/livez` 只证明进程事件循环可响应。`/readyz` 检查必要依赖并返回组件化 JSON。PostgreSQL 不可用时 readiness 失败。模型端点未配置或探测失败时按配置返回 degraded 或 not-ready，不能伪装为健康。响应不泄露连接串、密钥或内部错误堆栈。

## 配对与设备令牌

配对码只能通过服务器本地管理命令生成。配对码至少包含 128 bit 随机熵，默认十分钟过期，只能成功消费一次，并具有失败尝试上限。数据库只保存配对码哈希、过期时间、消费时间和尝试计数。

设备提交配对码与设备显示名后换取至少 256 bit 的随机 bearer token。令牌只在签发响应中出现一次，数据库保存 SHA-256 哈希、设备身份、作用域、创建时间、最后使用时间和吊销时间。认证使用常量时间比较。吊销立即阻止后续请求，且不影响其他设备。

认证 middleware 解析 bearer token、执行设备和作用域检查、更新受节流的最后使用时间，并把身份放入请求 context。配对与认证失败按客户端/IP 限流，错误响应不区分可用于枚举的内部原因。

## 模型 profile

模型网关面向 OpenAI-compatible Chat Completions 风格接口。必需能力是 system/user/assistant 消息、非流式响应、可解析的结构化 JSON、稳定错误分类、请求超时和配置的最低上下文窗口。原生 JSON Schema、流式响应和工具调用是可协商能力，工具调用不作为核心教学实现的前提。

探测返回 capability 与明确的不兼容原因。fake model server 覆盖成功、无效 JSON、schema 不匹配、401、429、5xx、超时和连接失败。模型密钥只进入 Authorization 请求头，日志和错误必须脱敏。

## Outbox

Outbox 记录业务类型、聚合 ID、幂等键、单调 revision、generation、payload、状态、可重试时间、尝试次数、最后错误类别和时间。状态至少支持 pending、processing、applied、dead。领取使用 PostgreSQL 锁避免同一消息并发执行。

具体适配器负责目标幂等，通用 worker 负责有界指数退避、jitter、尝试上限和死信。旧 revision 或旧 generation 是否可应用由消费适配器与业务 fence 共同判断。

## HTTP 与 OpenAPI

OpenAPI 描述健康、配对、设备查询/吊销和模型能力探测的基础端点。错误使用稳定机器码、用户可读消息和 request ID。响应不暴露密钥、哈希或内部数据库细节。

## 部署

`server/Dockerfile` 采用多阶段构建并使用非 root 运行用户。`deploy/compose.yaml` 启动 PostgreSQL 和 Go 服务，使用健康检查、持久卷和环境变量，不在仓库写入真实密钥。`deploy/env.example` 只包含占位符和安全说明，用户将其复制到被忽略的私有 `.env` 后通过 Compose 的 `--env-file` 使用。

## 验证

单元测试覆盖配置、监听策略、配对生命周期、令牌哈希/吊销、认证、限流、日志脱敏、模型错误分类和 Outbox 状态转换。HTTP 测试使用 `httptest`，模型契约使用 fake server。PostgreSQL 集成测试在可用环境中运行 migration、唯一约束、并发配对消费和 Outbox 领取。

本机缺少 Docker 时，Compose 启动检查必须明确记录为未运行；静态解析与 Go 测试不能替代真实容器验收。
