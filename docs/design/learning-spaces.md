# 学习区基础契约（Issue #2）

## 确认与交付范围

基线 `c5eb38d` 没有学习区实体、迁移、HTTP 生命周期或客户端选择入口。`server/internal/learning/domain.go:110` 的 `GoalRevision` 没有空间归属；`server/internal/learning/postgresstore/projection.go:560` 的 `CurrentSession` 直接读取全局当前会话。已有“学习区”文字仅指侧栏。

修改前运行 `go test ./internal/transport/httpapi -run '^TestExplicitLearningSpaceMustNotFallBackToGlobal$' -count=1`：携带伪造 `X-Learning-Space-ID` 的请求返回 200，并调用全局服务一次。回归测试保留，修改后必须拒绝且不调用全局服务。

拆分评估：保留一个垂直批次，因为实体、兼容范围和最小入口共同构成“可选择持久化学习区”的一个结果。资料隔离、目标管理、并行教学、规划和完整工作台均延期到各自 Issue。

结果：真实服务保存学习区，CLI/TUI 可列表、搜索、详情、创建、改名、归档、恢复、选择。
范围：新 learning-space owner 代码、既有 PostgreSQL、HTTP 范围检查、客户端入口和 learning 隐私 owner。
退出标准：生命周期与兼容回归、真实 PostgreSQL 升级/并发/隐私验证、生产 CLI/服务场景及 OpenAPI/帮助同步。
重型证据：在垂直路径完成后串行运行数据库检查；不引入部署服务或修改 Shell 权限。

## 身份与兼容性

固定默认 ID 为 `00000000-0000-4000-8000-000000000001`。迁移 12 只新增元数据和默认行；所有未显式迁移的旧资料、目标、教学会话和记忆按此 ID 解释。后续 owner 必须复用这个 ID，不得分别创建默认区。本次不修改旧行、事件、签名包、资源 ID 或引用，也不重新生成证据。

唯一 HTTP 范围参数是 `X-Learning-Space-ID`，必须是规范小写、非零 UUID。省略确定绑定默认区；空值、多值、非规范值为 400 `invalid_learning_space`；有效但不存在的 ID 为 404 `learning_space_not_found`。容易误用的 `space_id` / `learning_space_id` 查询参数明确拒绝。名称、目录和最近访问不会决定归属。

`learningspace.WithScope` / `Scope` 定义 Go 调用上下文，缺省同样是固定默认；`RequireLegacyModule` 是未接入 owner 的拒绝契约。HTTP 在进入任何旧业务 handler 前校验范围。旧业务目前仍只有默认区语义，不宣称已经隔离。

`GET /v1/learning-spaces/capabilities` 返回版本 1、默认 ID、`legacy_scope: fixed_default` 和 knowledge/learning/tutoring/memory 的 `default_only` 支持范围。非默认区对这些模块返回 501 `learning_space_module_unavailable`。新 CLI 的空间操作和显式业务范围先检查能力；旧服务器返回 404/405/501 时明确报告 `learning_spaces_unsupported`，不会发送业务请求或创建本地假空间。不使用学习区的旧 CLI 继续操作默认区。

MCP 当前无多区能力；其无范围入口固定默认，显式空间 header/query 返回 501。Agent 本地会话与离线包仍是旧格式，非默认选择下 CLI 明确禁止这些入口。完整多区接入属于后续 owner 工作。

## 生命周期与并发

`GET /v1/learning-spaces` 支持名称/说明的大小写不敏感字面子串搜索、状态过滤（省略包括两种状态），limit 1–100（默认 50），按稳定 UUID 排序的 keyset cursor。游标绑定 search/status，不能混用过滤条件；不是跨页快照。成功空结果为 `items: []`，错误不会伪装为空结果。

`GET /v1/learning-spaces/{id}` 可读取归档元数据。
`POST /v1/learning-spaces` 创建；`PUT /v1/learning-spaces/{id}` 完整替换 name/description/status。创建只接受 active 和 expected_version=0；修改必须使用当前 version。名称最长 120 字，说明最长 2000 字，名称不能仅为空白。

所有写请求包含 operation_id。相同设备与 operation_id、相同原始语义请求在服务端事务内去重，返回原始结果；不同请求为 409 `idempotency_conflict`。客户端网络重试复用同一请求。并发修改用行锁与 expected_version，过期版本为 409 `version_conflict`，必须重新读取；不自动覆盖。失败未提交时没有成功回执。脚本跨进程重试应保存完整原请求与 operation_id，直接使用 HTTP；CLI 的自动网络重试也保持请求不变。

归档不删除任何资源、不结束教学任务。归档区的历史读取与元数据编辑保持可用；业务写入返回 409 `learning_space_archived`。数据库中的既有 owner 写门同时检查默认区状态并持共享行锁，与 archive/restore 串行化，覆盖旧客户端、MCP 和后台写入。事件日志基础设施、投影维护和经过授权的隐私 scrub 不受归档阻止。恢复只改变状态，原 ID 与历史保留。

## 客户端使用

```sh
edu-agent space create --name 'Go 后端' --description '长期学习方向'
edu-agent space create --name '算法竞赛'
edu-agent space create --name '英语'
edu-agent space list --search Go --limit 10
edu-agent space show --id UUID
edu-agent space edit --id UUID --name 'Go 服务开发' --expected-version 1
edu-agent space archive --id UUID
edu-agent space restore --id UUID
edu-agent --space UUID progress
edu-agent space browse
edu-agent space help
```

