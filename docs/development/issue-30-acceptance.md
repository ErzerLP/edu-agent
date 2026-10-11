# Issue #30 验收记录

## 先确认，再实施

基线 `9a80ee4`，初始工作树干净。先读模块边界和现有合同，再记录
[开发方案与验收标准](../design/dynamic-knowledge.md)，之后才修改生产实现。

既有 ConceptRevision 仅含身份、名称、语义键和研究引用（基线
`server/internal/knowledge/context.go:14`）；既有 Web 知识页只有集合、导入和参考
（基线 `clients/web/src/knowledge-page.tsx:26`）。文档 maintenance 服务已经存在，
但不能提供概念关系、冲突主张、概念合并/拆分及其审阅。

先加入 HTTP 回归用例，再运行：

```bash
cd server
go test ./internal/transport/httpapi -run '^TestIssue30KnowledgeStructureCapabilities$' -count=1
```

修改前：能力入口返回 **404**，用例失败。修改后：同一用例通过。
原因是结构领域数据和正式服务/页面入口均缺失，不是已有设置未开启。

## 已实现范围

- 在 knowledge owner 内追加概念结构、独立身份、具体章节出处与主张/条件/缺口。
- 先授权过滤再分页，图和列表使用同一页；一跳关系及有限结果，不递归遍历图。
- 正式提案、审阅、合并/拆分映射、拒绝和补偿；幂等回放、基础版本与依赖核验。
- 学习事实由 learning owner 的只读端口提供；不复制 Evidence、不按来源采纳判定掌握。
- 后续上下文通过 learningchange 的正式路线候选、安全边界和版本检查接入。
- 新增数据纳入既有隐私屏障、清除与残留核验；旧版本不能恢复已清除正文。
- Web 节点/局部图/详情/维护记录与教学侧栏；旧文档维护复用原服务及默认映射。
- OpenAPI、能力入口、增量迁移、部署白名单和中文使用说明。

## 已通过检查

Go 检查在 `server` 运行。真实 PG 使用独立本机 PostgreSQL 17/pgvector 容器，
各集成测试创建独立 schema；没有连接生产库。设置独立 `TEST_DATABASE_URL`，
数据库测试按 `-p=1` 串行执行。

1. `go test ./internal/knowledge/... ./internal/transport/httpapi ./api ./migrations ./internal/learningchange`
   的非数据库检查。OpenAPI 包最初发现部署白名单仍将旧维护入口列为禁止；本次正式开放
   这些接口后，更新对应正向断言并补内部/强制写入路径的负向断言，`go test ./api` 通过。
   **未设置数据库的用例跳过，不计为真实 PG 验收。**
2. `TEST_DATABASE_URL=… go test -p=1 ./internal/knowledge/postgresstore -count=1` 全包通过。
   包括多来源、同名独立身份、关系环与分页、跨区隔离、原文核验、解除引用、并发审批、
   过期游标、旧引用、合并/拆分、拒绝、补偿和未知结果幂等回放。
3. `TEST_DATABASE_URL=… go test -p=1 ./internal/knowledge/postgresstore ./internal/mentorrun ./internal/privacy/postgresstore -run 'TestPostgreSQLIssue30|TestPostgreSQLAdaptiveApprovalVersionsAndExplanation|TestPostgreSQLAdaptiveGoalRevisionAndQueueFences|TestKnowledgeRedactedRevisionTombstoneAllowsFreshImport' -count=1` 通过。
   覆盖真实课堂安全边界、当前题不变、旧 context 不变、新内容依赖使提案过期和隐私清除。
4. `TEST_DATABASE_URL=… go test -p=1 ./internal/transport/httpapi ./migrations -run 'TestIssue30|TestKnowledgeMaintenanceHTTP' -count=1` 通过。
   包括实际从 28 升级到 29、旧版本头正确回填、追加修订推进版本、身份注入和独立审批权限。
