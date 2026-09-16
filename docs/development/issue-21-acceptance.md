# Issue #21 验收记录

## 确认与范围

起点 `81a3031`。实施前已通过代码定位确认：设置能力固定 `not_implemented`、HTTP 无运行/事件/命令/操作查询入口、迁移无运行存储，共享核心只有宿主协议；不是已有功能改名。具体文件行号、开发方案和验收标准见 [运行设计](../design/mentor-runtime.md)。

实现目标内导师的共享核心宿主、PostgreSQL 队列/租约/checkpoint/操作回执/有界 outbox、Cookie API/SSE、隐私清除和 React 面板。只提供有界目标理解与澄清，不提供自动教学、研究、改写或永久历史管理。

## 已执行的服务端证据（2026-09-15）

| 检查 | 结果与覆盖 |
| --- | --- |
| `cd server && go test ./...` | 通过；这次全包执行未设置数据库地址，不能将其中的数据库 skip 计为通过 |
| 受影响包 `go vet`，`go build ./...` | 通过；后者为 Go 常规构建，不是带真实 Web 资产的发行构建 |
| PostgreSQL 17.11 上 `go test -race -p=1 ./internal/mentorrun ./internal/transport/httpapi -run 'Test(CheckpointEncryptionAndToolBoundary\|PostgreSQLMentor)' -timeout 90s -count=1` | 通过；使用已有专用测试数据库中的独立随机 schema，逐包串行，执行后删除本次 schema |
| `TestPostgreSQLMentorRealCoreRecoveryAndIdempotency` | 真实 HTTP 模型夹具 → 共享 Runner → 读取绑定目标工具 → 加密持久结果；同操作重试、载荷冲突、服务对象重建恢复、不重放 |
| `TestPostgreSQLMentorLeaseCompetitionAndUnknownResult` | 双 worker 承领唯一、到期接管、旧租约 fencing、已发出调用不自动重试与结果/费用未知 |
| `TestPostgreSQLMentorInteractionBudgetAndStaleApproval` | 结构化批准、精确交互与版本、预算暂停、显式增加预算、拒绝旧回应 |
| `TestPostgreSQLMentorCompatibilityFallbackCannotBypassBudget` | HTTP 协议兼容回退也受真实请求预算约束；额度不足无第二次请求 |
| `TestPostgreSQLMentorTemporaryClearAndCursorExpiry` | 临时正文不写 checkpoint、不同进程不能恢复正文、清除及无效游标 |
| `TestPostgreSQLMentorInFlightCancellationAndLifecycleFences` | 在途停止、目标暂停/归档、设备撤销及 scope 收回，模型终止且不继续调用 |
| `TestPostgreSQLMentorExpiredCursorAndSnapshotCommitGate` | 事件窗口淘汰、过期游标、发送正文期间撤销等待锁，撤销后读取被拒绝 |
| `TestPostgreSQLMentorPrivacyBarrierClearsSavedAndInFlightTemporary` | 真实全局隐私 grant/barrier/local scrub；清除保存正文、在途临时正文、事件、载荷摘要和内存缓存 |
| `TestPostgreSQLMentorCookieHTTPAndSSERecovery` | 真实 Cookie 配对、非默认学习区/目标创建、CSRF、幂等、共享核心调用、快照到订阅补读、事件无正文、跨区拒绝、原操作查询和日志边界 |
| OpenAPI 合同与配置测试 | 运行快照/事件/回执符合合同，路由包含身份与 scope，密钥/流超时配置校验 |
| `git diff --check`、测试启动脚本 `node --check` | 通过 |

初次新增测试中两处夹具问题已修正：生命周期测试改用追加目标修订，遵守既有不可变历史约束；HTTP 模型夹具补全共享客户端要求的 `choice.index`。不通过修改生产协议或绕过历史保护来让测试通过。超时遗留的本次测试 schema 已按准确名称清理。

## 尚未验收，不计通过

- 前端锁定依赖尚未安装，安装授权待回复：还需 `npm ci`、`npm run generate`（更新 `schema.d.ts`）、类型检查、Vitest、格式化和构建。当前提交候选中的前端代码尚不能视为已完成构建。
- Playwright `mentor.spec.ts` 已编写，但真实浏览器执行及整进程重启恢复尚未运行。Go 对象重建和 HTTP 测试不冒充浏览器/进程证据。
- 真实提供商 smoke 未运行；没有发送私人正文到外部提供商或产生已知外部费用。夹具测试不代表真实提供商验收。
- 未推送、未创建 PR、未合并基础分支；整体前端验收前不宣称 Issue 已关闭。
