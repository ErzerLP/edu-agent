# Agent 学习上下文与正式流程（Issue #11）

## 确认与前置接入

首次调查已确认缺失并保持工作树干净。2026-09-09 用户确认前置推送后，任务分支快进至 `27ff41d`，接入 #2–#10 的已发布合同。没有修改或推送 main。

前置后的缺口：基线 `27ff41d` 的 `clients/cli-go/internal/agentloop/read_tools.go:65`、`:129` 为进度、复习设置 `Global: true`，`:84` 读取 `CurrentSession`；`tools.go` 注册的八个业务工具没有目标草稿、导入、规划入口，Agent Session 与 checkpoint 也没有学习区/目标绑定，非默认区 Agent 入口仍被禁止。通过 `git show 27ff41d:<路径>`、工具注册和会话创建/恢复代码交叉确认，这不是已有设置的别名。目标、规划、资料预览已有正式 API 和页面，本项只负责接入。

## 开发方案

1. 使用不可变学习区、可选目标和教学会话身份创建 Agent。服务端正式查询核对目标与会话归属；目标不由模型或最近更新时间选择。历史无绑定明确映射固定默认区，不按标题推断。新身份进入新聊天，已有聊天恢复原绑定。
2. 绑定保存在加密记录、索引和 checkpoint 中。恢复和切换为目标会话重建绑定客户端；来源账本与压缩随原 checkpoint 保存。沿用既有 generation 丢弃迟到 UI 回调，保留原端点确认、隐私代次、恢复不重放、授权重置和 no-save。
3. 读取正式按区目标、进度和复习服务；目标检索使用正式冻结范围，无范围明确提示选择资料。无教学会话不选择 current。全局偏好继续使用固定默认区的已有记忆服务，不引入新的长期记忆协议。
4. 对话提供正式流程入口，在客户端明确选择目标、文件和预览。导入、目标编辑及规划使用现有页面与 API，模型不能提供确认布尔值或把本地路径当上传。返回真实业务状态；失败、取消和过期不报告成功。
5. 工作台启动 Agent 传递当前区与显式目标/会话；Agent 页及 picker 展示绑定、按区过滤。切区不改写原聊天身份，不新增 Shell 权限限制。

## 验收标准

- 两区三目标：进度、复习、资料与教学路线只读明确绑定；无目标可交流，具体目标操作进入选择。
- 创建、恢复、picker 切换及无保存均保持绑定；旧历史固定默认区；端点确认、加密和恢复不重放回归通过。
- 迟到读取、审批和上下文事件只作用于原会话；跨区 checkpoint 不可安装，隐私清除不恢复旧业务正文。
- 实际注册工具进入正式导入、目标和规划流程；本地扫描与用户选择先于上传，确认前正式业务不变，服务端拒绝跨区 ID、版本冲突及过期草稿。
- 受影响 package、必要 race、服务端/CLI test、vet、build 与 Linux/macOS 构建通过；真实服务、模型 fixture、原生平台和真实模型验证分别记录，不把未运行写为通过。

## 验证记录

2026-09-09，Linux：

- CLI 执行 `go test ./...`；仅帮助文案参数顺序的兼容断言失败，修正后 `go test ./internal/command ./internal/agentcontroller ./internal/agentui` 全部通过，其余包复用全量通过结果。包括旧格式迁移、加密、端点确认、恢复不重放、Shell、4096 上下文预算及文件批准回归。
- CLI `go vet ./...` 通过；最后改动的 command/controller/UI 再次通过 vet。服务端 `go test ./...`、`go vet ./...` 和 `edu-agentd` 构建通过；该无数据库全量命令中的集成测试跳过，不计为数据库验收。
- agentcontext/controller/loop/UI 的绑定、隐私代次、切换、迟到回调及压缩定向 `go test -race` 通过。真实服务链路另以 `-race` 通过。
- 独立 PostgreSQL 测试库运行 `TestAgentLearningRealServiceWorkflows`：正式工具注册、目标编辑保存、规划草稿编辑并确认采用、完整 Agent TUI→导入 PTY→客户端扫描→服务端预览→用户确认→真实发布结果；确认前没有已发布版本。两区三目标的注册工具读取、冻结资料检索、明确目标选择、跨区 ID 拒绝、旧目标版本拒绝、no-save 切区保留工作区、归档后禁止发布全部通过。测试启动独立服务并自行关闭，不连接业务服务。
- 同一独立数据库串行运行服务端 `TestPostgreSQLPlanningGenerateEditConfirmAndRecovery`、`TestPostgreSQLProgressScopePaginationAndReplay`、`TestPostgreSQLScopedSessionMaterialsAndFocusSurviveSwitch`，全部通过；覆盖规划生成 fixture 与正式确认/恢复、聚合分页和教学冻结上下文。
- CLI `CGO_ENABLED=0` 构建 `linux/amd64`、`darwin/amd64`、`darwin/arm64` 通过。Linux 全屏终端桥接使用真实 PTY 验证；跨编译不等于 macOS 原生验证。

真实服务测试复跑入口（从 CLI 模块执行）：

```sh
TEST_DATABASE_URL='<独立 PostgreSQL 测试库连接>' \
EDU_AGENT_INTEGRATION_SERVER='<本分支构建的 edu-agentd 绝对路径>' \
go test -race ./internal/command -run '^TestAgentLearningRealServiceWorkflows$' -count=1 -v
```

模型验证边界：Agent 使用确定性模型 fixture 发起注册工具，业务返回来自真实 HTTP 服务和 PostgreSQL；服务端规划生成使用现有规划模型 fixture。未接入真实模型，不声称已验证自然语言选工具效果。未执行 macOS 原生 TUI、真实系统钥匙串的 opt-in 平台证据或完整数据库矩阵；原钥匙串实现未改动。本轮未接入可选 #7 后台导入任务恢复。

交付保持在 `task/21`，未推送或合并 `main`。学习区仅是业务上下文，不是 OS 沙箱；切区不复制批准，不收窄 Shell 权限。
