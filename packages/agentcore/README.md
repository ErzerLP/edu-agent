# 共享 Go Agent 核心

模块路径：`github.com/edu-agent/edu-agent/packages/agentcore`，Go 版本见 `go.mod`。
核心只依赖 Go 标准库，不导入 server 或 CLI 的 internal。
模型轮次、普通 HTTP/SSE 协议与上下文预算来自现有 CLI；CLI 的生产 `Session.run` 调用 `Runner[Result]`。

## 公共 API 与注入方式

| 接口或类型 | 宿主职责 |
| --- | --- |
| `Model` / 可选 `StreamingModel` | 实现模型请求；模型协议位于 `modelclient`。HTTP 适配器的 `New` 显式接收 endpoint、model、API key、超时和 HTTP transport，不查环境凭据或系统钥匙串 |
| `ContextSource` | 从已提交历史构造请求；调用 `ContextPlanner.Plan` 预算系统规则、工具 schema、用户输入、完整工具组及输出；用 `ObserveUsage` 校准与更新状态 |
| `HistorySink[R]` | 实现助手消息追加和最终回答提交；控制持久化、WAL、提交与取消的线性化点；存储失败直接返回，不能继续执行工具 |
| `ToolExecutor[R]` | 按顺序处理当前响应调用，提交有界的 `role=tool` 结果；待用户交互时返回 `ToolStep{Continue:false, Result:...}`，保留剩余调用 |
| `EventSink` | 同步接收 `RunEvent`，映射到应用自己的输出流；事件不代表授权。sink 必须及时返回，不保存可读推理；展示 panic 不改变业务结果 |
| `RoundBudget` | 单个用户 turn 的可选保护，初始化 `Limit:n, Remaining:n`，交互恢复沿用同一个预算；nil 或 Limit=0 不限制轮数 |
| `PendingQuestion` / `QuestionAnswer` | 与 CLI 共用的问询协议；响应只能来自已鉴权的真实交互入口，不能由模型消息替代 |

`Runner[R].Run(ctx)` 使用迭代循环，准备上下文、调用模型、校验响应、提交助手消息、执行工具，然后继续或返回最终/交互结果。
`R` 为宿主结果类型，允许 CLI 保留文件确认、学习流程导航等专有 DTO，核心不依赖这些领域类型。

宿主在调用 Runner 前验证并提交用户输入、建立 turn 及 dirty intent；Runner 不创建第二套 Session store。
取消使用传入的 context；模型和工具适配器必须响应它。最终提交后不会因迟到取消把已完成结果改成未执行。
错误原样传播，包含 `context.Canceled`、`context.DeadlineExceeded`、`modelclient.ClientError` 和 `ContextError`。
协议校验在宿主自定义模型返回的消息上同样执行，不能把 `system` 角色冒充助手响应。

可运行示例见 [`runner_test.go`](runner_test.go) 的 `memoryAdapter` 和
[`server 的消费契约`](../../server/internal/integrations/agentcore/agentcore_test.go)：它直接使用公共核心完成正式应用工具适配器的测试循环，不启动 CLI。
CLI 的对应接线见 [`execution.go`](../../clients/cli-go/internal/agentloop/execution.go)。

## 服务工具集合与信任边界

`NewToolSet(timeout, registrations...)` 提供可选的命令白名单适配器。空注册集合没有任何工具，`shell`、`sql`、`task` 等名称也没有特殊执行能力。
宿主创建的 `ToolRegistration.Execute` 闭包绑定真实用户、scope、学习区及正式应用命令。不得将模型自报身份/批准字段映射成宿主授权。
使用 `DecodeArguments` 拒绝非对象、未知字段、重复字段、null 和尾随载荷，再运行正式命令对必填、枚举、实体归属等参数的校验。
schema 用于模型协议说明，不取代实际权限与输入校验。注册目录与返回给调用方的目录使用独立 schema 副本。

`ToolSet.Invoke` 仅调用白名单，按调用 ID 防止重复执行，限定工具超时及 8 KiB UTF-8 结果。
结果始终是数据，宿主必须将 `Content` 作为 `role=tool` 加入历史，不得提升为系统指令；更大结果由应用负责投影或引用。
模型协议的字节边界与工具结果、上下文预算分别生效。`agentlimits` 中特定工具名的参数字节额度仅为 CLI 兼容资源策略，不构成注册或授权。

需要问询时返回 `ToolOutput{Question:..., Resume:...}`。Invoke 只向展示方返回独立问询数据，不暴露续行闭包。
真实用户入口调用 `Resolve(callID, answer)`；核心核对调用身份、问题 ID 和选项，取消不会调用续行函数。
执行前消费待处理状态，重复/迟到回答不能再次触发副作用。若执行后发生取消、超时或结果未知，宿主通过正式命令的操作 ID/WAL 核对，不能重新执行恢复。
CLI 已有文件/偏好审批、未知结果与重试协议继续由 CLI 的 `ToolExecutor` 实现，不改用此可选适配器。

## 生命周期、历史与平台边界

- Runner 不创建后台 goroutine、数据库、Shell/PTY、工作目录或任务 manager；模型流式读取和事件回调在当前调用中进行。
- 调用方串行化同一会话的 send/resume/close，取消正在运行的 context 后回收自己创建的资源。`ToolSet` 的调用身份和待处理表有互斥保护，但不替代应用的会话串行化与持久幂等。
- 恢复只读取已验证的历史和投影，不扫描旧 tool call 并执行。新建 ToolSet 后用 `ReserveCallIDs` 预留检查点中的旧身份；不恢复旧交互闭包或批准状态。
- 系统规则、工具目录和主体来自当前宿主配置；历史证据标记为旧观察，需要重新读取当前事实。`ContextPlanner` 保留工具 call/result 对及受保护历史；只投影请求，不修改存储原文。`off` 模式超限明确报错。
- 自动记忆的 Observer/Reflector、服务证据撤销和工作区新鲜度策略仍由 CLI 的上下文适配器维护，已提交投影通过 `ContextMemoryProjection` 注入预算器。核心没有自己的目标、mastery、Evidence 或知识数据库。
- CLI 保留 provider+endpoint 凭据绑定、恢复发送确认、no-save、Session 加密、旧工作区只读、授权重置、终端字符清理、文件发布和任务生命周期。系统 Keychain 和本地工具仅由 CLI 初始化。
- HTTP 普通响应使用请求超时；SSE 使用可续期的无响应超时，收到字节会续期，总响应时长不被误当成空闲超时。自定义 Model 应实现同样的取消/超时合同。

## 独立构建与测试

仓库保留三个独立 module，无需 `go.work`。server 和 CLI 的 `go.mod` 通过仓库内相对 `replace` 引用本模块，版本 `v0.0.0` 是仓库源码依赖占位，不依赖未发布的网络版本。
完整检出仓库后，在任一模块目录使用 `GOWORK=off go test ./...`、`go vet ./...`、`go build ./...` 即可。
若只打包 server 或 CLI 源码，必须同时带上相同仓库相对位置的 `packages/agentcore`；Dockerfile 已复制此依赖。

根目录 `make test/vet/build` 包含核心，`make agentcore-test agentcore-test-race agentcore-vet agentcore-build` 可单独验证。
核心没有外部模块依赖，因此无需 `go.sum`。核心 HTTP/SSE 测试使用本地 fake transport/server，不访问真实模型供应商。
CLI 的安全恢复、终端和真实本地工具回归留在原模块。验收范围见 [Issue #19 验收记录](../../docs/development/issue-19-acceptance.md)。
