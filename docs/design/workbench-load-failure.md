# 工作台概览加载故障（Issue #17）

## 问题确认与范围

核查基线为 `20459691370702d5f44a5c91d941ed9f355f8b8c`，工作分支 `task/23`。这不是用户故障环境的构建版本；用户环境的版本和原始响应仍未知。

架构中 PostgreSQL 与 learning/tutoring 保存权威状态，HTTP 层输出公开 DTO，CLI api 严格解码，command 将结果转换为页面，workbench/dashboard 负责持续全屏交互。资料和模型只参与构造教学数据；概览读取不需要模型调用。

确认的静态差异：服务端 `server/internal/learning/sessions.go:13–14` 输出会话摘要的 `node_revision_id`、`route_revision_id`，进度构造器 `server/internal/learning/progress.go:178–179` 从真实教学上下文赋值，客户端 `clients/cli-go/internal/api/sessions.go:9–24` 未声明它们。出现这些字段时，`api/client.go:518` 的未知字段检查会拒绝成功响应。以上行号均指核查基线。

本次等价环境的首个失败业务请求为 `GET /v1/learning/progress`，HTTP 200，`Content-Type: application/json`；此前能力声明与学习区读取成功。工作台进入顺序为能力声明 → 学习区详情 → 能力声明 → 带区业务预检的能力声明 → 进度。`TestWorkbenchReportsFirstFailingRead` 另在每个读取点注入错误，证明客户端报告首个失败请求，未将所有错误归到进度接口。

实际发现并修复的结构差异：

| 场景 | 服务端真实响应 | 客户端或输出层缺口 |
| --- | --- | --- |
| 进行中目标已有教学活动 | `recent_activity[]` 含 `source`、`actor_device_id`；离线事件另可带父会话及处置字段 | 基线 `api/dto_learning.go:502–510` 漏掉这些公开字段，严格解码拒绝未知字段 |
| 教学会话已有路线 | `sessions[]` 含 `node_revision_id`、`route_revision_id` | 基线 `api/sessions.go:9–24` 漏字段；最小回归直接失败于 `json: unknown field "node_revision_id"` |
| 路线节点没有待确认评估 | `nodes[].mastery.uncertainty_reasons` 为 `null` | `learning/reducer.go:92` 的结果允许 nil；基线 `httpapi/progress.go:47–48` 只规范化顶层 metadata，遗漏节点接口已有的空集合规范化 |

只补齐客户端 DTO 后，真实 CLI 仍以退出码 6 报 `decode=null_field field=response.items[1].nodes[0].mastery.uncertainty_reasons`。复用原节点接口的 `normalizeNodeReduction` 后，该场景通过；没有改变 reducer、存储投影或评分结果，也没有将客户端数组字段改为允许 null。

另一个已确认问题：`internal/workbench/workbench.go:238–239` 清空页面后，`:432–444` 的错误分支仅设置底栏，因而正文空白，已读到的学习区名称也未呈现。

## 开发方案与验收标准

本次作为一个故障修复批次，不改变学习事件、教学状态机、评分或数据库结构。

1. 用真实服务、隔离 PostgreSQL、正式 CLI 与既有严格模型夹具构造空区、无会话目标、已有路线会话；先记录失败，再修复字段差异及链路中实际确认的序列化问题。
2. 对齐客户端摘要字段和 OpenAPI，保留未知字段、必需字段、类型、重复字段及业务验证；合法空数据仍返回明确空状态。
3. 为协议错误补充请求方法、无查询参数的路径、HTTP 状态、Content-Type 类别、可用请求标识和解码类别；不输出凭据、学习正文、未知字段原文或完整响应。
4. 工作台正文显示加载失败及恢复方式，区分未加载与空数据；保留导航、返回、刷新、草稿隔离和隐私清除语义。
5. 以真实服务回归证明故障消失；验证旧服务、畸形响应、网络错误仍失败，刷新可恢复。运行受影响测试、必要 race、测试/vet/build，记录环境与未运行范围。