5. `go vet ./internal/knowledge/... ./internal/learningchange ./internal/learning/postgresstore ./internal/transport/httpapi ./internal/app ./internal/mentorrun ./internal/privacy/postgresstore` 与 `go build ./...` 通过。
6. 在 `clients/cli-go` 运行 `go test ./internal/api ./internal/command -run 'Knowledge|Maintenance|OpenAPI' -count=1` 通过，未改旧 CLI DTO。
7. `git diff --check` 通过。

完整 mentorrun 持久化包曾发现三处本次引入的兼容性回归：无结构变化时错误填入旧 context，
导致已修订目标或暂停路线被拒绝。已限定为确有结构变化时才创建候选 context，并重跑第 3 项
中的全部失败场景通过；**修复后未重新运行完整 mentorrun 包**，不将局部结果写成全包通过。

## 尚未完成的验收

当前工作树没有 `clients/web/node_modules`。用户工作规范要求安装须单独授权，已请求允许
`npm ci` 按现有锁文件安装，尚未获得确认，因此没有安装或升级依赖。
实际运行 `npm run check` 返回退出码 127：`tsc: not found`。

下列项目仍待完成，不能据现有后端结果认定 Issue #30 全部验收通过：

- Web OpenAPI 类型生成、TypeScript 检查、格式化、单元测试及生产构建。
- 新增 `knowledge-structure.spec.ts` 的真实浏览器列表/图、键盘、审阅、响应丢失恢复、
  移动视口及无障碍检查，以及受影响的既有浏览器用例。
- 根据实际检查结果修复前端问题并进行最终差异审查。

以上是开发阶段的验收结果。用户随后明确接受现有结果，授权提交任务分支并以 squash
落地到 `main`、推送远端；本次按该后续指令落地，不将 Web 的待验收项改写为通过。
专用测试容器 `edu-agent-issue30-test` 已停止，未删除；可在继续验收时重新启动。

## 浏览器补验（2026-10-11）

