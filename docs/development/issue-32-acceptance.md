# Issue #32 实施与验收记录

## 当前状态

本轮已修复复习提案遗漏原焦点引用，完成真实到期复习作答、动态 context 换版、
新承载恢复、隐私清除与生产 CLI 读取的三引擎闭环。最终检查结果见文末
“端到端补验（2026-10-11）”；此前实施与 PG 验证记录按当时状态保留。

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

## 初始实施时尚待完成的验收（历史记录，最终结果见文末）

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

## 端到端补验（2026-10-11）

**本轮待补验项全部通过。** 原焦点冻结引用遗漏已修复，第 1、6、7、10、11、12 项
及动态知识 context 换版的两个真实 PG / HTTP 场景，在 Chromium、Firefox、WebKit
均完整通过，浏览器正式作答与同一服务的生产 CLI 读取形成闭环。此前真实 PG 的
事务、来源、增量与重放证据沿用，本轮不扩大检查范围。

### 场景与实际覆盖

新增 `clients/web/tests/browser/review.spec.ts`，共两个场景；以下断言均已在三引擎执行。

| 缺口 / issue 验收项 | 本轮结论与证据 |
| --- | --- |
| 第 1 项：真实到期复习与范围 | **通过。** 两区三目标各有一个已到期任务，首间隔为 24 小时；浏览器原会话/目标链接及切区结果与 CLI 全局列表一致，实际生成并提交复习答案。 |
| 第 6 项：正式结果与原出处 | **通过。** 浏览器独立作答形成一条 `review_recall/pass/none` 正式证据；CLI `learn show`、`assessment show`、`progress --json` 读回答案、处置及证据 ID，与 HTTP 结果一致。原 attempt/evidence 不变，进度同时保留两次来源，新调度沿用原 task ID 并推进到未来。 |
| 动态知识 context 原指针与原版本 | **通过。** 正式导入推进集合版本后，承载 context 与原 context 完全一致，CLI `study context` 同样读回旧指针；新反馈保留原 context ID、目标/路线/知识版本和节点，原焦点引用的范围、正文及摘要与历史活动一致，所有新引用仍属旧冻结范围。 |
| 第 7 项：原会话不能继续时显式新建承载 | **通过。** 原会话保持 `Completed`，任务显示不可开始原因与原答案链接；用户经 `alertdialog` 明确确认新建，创建请求绑定原 task/evidence/attempt，新的复习承载实际完成作答，原会话状态及原答案不变。 |
| 第 10 项：承载丢响应、切区迟到、重复进入 | **通过。** 创建请求真实提交后丢弃响应，浏览器读取原操作恢复同一承载；首次承载显式完成后仅新增一次第二承载，迟到响应不改变已选区。重载与重复进入不再提交，累计只创建两个明确授权的承载、仍只有一个任务，最终证据数恰为 2。 |
| 第 11 项：隐私清除后投影、缓存和历史不恢复正文 | **通过。** 正式清除 UI 返回学习事件正文清除成功；已加载的第二标签页失效，重新配对后全局进度/复习返回 `data_cleared` 空结果。旧答案、会话、正文及版本列表不可读取，Web/CLI 的旧 context 返回 null，旧承载请求被拒绝；旧目标入口、浏览器后退和 local/session storage 均未恢复原正文。 |
| 第 12 项：完整浏览器与 CLI 双端闭环 | **通过。** 两场景均由浏览器完成正式复习，再由同一生产服务的 CLI 读回结果、来源与调度；换版后的旧 context 和清除后的空结果也在双端一致，三引擎各 2 passed、0 skipped、0 flaky。 |

### 夹具与生产边界

`server/testutil/webreview` 是独立 `main` 包，不编入 `edu-agentd`。它只允许
`webdatabase` 创建的 `web_candidate_` 加 32 位十六进制随机 schema，并校验正式
配对凭据与设备身份。资料、目标、参考范围和动态 context 均先由正式 HTTP 服务
创建；夹具读取测试 supervisor 已生成的正文密钥文件路径，复用正式正文服务与
`WithAnswer` 版本校验，随后通过学习应用服务 `ApplyAction` 提交、评估和退出活动。
学习动作使用 `ServiceOptions.Now` 固定到当前时间前 72 小时，正式证据与原调度自然
产生当前已到期任务；开放题提案的租约仍使用真实时间，保留 PG 时效校验。

夹具仅直接执行 `SELECT current_schema()`，没有直接 INSERT/UPDATE 事实表，没有
新增 HTTP 端点或生产时间控制。初始化之后的浏览器与 CLI 调用仍走原生产二进制、
生产服务、真实 PG 和当前时间。模型脚本只增加本地参考资料研究/开学的确定性回应，
出处、引文、正式接纳和事务仍由应用服务核验，不证明外部模型质量。

