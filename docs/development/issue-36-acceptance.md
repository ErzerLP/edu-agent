# Issue #36 验收记录

## 确认与修改范围

基线 `acc4d05`，先完成[缺失确认、开发方案和验收标准](../design/cli-learning-services.md)，
再实施代码。不是重建已有目标/进度能力：原 CLI 确实没有研究、开学、正文作答和具体变更
决定入口；`go run ./cmd/edu-agent goal research --help` 原先报告 internal_error、退出 6。
新增入口后，生产二进制同命令输出学习服务帮助、退出 0。

另以真实 PostgreSQL 复现旧作答入口绕过未知交互限制：新增测试在修复前失败，
错误为“旧入口必须拒绝未知作答规则，实际：<nil>”。修复点在原 learning 作答事务的
`learningcontent.ValidateAttemptTx`，不是前端隐藏按钮；所有在线入口共用交互校验，
无正文的旧活动仍沿用原合同，未知交互返回明确升级错误且不写答案。

新增 `study` 脚本入口与目标/课堂工作台页面，复用正式 HTTP/PG 服务和共享 Go 核心。
运行恢复须明确目标、类型及原运行会话，所有写入保留具体操作和版本，无自动重放。
不改本地 Agent 循环、钥匙串、加密聊天、Shell/PTY、离线包字节与签名；无数据库迁移。
帮助、OpenAPI 原 DTO 的 CLI 用法和能力矩阵同批更新。

## 已完成的验证

以下命令在对应 Go 模块执行；真实 PG 用例使用本机测试 PostgreSQL 17 的独立随机 schema，
用例结束由已有测试清理机制删除，不使用生产数据。运行时通过环境注入
`TEST_DATABASE_URL`，生产二进制路径通过 `EDU_AGENT_INTEGRATION_CLI` 注入。

| 检查 | 结果与边界 |
| --- | --- |
| CLI `go test ./...`、`go vet ./...` | 通过；包含 endpoint/credentials、加密历史/no-save、工作区、Shell/PTY、离线等既有回归 |
| 最终 CLI 受影响包 `go test ./internal/api ./internal/command ./internal/workbench`、对应 vet 和 `-race` | 通过；含旧服务/未来 schema、未知交互、跨区响应、写入只发送一次、关闭后读/清除、具体审批版本、运行恢复与重试身份 |
| `make cli-build` | 生产 Linux CLI 构建通过；研究别名帮助已通过 |
| CLI `CGO_ENABLED=0 GOOS=darwin GOARCH=amd64/arm64 go build ./...` | 两种架构分别编译通过，不作为 macOS 原生验收 |
| 服务端 `go test ./internal/learningcontent ./internal/learning/postgresstore ./internal/transport/httpapi ./internal/mentorrun ./api` | 通过；无 PG 环境的普通调用跳过 PG 测试，真实 PG 证据另列，不混算 |
| 服务端受影响包 `go vet`、`go build ./cmd/edu-agentd` | 通过；这是 Go 开发构建，不是包含 Web 资源的 web_release 构建 |
| OpenAPI 合同检查、`git diff --check` | 通过 |

### 真实生产 CLI、Go HTTP 与 PG

`TestPostgreSQLStudyCLIResearchAndWebContent` 使用生产 CLI 子进程和真实 Cookie/CSRF，
最后以真实 PG、`go test -race` 再次通过：

1. Web 身份创建空资料目标并研究开学，搜索、正文、模型调用使用既有 HTTP fixture；
   服务端运行产生正式课堂、知识上下文与内容，未直接伪造成功回执。
2. 独立配对的 CLI 读取原课堂与原正文，提交绑定正文/会话版本的答案；Web 读取同一答案。
3. CLI 自己再发起真实开学并查询完成结果，Web 读取其正式课堂；原课堂和答案不被替换。
4. CLI 请求服务端导师修改原课堂路线；fixture 模型通过真实
   `read_learning_context` / `propose_learning_change` 工具提出候选，当前题未处理时排队。
5. Web 读取相同 hash；CLI 明确对具体版本 `apply_now` 并重复同操作，返回原结果。
   Web 读取同一新活动；两端目标进度的路线、会话、答案与证据条目相等，原题答案只有一份。

`TestPostgreSQLStudyCLIAndWebShareChanges` 另验证：CLI 提出目标范围变更，Web 审阅
同一事实，CLI 明确批准和幂等重试；另一目标不变，没有复制课堂、答案或 Evidence。

### 真实 PG 领域与兼容回归

- `TestPostgreSQLLearningContentVersionsAnswersAndErasure`：旧入口的未知交互失败回归、
  正文版本、正常答案与清除通过。
- `TestPostgreSQLAdaptiveQueueImmediateAndCompensation`、
  `TestPostgreSQLAdaptiveApprovalVersionsAndExplanation`、
  `TestPostgreSQLAdaptiveRealMentorToolAndSubmittedAnswer`：安全边界、具体版本审批、
  原答案恢复与补偿通过。
- `TestPostgreSQLResearchStopDuringFetch`、`TestPostgreSQLStartLearningConcurrentGoals`、
  `TestPostgreSQLStartLearningEmptyLibrary`：取消/迟到结果、多目标隔离和三个学科的
  空资料开学通过；仅保存目标仍不产生模型/搜索调用。
- `TestPostgreSQLResearchSameNameAcrossSpaces`：不同学习区同名目标隔离通过。

### 平台与模型证据

Linux 原生 `TestNativePlatformSecretRoundTripCleanup` 在隔离 D-Bus / GNOME Keyring
环境、`EDU_AGENT_NATIVE_KEYBACKEND_TEST=1` 下通过，覆盖真实密钥存取、替换、删除与
删除后不可读；不使用用户原钥匙串。模型证据是本机 HTTP fixture，不等于外部付费模型实测。

## 尚未验收的发布边界

- 真正渲染浏览器的跨端闭环：当前工作树没有 Web 测试依赖，已请求按锁文件 `npm ci`
  的独立安装授权，尚未取得；没有执行安装。上述 Cookie/CSRF HTTP 用例不冒充浏览器测试。
- Web TypeScript/前端测试和包含前端资源的 `web_release` 构建未执行。
- macOS 钥匙串/原生交互未在 macOS 真机执行；Linux 下的测试或交叉编译均不能替代。

代码和已通过证据交任务分支审查，结论为需要复核上述具体环境项，不将 #36 全部发布验收
勾选为通过；未创建 PR、未推送、未合并基础分支。
