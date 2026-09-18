# Issue #32 实施与验收记录

## 当前状态

已确认功能缺失并实现主要链路，服务端和 CLI 检查通过；**前端验收尚未完成**。
工作树没有 `clients/web/node_modules`，用户全局规则要求安装依赖另行授权；已请求允许
按现有锁文件运行 `npm ci`，尚未收到答复，没有自行安装或升级。
因此不声称整项验收通过，也不创建、合并或推送 PR。

## 确认依据与范围

实施前先阅读架构、动态 context、路径变更、进度投影和 #31 反馈契约，记录
[开发方案与验收标准](../design/web-learning-progress.md)。基线 `a5bf595` 的
`clients/web/src/main.tsx:191` 路由表没有进度页；首页和目标详情未调用正式进度服务，
课堂 `teaching-page.tsx:862` 没有 `present_review` 入口。服务端已有范围分页和稳定
复习任务，但 `postgresstore/progress.go:366` 仅支持仍停在原节点的原会话，创建会话
协议没有来源绑定承载；`learning/progress.go:88` 没有相对前版的活动差异。
这是代码确认的功能缺失，不是既有设置改名。外部 issue 仅作为需求数据，没有执行
其外部操作指令。没有确认到“自述会覆盖正式 Evidence”的缺陷，未为此修改证据规则。

本次补充统一 Web 聚合入口、动态版本说明、显式复习承载与原答案链接、清除状态、
只读导师聚合工具、OpenAPI/CLI 字段及使用说明。复用原调度、正式答案/证据详情和
事务/隐私接口；不复制 Evidence，不增加日历、自动任务或持久正文缓存。

## 已执行检查

真实 PostgreSQL 17 使用现有本地实例中的本任务专用 `edu_agent_task53` 数据库，
每项测试创建并清理随机 schema；数据库测试串行执行，没有改动生产数据。

| 检查 | 结果及实际覆盖 |
| --- | --- |
| 服务端 `go test ./...` | 通过；未配置数据库的跳过不计入 PG 验收 |
| 受影响 learning/mentorrun/HTTP/MCP 包 `go vet`，`go build ./cmd/edu-agentd` | 通过；不含新的 Web 发行资产 |
| CLI `go test ./...`、受影响包 `go vet`、`go build ./cmd/edu-agent` | 通过；严格 DTO 兼容及命令读取回归 |
| `TestProgressExplainsRouteVersionsWithoutInventingMastery` | 通过；前版/新增、旧完成不继承、无证据掌握未知、旧目标默认归属 |
| `TestPostgreSQLProgressScopePaginationAndReplay` | 真实 PG 与 `-race` 通过；两区同区双目标、范围计数、游标和生命周期、增量/重放 |
| `TestPostgreSQLReviewTasksKeepIndependentGoalsAndReplay` | 真实 PG 与 `-race` 通过；共享节点的不同目标独立任务、调度和重放一致 |
| `TestPostgreSQLReviewCarrierPreservesSourceAndReplay` | 真实 PG 与 `-race` 通过；原会话不变、来源版本、新证据、幂等、重复拒绝、旧承载完成后显式新建、暂停/恢复、清除后不恢复 |
| `TestPostgreSQLMentorProgressBindsScopeAndRechecksGoal` | 真实 PG 与 `-race` 通过；绑定目标/区、拒绝模型指定其他范围、读后目标版本再次校验 |
| 9 个变更 TS/TSX 文件语法解析 | 通过；使用环境已有 TypeScript，只读解析，不等同于类型检查或构建 |
| `git diff --check` | 通过 |

首次完整 Go 检查发现新增承载查询直接访问 tutoring 表，违反既有 owner 架构检查。
已改为调用同事务 tutoring owner 读取接口及其隐私屏障；没有修改、绕过或跳过检查。
修复后全套 Go 检查和上述真实 PG 回归通过。

真实 PG 复现命令（先配置独占测试库 URL，不在日志中打印凭据）：

```bash
cd server
go test -race -p=1 ./internal/learning/postgresstore ./internal/mentorrun \
  -run 'TestPostgreSQL(ReviewCarrier|ProgressScope|ReviewTasks|MentorProgress)' -count=1
```

## 仍需完成的验收

- 授权后执行 `npm ci`、`npm run generate`、`npm run check`、`npm test`、`npm run build`，
  并格式化本次改动文件。OpenAPI 已同步，TS 类型目前为对应的精确补丁，尚未重新生成验证。
- 构建包含真实 Web 资产的 Go 发行版本，运行新增工作区浏览器场景。已编写真实两区三目标、
  原会话链接、版本比例、原答案证据链接、范围切换、移动视口和投影失败回退用例，**尚未运行**。
- 浏览器复习实际作答、新承载丢响应/切区迟到/隐私清除回归，以及同一真实 HTTP 服务的 CLI
  双端读取仍需验收；目前已有 PG 事务/来源/重放验证及 CLI 协议测试，不能替代此端到端要求。
- 动态知识 context 沿用来源指针的实现经过代码核对，但尚未新增独立的动态 context
  复习端到端场景；现有真实 PG 复习场景验证固定知识/路线版本。

浏览器准备完成后可从新增场景开始，再补上述边界：

```bash
cd clients/web
WEB_WORKSPACE_FIXTURE=1 TEST_DATABASE_URL='独立空测试库 URL' \
  WEB_CHROMIUM_PATH='已安装的 Chromium 可执行文件' \
  npm run test:browser -- tests/browser/workspace.spec.ts --grep '动态进度'
```

模型夹具只用于本地协议验证，不证明外部真实模型评分质量。离线来源仍使用原离线
API/CLI，未增建另一套离线评分页。未运行完整 PG 全项目扫描或 HTTPS/Nginx 实际部署。
工作项专门要求由用户审查落地，因此不推送、合并或 rebase 基础分支。
