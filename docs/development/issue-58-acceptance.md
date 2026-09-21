# Issue #58：导师停止测试的瞬时状态竞态

## 问题确认与架构

基线 `dc03439`，初始工作树干净；issue 所指 `364fbe1` 的相关测试和取消逻辑仍在。
已阅读开发工作流、分层测试策略、导师运行设计和应用组合入口。
项目由模块化 Go 服务、PostgreSQL 权威状态、共享 Agent 核心、React Web 和独立 CLI
组成。`mentorrun` 保存运行、加密正文和操作回执，后台 worker 在事务外调用模型，
HTTP 快照和 SSE 观察已提交状态。此次范围仅为导师运行测试，不改变公共协议。

修复前的代码证据：

- `server/internal/mentorrun/runtime_test.go:467–470`：提交 `stop` 后另起事务读取快照，
  严格要求状态仍为 `cancelling`，否则报“未显示取消中”。
- `server/internal/mentorrun/command.go:84–93、145–157`：运行中的停止命令将
  `cancelling/user_stopped` 提交到数据库后返回；未承诺保持该状态到下一次读取。
- `server/internal/mentorrun/worker.go:126–127、247–258`：后台续租发现状态不再是
  `running`，返回 `ErrLease` 并取消模型请求。测试设置的续租间隔为 200 毫秒。
- `server/internal/mentorrun/worker.go:216–229、284–287`：取消后结算为
  `cancelled/user_stopped`。它可以在测试的下一次快照读取前提交。
- `server/internal/mentorrun/query.go:107–138`：快照读取当前已提交行，不是命令提交
  时的历史快照。回执和事件只记录身份、版本等元数据，也不保留中间状态正文。

因此存在合法的“停止提交 → worker 完成取消 → 测试读取”顺序，原断言会误报。
这是测试同步问题，现有证据不能据此判定用户取消功能失效。

## 开发方案与验收标准（实施前）

保持一个局部交付批次：在途测试接受 `cancelling` 或 `cancelled`，核对停止原因，
并保留最终取消、调用终止及模型只调用一次的检查。另用同一个真实 PostgreSQL fixture
按顺序调用领取、停止和结算，严格验证 `running → cancelling → cancelled`，防止
放宽并发快照断言后掩盖中间状态缺失。

验收要求：

- 在原断言前等待 worker 完成，强制合法的竞态顺序，确认原断言失败且实际已取消。
- 修复后同一顺序通过；受控状态转换仍严格验证中间态和终态。
- 原 `stop/pause/archive/revoke/scope` 场景及 issue 所列三个数据库包串行通过。
- 受影响测试的定向 race、包级 vet、服务端构建和差异格式检查通过。
- 使用本机已有的固定 PostgreSQL 17 镜像、独立临时数据库和主机候选锁；不安装依赖，
  不将数据库缺失导致的 skip 算作通过。不扩大到无关前端或 CLI 验收。

## 验收结果

### 复现与精确回归

2026-09-21，Linux amd64、Go 1.26.6、PostgreSQL 17.11；使用仓库固定镜像
`postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`。
数据库位于本任务独立临时容器，模型使用原本地 HTTP fixture，不需要真实模型 Key。
执行期间持有 `/tmp/edu-agent-operations-candidate.lock`，数据库检查串行。

没有把随机重跑当作复现证据。临时在原 `stop` 断言前通过 `done` 通道等待 worker
完成（有五秒超时），强制上述合法顺序；生产代码不变。以下命令在 `server/` 执行，
设置独立 `TEST_DATABASE_URL` 和 `GOPROXY=off`：

```bash
go test -p=1 -count=1 -timeout=2m -v ./internal/mentorrun \
  -run '^TestPostgreSQLMentorInFlightCancellationAndLifecycleFences/stop$'
```

- 原断言失败，1.802 秒；日志先报告 `worker 已完成，快照状态：cancelled`，随后报
  `未显示取消中`。
- 修正断言后保留完全相同的同步实验，通过，1.910 秒。
- 移除临时同步代码后，运行下列最终精确回归，通过，11.306 秒，零 skip：

```bash
go test -p=1 -count=1 -timeout=2m -v ./internal/mentorrun \
  -run '^TestPostgreSQLMentor(StopStateTransitions|InFlightCancellationAndLifecycleFences)$'
```

新增用例显式控制领取、停止和结算的顺序，严格检查中间态及终态的 `user_stopped`
原因。原在途用例继续验证最终 `cancelled`、worker 退出和模型调用次数恰为一次。
没有延长租约或增加 sleep 来掩盖竞态，没有修改生产取消语义。

### 包级验收

以下命令均在 `server/` 下执行，数据库命令沿用上述隔离环境。

| 检查 | 结果 |
| --- | --- |
| `go test -json -p=1 -count=1 -timeout=20m ./internal/learningspace/postgresstore ./internal/mentorrun ./internal/transport/httpapi` | 三包通过，分别为 3.251、383.559、38.113 秒；410 个测试及子例通过，零失败，两个 CLI 场景因缺少二进制环境变量而跳过，见下方补测 |
| `go test -race -p=1 -count=1 -timeout=2m ./internal/mentorrun -run '^TestPostgreSQLMentor(StopStateTransitions\|InFlightCancellationAndLifecycleFences)$'` | 通过，12.321 秒，零失败、零 skip，无数据竞争报告 |
| `go vet ./internal/mentorrun` | 通过 |
| `go build ./...` | 通过，服务端普通构建 |
| `gofmt` 与 `git diff --check` | 通过 |

首次集合运行未设置 `EDU_AGENT_INTEGRATION_CLI`，以下两例显式 skip：
`TestPostgreSQLStudyCLIResearchAndWebContent`、`TestPostgreSQLStudyCLIAndWebShareChanges`。
随后在 `clients/cli-go/` 使用 `GOPROXY=off go build -o <临时目录>/edu-agent ./cmd/edu-agent`
成功构建当前源码的生产 CLI，并设置 `EDU_AGENT_INTEGRATION_CLI`，只补跑这两个场景：

```bash
go test -json -p=1 -count=1 -timeout=2m ./internal/mentorrun ./internal/transport/httpapi \
  -run '^TestPostgreSQLStudyCLI(ResearchAndWebContent|AndWebShareChanges)$'
```

两例补测均通过，分别为 11.798、3.431 秒，零 skip。合并集合与补测的唯一测试身份，
共 412 个测试及子例通过，零失败，无未补测的 skip；没有重跑已通过的完整集合。

本次不涉及 Web 或 CLI 实现，未运行其完整测试、Web 发布构建或真实模型验收。