TUI 主菜单 `z` 打开交互学习区入口，支持分页/搜索/详情及生命周期操作。选择保存在当前 App 进程，不写服务器、共享配置或磁盘；两个客户端可以停留在不同区。`--space UUID` 仅覆盖该次调用。`space select` 在当前交互进程生效；独立 Shell 命令结束后选择不保留，脚本每次显式传入 `--space`。当前没有“最近访问”持久化，因此没有跨设备抢占选择。

## 认证与隐私

沿用设备认证和 learning:read / learning:write 权限、速率限制、response read permits，以及 PostgreSQL learning owner 的隐私 generation gate。

空间名称、说明与幂等回执纳入 `learning_typed_payload` scrub 与残留校验。清除后名称为 `[redacted]`、说明清空；非默认区归档，默认区恢复 active 以允许现有清除后的新学习工作流。稳定 ID 保留为无正文的兼容引用。旧幂等回执变为无正文 tombstone，旧请求不能重新创建被清除的名称或说明。归档默认区不会阻止隐私 barrier、清除或重放维护。

## 验证入口

服务器：`go test ./...`、`go vet ./...`、`go build ./...`；客户端在 `clients/cli-go` 运行同样命令。数据库测试必须配置隔离的 `TEST_DATABASE_URL`，串行 `-p=1`。

关键测试：`TestExplicitLearningSpaceMustNotFallBackToGlobal`、`TestPostgreSQLLearningSpaceHTTPContract`、`TestPostgreSQLSpaceLifecycleRetriesConcurrencyAndRestart`、`TestPostgreSQLDefaultArchiveBlocksWritesAndPreservesHistory`、`TestLearningSpaceUpgradePreservesLegacyGoal`、`TestBarrierPersistsAcrossStepFailureAndLocalScrubResumes`、`TestExplicitSpaceRefusesOldServerBeforeBusinessRequest`、`TestSpaceSelectionIsPerAppAndFlagsAreTemporary`。

## 本次实施验证（2026-09-08）

环境：Linux、Go 1.26.6；隔离 PostgreSQL 17，固定镜像 `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`。Go/SQL/go.mod/go.sum/OpenAPI 文件按路径排序后的 SHA-256 清单摘要为 `fc345f5247b430291b2546a3b1979ad41c1ff5f52ed04a44d520bca4ec5e1c9f`。

实际通过：server 与 cli-go 的 `go test ./...`、`go vet ./...` 和生产命令构建；CLI API/command 的空间相关定向 `-race`；`git diff --check`。CLI 的精确 OpenAPI 错误集合测试最初指出新增范围响应未纳入预期，补充这些明确错误后通过，未放宽为任意响应。

真实 PostgreSQL 串行通过：`./migrations`、`./internal/learningspace/postgresstore`、`./internal/learning/postgresstore`、`./internal/privacy/postgresstore`、`./internal/transport/httpapi`、`./internal/knowledge/postgresstore`、`./internal/memory/postgresstore`，以及同批 tutoring store 单测；学习区 store 再以 `-race` 通过。未配置数据库时的普通全仓测试 skip 不作为此处数据库证据。

使用最终生产二进制启动独立服务，真实配对 CLI，在新数据库创建 Go 后端/算法竞赛/英语。搜索、分页、改名、归档、恢复、非默认业务拒绝均通过；停止并重启服务、重新启动 CLI 进程后，三个 ID 与改名结果仍存在。

| 验收项 | 结论与证据 |
| --- | --- |
| 三个学习区与重启持久化 | 通过：生产服务/配对 CLI 重启场景 |
| 搜索、分页、空列表与失败区别 | 通过：真实 PostgreSQL/HTTP，CLI 旧服务器错误及交互空结果测试 |
| 改名保持 ID | 通过：store 版本测试与生产 CLI |
| 归档/恢复保留历史 | 通过：真实旧 goal 行保留、写入拒绝与恢复；元数据路径无会话结束或删除调用 |
| 两个客户端不抢占选择 | 通过：独立 App 和不可变 client copy 测试；无配置保存 |
| 伪造、缺失、不存在、归档范围 | 通过：HTTP 范围矩阵；省略固定默认，不存在 404，归档写入 409 |
| 旧数据升级身份与引用 | 通过：从 migration 11 升级两次，默认 ID 一致，旧 goal 整行不变；迁移不重写事件/签名 |
| 新旧客户端/服务端兼容 | 通过：无参数 HTTP 默认兼容；旧服务器能力探测失败时零业务请求 |
| 未接入模块不可用 | 通过：非默认 HTTP 与生产 CLI progress 明确拒绝；Agent/offline 在 CLI 拒绝 |
| 重试、并发、隐私及入口一致 | 通过：事务去重、并发版本冲突、归档默认区隐私清除、旧请求 tombstone，以及 CLI/TUI/API 测试 |

未运行：全仓 race、跨平台原生 TUI、完整 Compose、真实外部模型/Nocturne 端到端矩阵。该批交付不依赖这些新功能；上述生产场景关闭可选模型与 sidecar，隐私 owner 集成测试使用项目既有夹具。
