# Issue #56：CLI 教学黑盒显式选择会话

## 问题确认与架构依据

基线为 `dc03439`（其父提交为 issue 报告的 `364fbe1`）。工作树起始无修改。
Go CLI 通过公开 HTTP 协议访问 Go 服务；服务端 learning/tutoring 与 PostgreSQL
保存教学状态和证据。黑盒使用真实 CLI、服务端、strict fake LLM 和响应丢失代理，
每个场景创建独立数据库 schema 和客户端配置目录，不使用外部模型 Key。
Web、共享 agentcore 和 Nocturne/NoteSync 不参与本次故障路径。

已对照 `docs/comet/specs/go-cli-m1/spec.md` 的 Issue #5 增量契约及
`docs/design/tutoring-sessions.md`：无参数 `learn` 进入目标/会话选择，脚本应使用
`learn --session UUID` 续学；旧的全局 current 假设已被替代。

基线代码的具体因果链：

- `contracttests/cli-m1/blackbox/harness_test.go:584` 的 `setGoal` 已通过 HTTP 显式
  创建会话并返回 ID，但 `:415` 的 `runCLI` 每次启动新进程，不保留进程内选择。
- `contracttests/cli-m1/blackbox/scenarios_test.go:51,98` 仍调用无参数 `learn`，
  `input_test.go:24` 的教学输入直接从路线检索确认 `y` 开始。
- `clients/cli-go/internal/command/learn.go:60-74` 找不到指定会话及进程内选择，
  因而调用目标选择器；`tutoring_sessions.go:92-107` 将这些输入作为编号重试。
- 输入耗尽后 `terminal/terminal.go:143-148` 返回 `io.EOF`，经过
  `command/error.go:217` 映射为 `internal_error`，教学循环和模型请求尚未发生。

在 Go 1.26.6、独立 PostgreSQL 环境执行 issue 指定的两项测试，得到原始失败：
教学流程 `exit=6 error_code=internal_error`，反复输出目标编号提示；离线流程
`materialized objective activity missing expected stable marker`。
首次本机复现使用 PostgreSQL 15.19，随后切换到 PostgreSQL 17.11 核对报告环境。
17.11 下两项仍按上述方式失败；另加响应丢失场景，得到
`state=GoalReady want=Evaluating error_code=internal_error fake_calls=map[]`。

原始复现命令（在 `contracttests/cli-m1`，已设置独立 `TEST_DATABASE_URL`）：

```sh
go test -p=1 -count=1 -v \
  -run '^TestBlackBox(AcceptedTeachingFlow|OfflineObjectivePrepareLearnSyncStatus|ResponseLossReplaysSameBodyWithoutDuplicateAuthority)$' ./blackbox
```

## 开发方案与验收标准

本次采用一个测试契约修复批次，不改变正式教学、选择器或错误处理语义。

1. 所有受影响的教学黑盒调用显式传入场景创建的会话 ID；第二客户端续学、评估
   处置及离线准备绑定原会话，复习场景分别使用各自会话。
2. 离线准备前显式退出在线课堂，断言题目已展示且在线会话为 `AwaitingResponse`，
   不再把任意非零退出当作准备成功。保留离线归档及在线状态不推进的断言。
3. 保留模型调用次数、各教学状态、Evidence 唯一性和响应丢失重放断言；补充
   原会话之外另有会话时仍能正确续学的回归覆盖，避免退回全局 current。
4. 先运行 issue 的两项复现及响应丢失定向回归，再运行受影响教学黑盒集合、fixture
   契约、模块 vet/构建和补丁格式检查；各检查按实际结果记录。

进入正式教学后确认了以下测试契约漂移，纳入同一批次：

- 服务端 `settings/service.go:328-334` 和 `integrations/llm/client.go:175-176`
  已发送输出预算 `max_tokens`，fake LLM 的严格请求结构未声明该字段，直接返回 400。
  将适配器合同测试配置为真实教学所用的 2048 预算后，五种 proposal 均稳定失败
  `tutor model failed: incompatible`。修复只声明可选正整数预算，继续拒绝未知字段、
  非整数和非正数；保留不带预算的兼容检查。
- 当前 `command/learn.go:257-259` 在应用路线前要求明确确认；旧输入把下一步空行当作拒绝，
  得到 `planning_confirmation_required`。按 `docs/design/learning-planning.md`
  的现有合同补充独立的路线采用确认，不绕过生产确认步骤。
- 复习场景仍为每次复习创建新目标，但
  `server/internal/learning/postgresstore/session_view.go:414-418` 已按目标查到期复习。
  完整门禁实际得到 `review presented event count=0 want=1`。改为在原目标版本下
  显式新建复习会话，保留临时评估、作废与已接纳证据的所有推进断言。

