# Issue #19 验收记录

## 确认、方案与完成范围

环境：Linux/amd64、Go 1.26.6，工作树起点 `82573c7`（包含工单基线 `bc55267`）。
修改前工作区干净。确认命令在 server 中运行：

```text
go list -m github.com/edu-agent/edu-agent/packages/agentcore
go: module github.com/edu-agent/edu-agent/packages/agentcore: not a known dependency
```

结合生产入口 `agentcontroller/controller.go:589` → `agentloop.New`、原 `agentloop/types.go:17,133` 的 internal 协议/本地依赖，以及原 `execution.go:20,145` 的循环/工具调度，确认公开共享边界缺失，不是已有功能的其他设置。
先记录 [开发方案与验收标准](../design/shared-agentcore.md)，再实施。

完成：独立共享 module、生产 CLI 调度接线、模型通信及预算投影迁移、问询 DTO、注入端口、服务命令白名单适配器、跨模块依赖与构建配置、文档及确定性测试。
共享模块只依赖标准库，不引入本地工具 provider 或领域数据库。原有模型通信和资源限制测试随实现迁移；CLI 生产、安全及平台测试保留。
共享核心在自定义模型接口返回处拒绝伪造角色和非法 UTF-8，避免宿主把外部文本当系统指令。

## 迁移前后对照

修改生产代码前运行下列七项 `internal/agentloop` 测试，全部通过；接线后以相同表达式再次运行，全部通过。

```bash
go test ./internal/agentloop -count=1 -run 'Test(SessionSupportsUnlimitedToolLoopAndOptionalUserLimit|SessionExecutesKnowledgeToolBeforeAnswer|PreferenceConfirmationAdmitsPendingPersonalContext|StreamingModelPublishesDeltasAndCommitsOnlyCompleteAssistant|CancellationStopsFirstModelReadToolAndPostToolModel|ContextPlannerOffAndTypedBudgetFailures|ThinkingLifecycleAfterTools)$'
```

迁移前首条命令也包含 modelclient/agentcontroller 两个包，但该表达式在这两个包没有匹配测试，不将其记作迁移前行为证据。
通过的对照分别覆盖不限轮数/用户保护、工具后回答、长期偏好确认、流式增量/最终提交、取消、上下文错误及工具续行的活动顺序。

## 实际执行的检查

| 检查 | 结果与范围 |
| --- | --- |
| 核心 `GOWORK=off go test ./... -count=1` | 通过；文本、65 轮工具、用户保护、交互暂停兄弟调用/续行、存储故障、取消/工具超时、非法载荷、重复/历史身份、默认无工具、严格参数、结果边界和预算投影 |
| 核心 `GOWORK=off go test -race ./...` | 通过；包括迁移的普通 HTTP、SSE、空闲超时、流式读取、长输出和 usage 测试 |
| 核心 `GOWORK=off go vet ./...`、`go build ./...` | 通过；`go list -deps ./...` 确认项目依赖只涉及 agentcore 自身，没有 CLI/server internal 或 TUI/OS provider |
| CLI `go test ./internal/agentloop ./internal/agentcontroller ./internal/agentui -count=1` | 通过，接线后的受影响包完整回归 |
| CLI `GOWORK=off go test ./...` | 通过；未配置真实外部依赖的可选测试按项目规则 skip，不算对应外部行为通过 |
| CLI 定向 race（命令如下） | 通过；轮次活动、流式交互、取消、切换、恢复、本地任务与 PTY |
| CLI `GOWORK=off go vet ./...` | 通过 |
| server `GOWORK=off go test ./...`、`go vet ./...`、`go build ./...` | 通过；数据库相关测试因 `TEST_DATABASE_URL` 未配置而 skip，此处是非数据库候选证据；构建为 Go 开发模式 |
| server `TestServerRunsSharedCoreWithApplicationToolOnly` | 通过；直接导入公共核心并运行模型→应用工具→最终回答，Shell 不在注入能力集合 |
| 根 `make agentcore-build cli-build` | 通过；构建核心与 Linux CLI，执行 `bin/edu-agent version` 和 `agent --help` 成功 |
| CLI `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 GOWORK=off go build -o bin/edu-agent-darwin-arm64 ./cmd/edu-agent` | 通过，仅交叉构建，不代表 macOS 原生运行通过 |
| server 重跑原缺失命令 | 成功解析 `agentcore v0.0.0 => ../packages/agentcore`；`go list` 公共包成功 |
| `git diff --check` | 通过 |

CLI 定向 race 命令：

```bash
GOWORK=off go test -race ./internal/agentloop ./internal/agentcontroller ./internal/agentui \
  -run 'Test(Thinking|Streaming|FinalAnswerCommit|Cancellation|Activity|Reasoning|LiveReasoning|LocalExecution|PTY|Controller.*(Switch|Resume|NoSave|Provider)|ArchiveRestore)'
```

`TestLargeContextDefaultRequestsAndLongResponses` 等原有生产适配回归也在完整 CLI 测试中通过，覆盖共享核心与真实 HTTP 适配器连接 fake provider 的路径。
新交互测试曾发现展示结果与内部待处理选项共享切片；已改为两份独立副本，并通过篡改选项、错误调用 ID、合法回答和重复回答验证。没有通过放宽断言跳过问题。

## CLI 安全与原生证据

完整 CLI 测试保留以下具名回归及其相关测试：

- `TestModelCredentialsAreScopedToProviderEndpoint`、远程端点无 Key 失败关闭。
- `TestAgentNoSaveSkipsSessionStoreAndTitleRequest`、`TestAgentCommandProviderChangeZeroSendUntilConfirmationAndLocalManagement`。
- `TestAgentCommandMissingHistoricalWorkspaceRestoresConversationWithoutCWDFallback`、切换后授权重置、加密恢复及 WAL 不重放。
- `TestLocalExecutionModelLoopAndMetadataOnlyCheckpoint`、`TestPTYLocalTerminalPortsAndEncryptedRecovery`、文件发布后取消保留事实与终端清理。

Linux 真实密钥后端另在 `dbus-run-session` 隔离会话中运行，使用隔离 XDG 目录 `/tmp/edu-agent-issue19-native.CuvaES` 和临时 GNOME Keyring，未改变用户模型配置或用户登录钥匙串：

```bash
EDU_AGENT_NATIVE_KEYBACKEND_TEST=1 GOWORK=off go test \
  ./internal/keybackend ./internal/agentsession -run '^TestNativePlatform' -count=1 -v
```

`TestNativePlatformSecretRoundTripCleanup` 与 `TestNativePlatformAgentSessionPrivacyClear` 均通过，测试自行清理随机凭据。
Linux 文件/Shell/PTY 真实本地回归包含在 CLI 测试中；这与 fake provider/存储逻辑测试分开记录。

## 未运行范围

没有运行 PostgreSQL、真实模型供应商、完整 CLI+真实服务黑盒、Docker/Compose 或 Web 发行资产重建。
前端依赖未安装，本次未运行会安装这些依赖的 `make server-build`/完整 `make build`；已验证各 Go module 的独立构建和根核心/CLI 构建目标。
Dockerfile 的相对依赖复制和 CI 的核心/服务端消费检查已更新，Docker 镜像构建未声称通过。
未进行 macOS 原生 Keychain/TUI 或 Windows 运行验证，也未扩展 Windows Shell 支持。没有因本次重构修改数据库、Web 页面或加密格式。

公共接口、生命周期、平台边界与完整检出依赖说明见 [共享核心 README](../../packages/agentcore/README.md)。
