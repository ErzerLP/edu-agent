# 显式教学会话与续学（Issue #5）

## 确认依据与前置

2026-09-09 将远端 `9f62b95` 快进接入任务分支，已具备 #2/#3/#4 的区、冻结范围及目标生命周期。
`learning/postgresstore/session_view.go` 仍按全局最近事件选择会话；CLI `learning_common.go` 的 proposal 成功与过期路径重新读取 current，将另一会话返回学习循环。指定会话 HTTP 读取已存在，但 `learn` 不接受选择参数，也没有教学会话 picker。Agent Session picker 仅管理本地对话，不是教学续学。

具体基线位置：`9f62b95:server/internal/learning/postgresstore/session_view.go:43` 无区/目标条件；`9f62b95:clients/cli-go/internal/command/learning_common.go:141,150` 在过期/成功时调用 current，`:154-156` 即使发现 session 改变，仍将 B 返回循环。先加入 `TestProposalRefreshKeepsOriginSession` 并运行 `go test ./internal/command -run TestProposalRefreshKeepsOriginSession -count=1`：两分支均失败，来源 A 返回 B，current 调用 1、指定读取 0；修复后均为只读 A。

收尾增加 `TestAnswerResponseLossDoesNotReplaySubmission`：模拟 504，修改前 HTTP 答案调用 2 次，修改后 1 次；命令层对丢响应查询原 session，不重放答案。

## 开发方案

1. 服务端按区和目标查询教学会话，显示阶段、位置、历史与可继续状态。继续是读取既有会话，不写新的教学事件；新建显式引用目标修订，不使其他会话失效。
2. 归属来自服务端目标修订，旧会话按迁移时已有归属解释，不重写历史事件。读、写、proposal、离线签发核验来源；显式范围错误不回退 current。冻结资料范围沿用目标修订，旧无范围目标保留默认资料兼容。
3. 学习、proposal、冲突恢复和侧栏刷新使用具体 session。客户端进程保存选择和按 session/activity 隔离的草稿，重启只恢复服务端已确认事实，不保存明文答案。
4. 页面切换与暂停目标、结束会话分开。请求使用发出时的不可变客户端与会话上下文；结果只返回来源会话。取消不视为事务回滚，恢复查询原会话，不自动再次提交答案。
5. paused/completed/archived 目标和归档区禁止新教学任务；已接收答案、评估和离线事实保留可查询性。在途结果与新任务的事务门分别处理，离线仍不推进在线会话。签名包字节不变，归属以原签发记录校验。
6. HTTP 与既有 MCP 复用应用服务，同步 OpenAPI、CLI 帮助与使用说明。

## 验收标准

- 先用最小回归证明 proposal 刷新会跳到 B，再验证成功、过期和失败路径都只读取 A。
- 跨区 Go/英语及同区双目标可独立新建、选择、续学；AwaitingResponse、Feedback、自由问答的焦点和事实保持。
- 草稿按题隔离；进程重启不保存未提交内容。另一设备修改 B 不影响 A 的刷新。
- 服务端拒绝跨区 goal/session/activity/node；同会话 CAS 冲突可恢复，不同会话互不改写。
- 生命周期与迟到结果不丢已接收答案；离线在 A 签发、从 B 同步仍属 A；重试不重复 Evidence。
- 真实 PostgreSQL 验证迁移、重放、归属与并发；模型 fixture、原生终端分别记录，不把普通测试 skip 当通过。

实施采用一个教学续学垂直交付，复用已有 owner 与协议，不建设新的离线存储或目标管理表单。验证从定向回归开始，生产链路完整后再运行受影响包、数据库及项目构建检查。

## 已实现合同与使用