验收要求：教学到达 Completed 且模型各调用一次；离线 prepare/learn/sync/status
完成并仅生成一份 Evidence；多行答案、临时评估确认/覆盖、自由问答/附加测验、复习、
模型失败重试、响应丢失和知识维护均进入并通过各自阶段，原有数据库断言不降级。

## 验证结果

环境：Linux amd64、Go 1.26.6、独立 PostgreSQL 17.11；复用本机已有镜像，
数据库仅绑定 loopback，`psql` 使用同一容器内的客户端。所有数据库场景串行运行。

已完成的定向检查：

- `contracttests/fakellm`：先将真实适配器测试带上预算，五种 proposal 失败；
  补齐协议后，`TestRealTutorModelAdapterDecodesEveryProposalSchema` 和
  `TestFixtureOutputBudgetContract` 通过。随后 `go test ./... && go vet ./... && go build ./...` 通过。
- `contracttests/cli-m1`：原始教学、离线同步、响应丢失场景通过；教学增加另一个较新会话，
  原会话完成且另一会话状态及版本不变。输入顺序测试通过。
- `contracttests/cli-m1`：`go test ./responseproxy ./cmd/response-loss-proxy && go vet ./... && go build ./...` 通过。
- `clients/cli-go`：显式路线确认、目标保存不改会话、proposal 来源会话刷新及答案响应丢失
  四项命令层回归通过。没有改动客户端/服务端生产代码，不宣称运行全仓测试。

完整门禁命令为设置 `TEST_DATABASE_URL` 后执行 `make cli-m1-blackbox`。
生产模型配置对照场景的 baseline/candidate 均复现 `process readiness failed: status endpoint`，
与已有 #55 一致；本次只同步其中同类会话调用，不修改其启动配置或重试预算。
该场景的实际教学/模型对照仍未得到端到端通过证据，不能将完整门禁记为通过。

完整门禁中的目标管理、可恢复导入、导入预览、输入顺序、工作台及其两个原生 PTY
子场景均通过；复习初次因上述目标归属失败，修复后定向回归通过。
最终定向集合使用以下根目录命令覆盖本 issue 的全部十项教学黑盒，保留 #55 的独立失败记录：

```sh
CLI_BLACKBOX_RUN='^TestBlackBox(AcceptedTeachingFlow|OfflineObjectivePrepareLearnSyncStatus|MultilineAnswerAndNonDefaultHelp|ProvisionalSurvivesExitAndSecondCLI(Confirms|Overrides)|FreeAnswerAttachedQuizFeedbackAndExplicitResume|DueReviewOnlyAcceptedEvidenceAdvances|ModelFailurePreservesAuthoritativeStateAndCanRetry|ResponseLossReplaysSameBodyWithoutDuplicateAuthority|KnowledgeMaintenanceSharesProposalAcrossHTTPMCPAndCLI)$' \
  make cli-m1-blackbox-run
```

最终集合结果：10 项全部通过，0 失败、0 跳过，黑盒耗时 92.886 秒。每个具名场景都
保留原有教学状态及数据库验收断言；fixture/proxy 契约也通过。会话创建 helper 提取后
再次执行模块 vet/构建通过，`git diff --check` 通过。

本次创建的临时 PostgreSQL 容器及其 tmpfs 测试数据已清理；测试数据不可恢复，
未改动其他运行中的数据库。验收未使用真实外部模型，不代表 #55 或全端发布验收通过。

## 合入主线后的补验

用户验收后，将 `main@0295613` 合入任务分支，产生合并提交 `9979490`。
合并自动完成，没有手动冲突处理；保留主线 #55 的 5 秒超时及 6 秒故障延迟，
同时保留本任务的显式会话、路线确认和 fake LLM 输出预算协议修复。

使用新的独立 PostgreSQL 17.11 测试容器，在 `contracttests/cli-m1` 执行：

```sh
go test -p=1 -count=1 -v -run '^TestBlackBoxProductionFakeModelVerticalPostgreSQL$' ./blackbox
```

baseline 与 candidate 两组全部通过，完整覆盖原先被启动与目标选择阻断的评估语料、
真实超时重试及持久化断言；测试包耗时 64.529 秒。fake LLM 的测试和 vet 也通过。
此前失败记录保留为历史证据；本次补验解除了该模型对照场景的未验证限制，
未重新运行全端发布矩阵。临时容器及其不可恢复的 tmpfs 测试数据已清理。
