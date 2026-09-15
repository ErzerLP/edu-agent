# 共享 Go Agent 核心（Issue #19）

## 问题确认

工作树起点 `82573c7`，包含工单基线 `bc55267`，初始工作区干净。
服务端运行 `go list -m github.com/edu-agent/edu-agent/packages/agentcore` 失败：`not a known dependency`。
迁移前 `clients/cli-go/internal/agentcontroller/controller.go:589` 实际调用 `agentloop.New`；
`internal/agentloop/types.go:17` 的 Model 使用 CLI internal 模型协议，`:133` 的 Options 同时依赖本地文件、制品和执行管理器；
`internal/agentloop/execution.go:20` 的循环和 `:145` 的工具调度处于同一 internal 包。
不存在独立共享模块，也没有其他公开入口或设置实现同等能力。直接让 server 导入这些 internal 包违反 Go 的包边界。

项目架构已核对：server 的 app 组合 identity、learning/tutoring、knowledge、memory、privacy 和 HTTP/MCP，PostgreSQL 保存领域事实；
CLI 的 command/dashboard → agentcontroller → agentloop 承担交互会话，agentcontext 通过正式 API 绑定学习上下文；
agentui 负责 TUI，agentsession/keybackend/modelsecret 负责本地加密和凭据，workspace/localexec 负责本机能力；
Web 当前提供学习身份和目标工作台，没有待复用的 Go Agent 核心。

## 开发方案（实施前确定）

这是一个有界基础重构，保持单任务，串行修改公共接口和 CLI 接线。

1. 新建独立 `packages/agentcore` Go module，迁移模型协议及可选 HTTP/SSE 适配器、资源预算和确定性上下文投影。
2. 从生产执行路径提取模型轮次调度，通过明确的上下文、历史、工具和事件接口注入宿主行为；CLI 调用共享调度器。
3. 本地工作区、Shell/PTY/任务、凭据查找、加密格式、恢复检查、审批与副作用 WAL 留在 CLI。
   共享模块不导入任一客户端或服务端 internal，不初始化 OS provider，也不注册默认工具。
4. 提供只按宿主注册工具执行的服务适配边界、交互协议与确定性 fake 测试；测试必须实际驱动公共核心。
5. 两个消费模块使用仓库内相对 `replace`，`GOWORK=off` 仍独立构建；根 Makefile 和 Docker 构建上下文包含共享模块。
6. 补充公共 API、生命周期、依赖与平台边界说明，记录实际验收范围。

核心不拥有学习数据库或模型凭据来源。工具参数、输出和模型事件不授予身份、scope 或审批；服务端工具闭包固定真实主体并调用正式应用命令。
历史接口只提交消息与读取投影，不通过重新执行工具恢复状态。CLI 原有 Observer/Reflector 的领域证据有效性和历史存储仍由 CLI 适配器维护。

## 验收标准

- CLI 生产 send/resume 经共享核心，原有文本、多轮工具、等待交互和流式活动回归在迁移前后均可运行。
- 核心独立测试正常回答、多轮、交互暂停/继续、取消、无响应超时、非法载荷、重复调用身份、工具集合隔离及上下文上限。
- 保持默认无固定轮数及用户可选保护；输入、协议参数、输出、上下文额度相互独立，原错误分类不降级。
- CLI 的 provider+endpoint 凭据绑定、no-save、加密恢复、发送确认、旧工作区只读、授权重置、终端清理及本地任务回归保持通过。
- 核心与 server、CLI 均可独立测试、vet、构建，无跨模块 internal 导入、循环依赖或绝对 replace。
- 恢复历史不会执行工具；工具输出不进入系统指令或成为授权；伪造工具和批准字段不扩权。
- 分开记录核心纯逻辑、CLI 生产适配回归与实际平台证据。未运行的数据库、真实 provider、Keychain 或平台矩阵不宣称通过。

验收记录见 [Issue #19 验收](../development/issue-19-acceptance.md)。