从干净的 `main@6fe9a7a` 新建 `issue-30-knowledge-browser`。此前“尚未完成”
小节保留开发阶段的历史状态；后续补验见
[task-91-test-report.md](task-91-test-report.md#真实模型-web-与教学补验) 和
[issue-71-acceptance.md](issue-71-acceptance.md)。本次在 #71 修复后的基线上
补齐剩余浏览器缺口，不重新执行已确认通过的 PostgreSQL 与 CLI 验收。

只修改 `clients/web/tests/browser/knowledge-structure.spec.ts` 和本记录。
复用 `offlineFixture`、`serverCommand`、本地确定性教学模型和正式 HTTP 服务，
知识夹具也经版本化提案及正式批准创建，没有 SQL 旁路或知识 API 模拟。

| Issue 验收项 | 结论与证据 |
| --- | --- |
| 第 2 项：局部关系/图与列表一致，服务端范围校验 | **通过。** 原用例只有孤立节点，未覆盖真实关系边。本次补入两个同目标节点和一条前置关系，核对列表与 SVG 的节点集合、图节点键盘入口、边数量/类型、列表中的关系方向和正式 API 的边身份；一跳范围仍显示相同两个节点与关系，当前目标范围外的历史节点不进入图/列表。服务端授权隔离沿用前述真实 PG 证据，本次没有将目标过滤当成越权验证。三引擎均通过。 |
| 第 7 项：安全接入未来教学，当前题与草稿不丢 | **通过。** 真实课堂填入未提交答案，经应用内链接修订并批准本目标的知识，再返回原课堂；点击“接入已审阅知识维护”，正式变更返回 `queued_for_boundary` / `boundary`。维护批准后与教学排队后，逐一比较题目正文、课堂基础版本和活动记录，并确认草稿保留；再次往返知识页仍恢复原草稿。浏览器场景复用旧协议课堂夹具，验证排队和草稿保护；新知识 context 在安全边界生效沿用前述 `TestPostgreSQLIssue30MaintenanceUsesAdaptiveBoundary` 证据。三引擎均通过。 |
| 第 11 项：清除覆盖节点、索引、关系与缓存，旧图不能恢复正文 | **通过。** 清除前实际打开节点列表、关系图和含私密正文的详情；另一独立浏览器身份通过一次性授权发起全局清除，核对 `knowledge_content`、`knowledge_index`、`process_cache` 步骤成功。旧页在焦点触发身份重验后进入配对页，旧图/列表/详情及正文消失；不刷新直接重新配对，新代次的列表、节点、边与详情均为空，旧修订请求返回 404。浏览器历史返回与刷新后仍为空，旧正文没有恢复。三引擎均通过。 |
| 第 12 项：真实 PG、浏览器图/列表与旧 CLI 合同测试 | **通过（合并已有与本次证据）。** 本次修改的单文件三引擎各 3 个场景全通过，保留审阅、响应丢失幂等核对、键盘、移动宽度与 Axe 无障碍断言。真实 PG 和旧 CLI 合同采用本记录此前已通过结果；本次未重跑，也未声称执行全量浏览器矩阵。 |

### 命令与结果

使用已准备的 `edu-agent-acc-postgres` 和既有 `webdatabase` 隔离工具；每个引擎
单独创建随机 schema，完成后只清理本组 schema。未操作其他项目容器。
在仓库根目录运行以下等价命令（原执行采用相同环境的逐组命令）：

```bash
cd clients/web
export TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable'
export WEB_RELEASE_MATRIX=1 WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0
export WEB_CHROMIUM_PATH='/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome'
issue30_database=/tmp/edu-web-baseline-6fe9a7a.5ZH8lk/webdatabase
issue30_evidence=/tmp/edu-issue30-browser.bknEgW

PLAYWRIGHT_JSON_OUTPUT_FILE="$issue30_evidence/chromium.json" "$issue30_database" npm run test:browser -- knowledge-structure.spec.ts --project=chromium --reporter=line,json --output="$issue30_evidence/artifacts/chromium-final" > "$issue30_evidence/chromium.log" 2>&1
PLAYWRIGHT_JSON_OUTPUT_FILE="$issue30_evidence/firefox.json" "$issue30_database" npm run test:browser -- knowledge-structure.spec.ts --project=firefox --reporter=line,json --output="$issue30_evidence/artifacts/firefox" > "$issue30_evidence/firefox.log" 2>&1
PLAYWRIGHT_JSON_OUTPUT_FILE="$issue30_evidence/webkit.json" "$issue30_database" npm run test:browser -- knowledge-structure.spec.ts --project=webkit --reporter=line,json --output="$issue30_evidence/artifacts/webkit" > "$issue30_evidence/webkit.log" 2>&1
```

| 引擎 | 最终结果 | 运行耗时 | 退出码 |
| --- | --- | --- | --- |
| Chromium | 3 passed，0 failed，0 skipped，0 retries | 17.2 秒 | 0 |
| Firefox | 3 passed，0 failed，0 skipped，0 retries | 20.9 秒 | 0 |
| WebKit | 3 passed，0 failed，0 skipped，0 retries | 17.9 秒 | 0 |

三份 JSON 均核对到 3 个场景单次通过，无收集错误；通过后即停止对应引擎的测试。
首次 Chromium 运行发现两处新增测试步骤错误：下拉框标签的精确匹配无法定位，
以及配对请求尚未完成就读取会话。改用 combobox 定位并等待知识面板出现后，
在新 schema 完整复测通过。初次日志/JSON 保留为 `chromium-initial.log` /
`chromium-initial.json`，不把它们记为产品缺陷或最终通过证据。

`node --check clients/web/tests/browser/knowledge-structure.spec.ts` 和
`git diff --check` 通过。没有修改前端产品源码或 Go 代码，因此没有增加前端
全量单测/类型检查或 Go 包测试；未运行全量浏览器矩阵、Go 全量、race 或 Compose。
仅有既有 `NO_COLOR` / `FORCE_COLOR` 环境提示，未为此修改配置。

本次未发现产品缺陷，未修改公共 API/OpenAPI、migration 或用户可见语义。
第 2、7、11、12 项的剩余浏览器证据已补齐，本次范围内无未解决验收项。
GitHub issue 状态未修改；临时日志、JSON 和浏览器产物留在上述 `/tmp` 目录，
不纳入提交。