- `learn browse [--space UUID]` 先按名称选目标，再按阶段/路线位置选会话；编号选择、`s` 新建、`n` 翻页、`q` 返回。`learn start --goal UUID`、`learn --session UUID`、`learn show --session UUID`、`learn list --goal UUID` 是脚本入口；已有目标可直接进入，空资料草稿仍需先选择资料才能生成教学内容。
- F2 在网络等待中也能离开原页面，原请求使用独立取消上下文与输出接收器；迟到结果不合并到新页面。输入等待中先让输入桥收尾，合并本进程草稿；单行编辑和多行答案都按 session/activity 隔离。Esc / `:quit` 只退出页面，`:switch` 切会话，`:space` 切区，`:pause` 暂停目标，`:complete` 结束本次教学。选择和草稿不跨进程，不新增明文落盘。
- HTTP 列表支持 `goal_id`、`status=resumable|completed`、`cursor`、`limit=1..100`；游标绑定区、目标和筛选条件。读取、写入及幂等重放先核验真实归属，跨区查询不退回全局 current。无范围旧 current 固定默认区；其他区只能显式选择。进度侧栏读取所选 session 的 stats/route，不拼接其他会话的投影。
- `switch_goal` 只保留同目标修订替换这一明确操作；换目标必须新建/选择其他会话。替换仍保留历史事件，并沿原状态机处理旧 FocusFrame；普通页面切换不发该操作。
- 新建、诊断、生成路线/题目/自由问题和离线新签发在事务内检查最新目标状态及区状态。暂停、完成、目标归档及区归档后不发新任务；已发行题目可接收答案，已接收答案可完成评估/人工处置，已签发离线授权继续按原期限归档，隐私清理门始终优先。归档不是删除。已完成操作可按原幂等身份查询/重放，不改写既有事实。
- 冻结范围以同 ID 的知识关系锚点接入既有教学外键；集合 head 不推进、不复制正文，读取继续校验范围成员与章节限制。一个范围引用同一文档的多个章节可用；同时引用同一文档的冲突修订不能物化为一个教学快照，会明确拒绝。
- 离线 prepare 的可选 `session_id` 不改签名协议。新 CLI 的加密 prepare journal 额外记录原区，发布/崩溃恢复保留它，切区后重试仍发送原 header；旧 journal 映射默认区。该元数据不进入签名包，旧 payload、授权字节与信任链不变。sync 从原签发记录确定归属，绝不推进在线会话。
- MCP 的创建、proposal、动作接受显式 `learning_space_id`，指定区资源读取同一应用服务；列表和 picker 在 HTTP/CLI 提供。记忆、Agent 与跨目标全局复习面板不在本次扩展范围内。

## 验收记录（2026-09-09）

真实数据库使用现有测试 PostgreSQL 的独立 `edu_agent_task15` 库，每个测试创建独立 schema；未操作生产库。串行执行（下列数据库命令均设置 `TEST_DATABASE_URL`）：

| 层次 | 检查与结果 |
| --- | --- |
| 真实 PostgreSQL | `go test -p=1 ./internal/learning/postgresstore -count=1`：通过；跨区、同区双目标、CAS、原题/自由问答/反馈恢复、冻结资料与离线签发、切区同步、幂等和重放 |
| HTTP | `go test -p=1 ./internal/transport/httpapi -count=1`：通过；真实按区 session 创建/列表/读取与 OpenAPI 响应校验 |
| 资料/隐私/迁移 | `go test -p=1 ./internal/knowledge/postgresstore ./internal/privacy/postgresstore ./migrations -count=1`：通过；旧版本比较只排除新增归属列，并要求旧 `scope_snapshot_id` 为 NULL |
| 模型 fixture | Go/英语各从真实导入与冻结范围生成路线、题目和自由答；评估采用可验证 fixture，未调用付费/生产模型 |
| 原生 TUI | 编译 command 测试二进制后在真实 PTY 运行 `EDU_AGENT_NATIVE_TEACHING_TEST=1 ... -test.run '^TestNativeTeachingDraftSwitch$'`，输入 A/B 不同草稿并 F2 切换，返回 A 显示原题草稿；通过。不是把普通 `go test` 的人工测试 skip 当作通过 |
| 竞态/迟到结果 | CLI 定向 `go test -race` 覆盖 F2 后迟到成功、失败、取消、按题草稿以及来源客户端；通过 |
| 构建检查 | server 与 clients/cli-go 各执行 `go test ./...`、`go vet ./...`、`go build ./...`；普通全量测试中的数据库 skip 由上述真实数据库检查补足 |

验收不声称测试了真实远程 LLM、两台物理设备或操作系统间的草稿同步；两客户端隔离用独立应用/会话与真实数据库来源校验覆盖。迁移未改写旧事件或重签离线包。普通在线答案无持久队列；响应无法确认时查询原会话，无法读取则退出并要求重新进入，不盲目补发答案。