## 验证记录

环境：Linux `6.8.0-117-generic` / amd64、Go `1.26.6`、PostgreSQL `17.11`。使用本机已存在的 `pgvector/pgvector:pg17` 镜像，镜像 ID 为 `sha256:cf134a767f474095eeba57e0117be8e568e011a63f33fbf252f14c9b760f8e6f`；临时容器只绑定 loopback，每个黑盒场景使用随机隔离 schema，未访问已有部署的数据。

客户端和服务端均由当前工作树源码构建：未修复基线为上述 `2045969`，最终候选对应本记录所在提交。CLI 自报 `edu-agent dev commit=unknown go=go1.26.6 linux/amd64`（既有黑盒构建没有注入发布版本）；服务端没有构建版本查询命令，以测试 harness 的同树构建记录定位，不能将它们冒充用户部署的版本。

| 检查 | 结论 |
| --- | --- |
| `cd contracttests/cli-m1 && go test -p=1 -count=1 -v ./blackbox -run '^TestBlackBoxWorkbenchProgress$'`，显式配置独立 `TEST_DATABASE_URL` 及容器内 psql 包装器 | 通过。新初始化无目标、有目标无会话、教学无路线、已有路线与节点，均使用真实 HTTP/服务/数据库/CLI，并核对实际目标数量 |
| 同一黑盒的两个 Linux 原生 PTY 子用例 | 通过。真实 CLI 主入口按 `b` 进入概览，空区与已有数据均显示真实区名和内容，可切帮助页 |
| `TestInitialFailureKeepsNavigationAndRefreshRecovery`、`TestFailureClearsContentAndCanRetry` | 通过。正文失败态、未加载标识、保留已知区名、切页、返回、刷新恢复真实内容；失败响应的部分正文不会冒充成功 |
| `TestWorkbenchReportsFirstFailingRead`、`TestWorkbenchDistinguishesOldServerAndNetworkFailure` | 通过。能力声明、学习区及进度的失败请求分别可定位；旧服务和网络故障保持独立错误类别 |
| `TestProtocolDiagnostics*`、`TestProtocolPresenceHidesDynamicMapKeys` | 通过。未知字段、缺失/null/类型错误、重复字段、非法及多值 JSON 仍拒绝；诊断不含正文、动态 map 键、查询参数、完整端点或 token，异常响应头被丢弃或分类 |
| `TestProgressNormalizesRealReducerOutput`、`TestWorkbenchEntrySchemasMatchServerDTOs` | 通过。真实 reducer 的空节点结果满足数组合同，学习事实不变；公开学习区/能力声明/会话摘要与服务 DTO 一致 |
| 受影响的 API、command、workbench、dashboard、HTTP、OpenAPI 包测试及 vet | 通过 |
| `cd contracttests/fakellm && go test ./... && go vet ./...` | 通过。旧模型夹具补齐现有目标管理 DTO，仍采用严格结构和独立权威校验；路线确认使用正式显式确认步骤 |
| `make test && make vet && make build` | 通过。此命令没有设置数据库环境变量，数据库测试的 skip 不计为通过；真实数据库证据只来自上述指定黑盒 |
| `cd clients/cli-go && go test -race ./internal/api ./internal/workbench ./internal/command ./internal/dashboard` | 通过 |
| `git diff --check` | 通过 |

本次真实业务夹具停在已采用路线，不额外生成评估和 Evidence。未运行用户实际部署、旧数据迁移环境、macOS/Windows 原生 TUI、在线模型、完整 PostgreSQL 候选矩阵或全仓 race；不将局部证据表述为这些环境的通过。概览兼容修复涉及两端，部署时需同时更新客户端和服务端。故障环境若仍失败，应使用新增的方法、路径、状态、请求标识和解码类别继续定位。
