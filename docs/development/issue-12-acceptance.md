# 学习工作台贯通验收（Issue #12）

## 基线与范围

2026-09-10，用户确认前置任务已经开发并推送。`task/22` 从已接受的
`68d507d` 快进到 `a34e39b`，包含 #2–#11 的实现；不改写此前只读调查结论，
不修改或推送 `main`。GitHub 跟踪页的复选框不作为代码或验收证据。

架构沿用 Go CLI 的工作台/Agent → command 应用动作 → HTTP/MCP →
knowledge、learning、tutoring owner → PostgreSQL。资料范围绑定不可变版本，
教学通过显式会话定位，导入任务另有加密文件暂存。客户端页面状态不是业务权威。

本批只有一个结果：核对前置能力组成的完整工作台，补充可重复的贯通测试和证据，
修复实际复现的集成缺陷。各模块规则仍以对应设计和独立 Issue 为准；不重写
导入、规划、评分或权限规则，不改 Shell 权限，不代替 Runtime 归档。

## 开发方案

1. 核对各模块的正式入口、既有测试及验证边界，建立以下场景与证据映射。
2. 在独立 PostgreSQL 测试环境中运行现有真实服务/生产命令场景；各数据库检查串行。
3. 补充工作台与真实服务之间缺少的组合验证。发现缺陷时先保留失败断言，
   记录原因，再作最小修复；已有通过的前置场景不改写为本批实现。
4. 运行相关测试、必要的并发检查及服务端/CLI 的测试、vet、构建，记录 skip
   和未运行的平台/模型范围。只暂存本批文件，在任务分支提交供审查。

## 验收标准与初始证据入口

| #12 场景 | 验收标准 | 现有证据入口/需要补充的部分 |
| --- | --- | --- |
| 两区三目标 | Go 两目标和英语一目标独立，目标保存不创建教学会话 | 目标黑盒、进度 PostgreSQL；补充真实服务工作台组合 |
| TUI 导入 | 扫描、预览和确认真实可见；两区同名资料互不覆盖 | 导入 PTY、正式预览黑盒；补充跨区组合 |
| 切换续学 | 切换后原路线、题目、已提交状态保持 | ScopedSessionMaterialsAndFocus、工作台教学草稿隔离 |
| 重启与双客户端 | 已确认事实可按原会话恢复，不争抢 current | 显式会话 PostgreSQL、生产进程重启黑盒 |
| 迟到响应 | 响应留在原请求身份，不覆盖新页面/目标 | 工作台取消/迟到消息、原客户端范围、教学 F2 测试 |
| 共享资料与复习 | 新资料版本不改旧题引用；真实证据/复习任务不重复 | 冻结范围、会话资料、复习去重与重放 PostgreSQL |
| 无模型与规划 | 无模型可管理；草稿可编辑，采用需要明确确认 | 无模型目标黑盒、PlanningGenerateEditConfirmAndRecovery、Agent 正式流程 |
| 大批导入恢复 | 中断后原任务可查，结果与未提交可区分，重试不重复发布 | ImportJobsProcessRestartAndLostResponse、LargeManifest |
| 工作台与 Agent | 使用同一服务端归属和状态，按名称导航 | AgentLearningRealServiceWorkflows；补充真实工作台入口 |
| 迁移与平台 | 旧 ID/历史不被重写；PostgreSQL、Linux/macOS 范围明确 | 迁移、离线归属测试；Linux PTY，macOS 原生单独记录 |

## 本批复现与修复

核查到两处前置模块之间的集成缺陷，均先运行失败断言再修改生产代码。

1. `a34e39b:clients/cli-go/internal/command/workbench_goals.go:88` 在读取正式目标前
   使用空 `g.GoalID` 拼接 Agent 入口。`TestWorkbenchLibraryGoalAndExplicitSessionFlow`
   新增断言实际得到 `agent:/`，期望 `agent:<当前目标ID>/`，测试失败。
   修复为读取成功后随目标详情构造入口，沿用原 Agent 导航和服务端绑定核验。