### 缺陷与最小修复

根因是 `clients/web/src/api/teaching.ts` 的 `propose()` 在没有活动引用时，只采用按
目标正文检索的节点；检索不含原复习焦点时，正式服务正确返回 `proposal_rejected`。
修复位于该函数的检索映射之后：仅对活动提案，当引用缺少原焦点时，调用已有
`GET /v1/learning/goals/{goalID}/change-context?session_id=...`，从正式 `sources`
中按 `focus.knowledge_revision_id` 与 `focus.focus_node_revision_id` 同时匹配冻结引用，
补入模型输入及允许节点；找不到原版本引用则停止提案。没有让模型编造节点，也
没有改变服务端引用校验、公共 API、OpenAPI、migration 或用户动作协议。
`teaching.test.ts` 新增一个回归单测，先观察允许节点缺少原焦点而失败，修复后通过，
同时验证不会选取同节点的其他版本引用。

早期准备问题已压缩处理：导航/确认组件定位、主题与正文协议、租约时钟、身份审阅
边界和缺省 false；后续修正了新建的 201、旧 evidence 端点的默认区边界，以及清除后
null context 与旧目标告警的既有契约断言，均未扩展生产修复范围。

### 命令与结果

```bash
cd clients/web
npm run check && npm test
./node_modules/.bin/vite build
cd ../../server
go build -tags web_release -o edu-agentd ./cmd/edu-agentd
go vet ./testutil/webreview && go test ./testutil/webreview
go build -o /tmp/issue32-webreview ./testutil/webreview
```

前端类型检查通过，Vitest 21 个文件、53 个测试通过；夹具 vet、局部测试与构建
通过，最终 Web 资产与生产服务构建通过。CLI 沿用上一轮以下命令已产出的二进制：

```bash
make cli-build
```

浏览器使用上一轮的 `webdatabase` 包装器，每次调用创建并清理独立随机 schema。
复习三引擎串行运行，首次完整通过后即停止；工作区只跑 Chromium：

```bash
cd clients/web
export TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable'
export WEB_RELEASE_MATRIX=1 WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1
export WEB_REVIEW_FIXTURE=/tmp/issue32-webreview
export EDU_AGENT_INTEGRATION_CLI=/home/yunyue/Projects/myworks/edu-agent/clients/cli-go/bin/edu-agent
export WEB_CHROMIUM_PATH=/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome
browser_database=/tmp/edu-web-baseline-6fe9a7a.5ZH8lk/webdatabase
acceptance_logs=/tmp/edu-issue32-review-e2e

for run in chromium-final-4 firefox-final-1 webkit-final-1; do
  engine=${run%%-final-*}
  PLAYWRIGHT_JSON_OUTPUT_FILE="$acceptance_logs/$run.json" \
    "$browser_database" npm run test:browser -- tests/browser/review.spec.ts \
    --project="$engine" --reporter=line,json --output="$acceptance_logs/artifacts/$run" \
    > "$acceptance_logs/$run.log" 2>&1 || exit 1
done

PLAYWRIGHT_JSON_OUTPUT_FILE="$acceptance_logs/workspace-chromium-final-1.json" \
  "$browser_database" npm run test:browser -- tests/browser/workspace.spec.ts \
  --project=chromium --reporter=line,json \
  --output="$acceptance_logs/artifacts/workspace-chromium-final-1" \
  > "$acceptance_logs/workspace-chromium-final-1.log" 2>&1
```

若上一轮临时包装器已不存在，可在 `server` 中运行
`go build -o /tmp/issue32-webdatabase ./testutil/webdatabase` 后替换 `browser_database`。

| 检查 | 最终结果 |
| --- | --- |
| `review.spec.ts` · Chromium | 2 passed，0 failed/skipped/flaky；48.5 秒，退出码 0 |
| `review.spec.ts` · Firefox | 2 passed，0 failed/skipped/flaky；51.0 秒，退出码 0 |
| `review.spec.ts` · WebKit | 2 passed，0 failed/skipped/flaky；48.7 秒，退出码 0 |
| `workspace.spec.ts` · Chromium | 13 passed，0 failed/skipped/flaky；2.0 分钟，退出码 0 |

脚本 `node --check` 和本轮 `git diff --check` 通过。日志、JSON 与浏览器错误快照
保留在 `/tmp/edu-issue32-review-e2e/`，不纳入提交；早期失败日志保留为定位记录，
不计为最终通过。没有操作 `forjx-*` 容器，也没有运行全量浏览器矩阵、Go 全量、
race 或 Compose。构建仅出现既有依赖注释和 bundle 大小提示，不影响退出码。
本轮没有推送、创建 PR 或修改 GitHub issue；验收不证明外部模型质量或实际部署环境。