2. `a34e39b:clients/cli-go/internal/command/workbench.go:243` 从全屏动作调用
   `learnDiagnostic`；同文件第 30 行将输出设为 `io.Discard`，而
   `learn.go:253` 向外部 `Terminal.Confirm` 读取确认。结果是用户看不到路线，
   全屏程序与传统终端争用输入。`TestWorkbenchRoutePreviewRequiresVisibleConfirmation`
   实际返回 `planning_confirmation_required`、空页面和一次外部终端确认，测试失败。
   修复将现有路线提案生成提取为结构化共享动作，工作台显示原提案，再由原容器确认后
   调用正式 `apply_route`。提案 ID 和会话版本跟随页面，不再次生成另一条路线；取消不写入，
   过期版本拒绝。命令行仍使用自身的既有确认流程。

两项回归及 `TestDiagnosticRequiresExplicitRouteConfirmation` 修复后均通过。
没有改服务端、公共协议、迁移、身份审批、路线应用规则或 Shell 权限。

## 实际执行结果（2026-09-10）

环境：Linux/amd64，Go 1.26.6，独立 PostgreSQL 17 容器与 `issue12` 测试库。
复用本机已有镜像
`pgvector/pgvector@sha256:cf134a767f474095eeba57e0117be8e568e011a63f33fbf252f14c9b760f8e6f`，
没有安装软件或连接运行中的业务数据库。测试镜像包含 pgvector 不代表启用了模型检索。
黑盒需要的 psql 由同一固定镜像提供临时客户端，包装脚本与二进制不进入仓库。
数据库场景串行执行，各测试沿用既有隔离/清理机制。

证据绑定本提交的客户端源码/测试及服务端 `a34e39b`；本文的历史说明和计划不作为通过证据。

| #12 场景 | 本批实际证据与结论 |
| --- | --- |
| 两区三目标 | 新增 `TestWorkbenchRealServiceTwoSpacesAndThreeGoals` 通过：真实 HTTP/PostgreSQL，工作台保存 Go 两目标及英语一目标，保存后该目标零教学会话，随后逐个明确创建；目标详情保留名称、冻结范围及 Agent 目标 ID。生产黑盒 `TestBlackBoxGoalsWithoutModelPersistenceAndIndependentLifecycle` 亦通过。 |
| TUI 导入同名资料 | 新增测试的 Go/英语子场景均通过：真实 Linux PTY/Bubble Tea → 同一工作台导入叶子 → 客户端扫描各自 README.md → 服务端预览（确认前 head 不存在）→ Ctrl+S 提交 → 全屏返回资料页；两个版本 ID 不同，分别导出对应正文。 |
| 切换续学 | `TestPostgreSQLScopedSessionMaterialsAndFocusSurviveSwitch`、`TestPostgreSQLExplicitSessionsRemainIndependent` 通过，验证真实路线、题目、作答、焦点与重放；工作台会话/活动草稿隔离测试通过。前者直接调用生产应用服务及模型 fixture，不冒充完整人工 TUI 课程。 |
| 重启与双客户端 | 新增工作台测试用新的 App 实例通过原 ID 恢复第一会话的版本和阶段；目标与导入生产黑盒实际重启服务/CLI，持久状态不丢。显式会话数据库测试验证不同目标独立，客户端原有 proposal 回源测试通过。 |
| 迟到响应 | `TestCancellationAndLateResponse`、`TestEmbeddedLeafLateMessagesAndSmallResize`、`TestOriginClientKeepsScopeAfterUISelectionChanges`、教学 F2 迟到结果及导入迟到结果测试通过；工作台取消、草稿隔离和新真实服务场景定向 race 通过。 |
| 冻结版本与复习去重 | `TestPostgreSQLKnowledgeSpacesIsolationAndFrozenVersions`、`TestPostgreSQLScopedSessionMaterialsAndFocusSurviveSwitch`、`TestPostgreSQLReviewTasksKeepIndependentGoalsAndReplay` 通过；范围与旧题保持原资料，真实目标复习任务及重放保持独立。 |
| 无模型与规划 | 无模型目标生产黑盒、新真实工作台测试通过；`TestPostgreSQLPlanningGenerateEditConfirmAndRecovery`、Agent 正式规划流程通过，编辑后明确确认。新路线回归验证工作台展示、取消、旧版本拒绝和确认原提案，零外部终端输入。 |
| 大批导入恢复 | `TestPostgreSQLImportJobRestartLostResponseAndLargeManifest` 通过，累计正文超过旧请求预算、分批结果与原操作回执真实持久；`TestBlackBoxImportJobsProcessRestartAndLostResponse` 通过，生产进程重启及实际 HTTP 响应丢失后继续，不重复发布。 |
| 工作台与 Agent | 新真实工作台测试与 `TestAgentLearningRealServiceWorkflows` 通过；后者覆盖注册工具、真实导入 PTY、目标/规划页面、两区读取、范围检索、跨区与旧版本拒绝、no-save 切区和归档限制。目标页丢失绑定的缺陷由本批修复。 |
| 迁移与平台 | `TestLearningSpaceUpgradePreservesLegacyGoal`、`TestKnowledgeSpaceUpgradePreservesLegacyRevision` 通过；同批 scoped 会话测试覆盖冻结离线授权跨区同步、重复回执及重放归属。本批不修改迁移、旧事件或签名格式。Linux 原生 PTY 通过；macOS amd64/arm64 交叉构建通过，原生 macOS 未运行。 |

完整普通检查：在 `server` 和 `clients/cli-go` 分别运行
`go test ./... && go vet ./... && go build ./...`，均通过。这两个命令没有设置数据库变量，
其中集成测试的 skip 不作为数据库证据；真实数据库证据来自表内另行显式运行的检查。
`git diff --check` 通过。

## 重跑入口

先构建当前 `server/cmd/edu-agentd`，设置其绝对路径为 `EDU_AGENT_INTEGRATION_SERVER`，
设置独立测试库连接为 `TEST_DATABASE_URL`。不要使用业务库。随后在 CLI 模块执行：

```sh
go test -race ./internal/command -run '^Test(WorkbenchRealServiceTwoSpacesAndThreeGoals|AgentLearningRealServiceWorkflows)$' -count=1 -v
go test ./internal/command -run '^Test(WorkbenchLibraryGoalAndExplicitSessionFlow|WorkbenchRoutePreviewRequiresVisibleConfirmation|DiagnosticRequiresExplicitRouteConfirmation)$' -count=1 -v
```

在服务端模块执行本批 PostgreSQL 组合：

```sh
go test -p=1 ./internal/learning/postgresstore ./internal/knowledge/postgresstore ./migrations -run 'Test(PostgreSQLExplicitSessionsRemainIndependent|PostgreSQLScopedSessionMaterialsAndFocusSurviveSwitch|PostgreSQLProgressScopePaginationAndReplay|PostgreSQLReviewTasksKeepIndependentGoalsAndReplay|PostgreSQLPlanningGenerateEditConfirmAndRecovery|PostgreSQLKnowledgeSpacesIsolationAndFrozenVersions|PostgreSQLImportJobRestartLostResponseAndLargeManifest|LearningSpaceUpgradePreservesLegacyGoal|KnowledgeSpaceUpgradePreservesLegacyRevision)$' -count=1 -v
```

确保 psql 可调用后，在 `contracttests/cli-m1` 模块执行生产黑盒：

```sh
go test -p=1 ./blackbox -run '^TestBlackBox(GoalsWithoutModelPersistenceAndIndependentLifecycle|ImportPreviewConfirmedBatchAndOperation|ImportJobsProcessRestartAndLostResponse)$' -count=1 -v
```

## 验证边界与交付

没有运行真实外部模型、Nocturne/NoteSync 远端、完整数据库故障矩阵或 macOS 原生 TUI/钥匙串。
服务端规划/教学使用确定性模型 fixture；Agent 模型只触发正式工具，不伪造业务结果。
组合测试与分层证据不等同于一段使用真实模型、跨平台人工完成全部课程的录像。
macOS 原生确认仍需在对应环境执行，交叉构建不能替代。

本批交付贯通测试、两处已复现缺陷的修复及本记录，在 `task/22` 提交供审查。
不修改 GitHub Issue 状态、复选框或 Runtime 归档，不合并或推送 `main`。
