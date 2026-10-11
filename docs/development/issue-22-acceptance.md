# Issue #22 验收记录

## 缺失确认与实施

起点 `549c4fe`。实施前通过设置能力、工具白名单、调用者与前端入口确认研究缺失，不是别名或已有开关。确切位置及实施前方案见[目标研究设计](../design/goal-research.md)。

新增专用公共网页抓取、来源与片段模型、研究运行类型、真实搜索/解析/综合流水线、knowledge 采纳与持久来源修订、OpenAPI 和独立 React 研究页面。复用原运行回执、预算、租约、取消、加密与隐私 owner。新 `research` 配对档案授予明确采纳权限，旧身份不升级。

## 已执行证据（2026-09-16）

| 检查 | 结果与范围 |
| --- | --- |
| `cd server && go test ./...`、`go build ./...` | 通过；普通全包测试未设置数据库地址，其中数据库 skip 不算数据库验收；不是带 Web 资产的发行构建 |
| 受影响包 `go vet`、`git diff --check` | 通过 |
| `go test ./internal/research` | 公共地址、IPv6 映射/转换、混合 DNS、固定拨号、重绑定、范围跳转、压缩炸弹、字节上限、禁止归档/索引、HTML 脚本隔离、不支持 PDF、真实/伪造引用通过 |
| `TestPostgreSQLResearchPipelineAdoptionAndIsolation` | 真实 Brave HTTP 协议夹具 → 生产抓取/解析 → 模型 HTTP 夹具 → PostgreSQL；摘要不变正文、失败可见、自动采纳与 knowledge 审计、重启读取、幂等、伪造 ID/跨区拒绝，以及运行清除后从正式知识读取历史片段 |
| `TestPostgreSQLResearchBudgetAndForgedCitation` | 正文请求前预算暂停、续行不重复搜索；模型伪造片段不能进入综合结果 |
| `TestPostgreSQLResearchStopDuringFetch` | 在途网页请求被取消，不调用综合模型，不保存迟到正文 |
| `TestPostgreSQLResearchRequiresSpecificConsentAndKnowledgeScope` | 缺外发同意或采纳权限时拒绝 |
| `TestPostgreSQLResearchSameNameAcrossSpaces` | 两个真实学习区的同名来源互不覆盖，跨区来源 ID 和查询被拒绝 |
| `TestPostgreSQLResearchGlobalErasureCannotReviveSources` | 真实隐私 grant/barrier/scrub；运行、正式正文、索引/来源关系清除，旧运行和操作不能复活正文 |
| `TestPostgreSQLWebResearchProfileAndRevocation` | 显式浏览器研究权限及撤回立即生效 |
| `TestPostgreSQLResearchCookieSourcesAndDecisions` | 真实 Cookie/API、来源分页/详情、CSRF、伪造 ID、采纳及同操作回执；此 transport 测试注入已读 checkpoint，不能冒充浏览器端到端 |
| 原导师运行、HTTP/SSE 定向 PostgreSQL/race 回归 | 通过；包括租约、停止、预算、隐私、读取屏障和恢复 |
| 新来源身份与 OpenAPI 合同 | 外部身份标记不能覆盖正式知识，响应/路径/scope 合同通过 |
| `RESEARCH_LIVE_SMOKE=1 go test ./internal/research -run '^TestLivePublicPageSmoke$' -count=1 -v` | 真实读取 RFC Editor 的 RFC 9110，核对正文 `HTTP Semantics`，14 个真实片段；明确标记 `partial_text_limit`，没有把 HTTP 200 当证据 |

数据库使用已有 PostgreSQL 17.11 测试服务，每个场景创建独立随机 schema，串行运行并由测试清理。网络夹具注入受信解析/拨号依赖，仍经过生产地址、预算、解析与引用检查；没有给生产代码增加 loopback 来源开关。

公开网页 smoke 的正文 SHA-256 为 `21c1cdce6ab0e5509b04d84a28000836c7a087cf786efe6f04877ebfff47232a`。这仅验证真实公共网页读取，不替代 Brave 真实提供商搜索 smoke。

## 尚未验收，不计通过

- 当前工作树无前端依赖，按用户 AGENTS.md 已询问 `npm ci` 安装授权，尚未收到回复；因此 TypeScript、Vitest、Web 构建和 Playwright 浏览器验收未运行。不可据此宣称完整浏览器流程验收通过。
- 基线 `schema.d.ts` 尚未包含 #21 的运行接口。本次用已有 Go YAML 依赖按 OpenAPI 补齐运行/研究声明，没有安装替代依赖；安装获准后仍应运行项目标准 `npm run generate`，统一生成格式并执行类型检查。
- 未获得获准使用的真实搜索提供商配置和可能费用授权，Brave 搜索 smoke 与人工核对搜索结果未运行。HTTP 搜索夹具、真实公开网页读取均不替代此项。
- 未推送、创建 PR 或合并基础分支；以上验收缺口补齐前不宣称关闭 Issue #22。

## 真实搜索浏览器闭环补验（2026-10-11）

在独立 worktree `research-live`、分支 `issue-22-research-live` 上从
`main@67e37e3` 补验。此前“尚未验收”保留为历史记录；本节补齐第 1 项和
第 12 项的真实搜索提供商 smoke，**第 12 项的人工来源核对仍待真人完成**。
GitHub issue、主仓库和其他 worktree 未修改。

独立用例为 `clients/web/tests/live/research-live.spec.ts`，配置为
`clients/web/playwright.live.config.ts`。显式要求 `WEB_LIVE_SEARCH=1`，只运行
Chromium，单 worker、零自动重试。默认配置仍只收集 `tests/browser`：本次
核对到 14 个文件、44 个 Chromium 场景，live 用例不进入发行门禁。

### 结论与真实证据

| 验收项 | 结论与证据 |
| --- | --- |
| 第 1 项：空资料目标的浏览器研究闭环 | **通过。** 浏览器分别用 `settings`、`research` 档案配对，经设置页保存 Brave 凭据并显式测试连接；新建目标后核对独立空知识库返回 404。目标页发起研究，以一次请求预算停在候选阶段，核对候选没有正文、片段或获取时间，页面显示“候选，尚未读取”和无原文副本；随后通过正式续预算命令读取真实公网正文与 PDF，运行经本地确定性模型综合。浏览器点击采纳一个 HTML 来源，核对来源列表/详情 API、正式知识修订身份、引文修订和 UTF-8 字节范围，支持链接打开对应片段；刷新后综合结果、采用状态和引文仍在，外部请求数未增加。`pageerror` 为零。 |
| 第 12 项：真实外部提供商 smoke | **通过。** Brave 固定探测查询 `education`，探测状态 `ready`、实际请求 1 次；研究查询 `(site:www.rfc-editor.org) (RFC 9111 HTTP Caching)`，实际搜索 1 次，得到 2 个真实候选。正式抓取所得正文、片段和引文均已核对，未以 HTTP 200 或搜索摘要代替正文证据。 |
| 第 12 项：人工核对来源 | **待真人完成。** 下方清单只准备核对位置与片段，不把自动原文/引文一致性断言计为真人审阅。 |

公开研究主题为 `RFC 9111 HTTP Caching`，来源范围为精确主机
`www.rfc-editor.org`。最终研究运行为
`4b3157b9-7442-4dca-84df-cd76a2a7a169`，状态 **`partial`**：正文有明确解析
遗漏，页面如实显示“有缺口，未完整完成”和覆盖说明。

| 真实来源 URL | 最终采用状态 | 覆盖 | 片段数 | 获取时间（UTC） |
| --- | --- | --- | --- | --- |
| https://www.rfc-editor.org/info/rfc9111/ | `adopted`，已纳入本目标参考 | `partial_text_limit` | 14 | 2026-10-11 12:18:26.07554667 |
| https://www.rfc-editor.org/rfc/rfc9111.pdf | `partial`，未采用 | `partial_pdf` | 19 | 2026-10-11 12:18:26.452613277 |

采用来源的正文指纹为
`9820f38d7ec7054655ed012561854ff33487623338e92e093b554c624110acc3`，来源 ID
`a73cba2a-f2c8-4d41-8ca9-1afb8dd79d27`，来源修订
`a734b0aa-41ea-4bf8-9766-b09c800c61ab`，正式知识修订
`4e2536f0-7058-4a0b-bad7-0a80ae0824df`。PDF 指纹为
`2663227a94ec8a81892a02f5697fc15c578571cdee96407adb5c60a60236c76f`。

浏览器观察延迟：连接探测 **679 ms**；从发起研究到候选预算暂停 **2149 ms**；
续预算后到正文读取与综合终态 **2227 ms**。这些时间包含服务调度、轮询与页面操作，
不是提供商纯网络时延。研究预算共消耗 4 次请求：1 次 Brave 搜索、2 次真实来源
GET、1 次本地模型请求；另有一次 Brave 连接探测。最终通过轮 Brave 调用 **2 次**，
含此前调试的整个补验累计 **16 次**，低于 20 次上限。

模型只使用 `WEB_WORKSPACE_FIXTURE=1` 的本地 HTTP 夹具，最终模型统计为
`calls=1`、`historyRequests=0`。其要点文本固定为偶数示例，与 HTTP 缓存主题不符；
本次验证研究结果展示、真实引用和来源采纳通路，不把固定回答计为真实模型质量或
HTTP 知识正确性验收。真实来源正文没有被夹具替换。

### 产品缺陷、修复与失败尝试

补验发现合法 PDF 原件导致研究中断：RFC 9111 PDF 原件为 566083 字节，抓取与
解析成功，但数据库保存仍将整个运行 JSON 与模型正文的 256 KiB 上限比较，产生
`partial / research_failed`，未生成综合结果。既有加密层、附件验证与 checkpoint
计算已经明确区分模型正文和 PDF 原件；问题是 `Service.save()` 没有使用同一边界。

最小修复提交 **`d7f1f65`**：保存时沿用 `maxSealedBody` 和
`checkpointJSONSize()`，保持模型正文上限及附件身份/大小验证。改动仅在
`server/internal/mentorrun/store.go` 与对应 PDF 回归，不涉及公共 API/OpenAPI、
migration 或新增用户语义。真实 PostgreSQL 的既有 PDF 采纳用例改用合法的大原件：
修改前复现相同 `research_failed`，修改后完成解析、加密保存、重启恢复、明确部分
采纳和正式来源读取；既有“PDF 附件不能扩大模型载荷预算”用例仍通过。

| 尝试与产物前缀 | 结果 | Brave 调用 |
| --- | --- | --- |
| `chromium-initial` | 测试额外请求了 research 档案没有权限的参考管理 API，403；移除这项测试权限假设，以空知识库检查验证起点。 | 1 |
| `chromium-second` | 下拉框精确标签定位超时，尚未搜索；改用 combobox 并增加操作超时。该轮未留下终态证据 JSON，失败日志/Playwright JSON 保留。 | 1 |
| `chromium-third` | 裸域名限制下未出现候选预算暂停；后续按生产精确主机规则使用 `www.rfc-editor.org`。该轮未保存终态快照，不把推断写成已核实终态。 | 2 |
| `chromium-fourth` | RFC 9110 搜索得到真实候选，但未形成带引文的综合；独立公开网页检查确认其 HTML 为 1187554 字节，超过 1 MiB 抓取上限。保留生产限制并调整主题。 | 2 |
| `chromium-fifth` | 带 `filetype:txt` 的 RFC 9110 查询得到 `research_search_invalid_response`；未抓取提供商原始响应进一步分类，未把它认定为瞬时网络故障。 | 2 |
| `chromium-sixth` | RFC 9111 已真实读取正文，PDF 保存缺陷阻断综合；页面终态断言超时。 | 2 |
| `chromium-seventh` | 新增终态诊断步骤读到了续预算前的旧暂停状态；修正为先等待命令回执，再按回执版本等待终态。 | 2 |
| `chromium-eighth` | 确认服务端为 `partial / research_failed`，已读取 HTML、PDF 仍是候选；定向检查和 PG 回归定位为保存上限缺陷。 | 2 |
| `chromium-final` | **1 passed，0 failed，0 skipped，0 retries，14.1 秒，退出码 0。** | 2 |

未进行自动网络重试，也未使用“允许重跑一次”把失败轮记为通过。
所有尝试共用同一 Brave 账本；只调整测试步骤/公开样本和上述最小产品修复。

### 命令与结果

从本 worktree 根目录运行；使用已有依赖、测试 PostgreSQL 和 schema 隔离工具。
Vite runner 加载避免向共享的 `node_modules/.vite-temp` 写入。

```bash
cd clients/web
npm run build -- --configLoader runner
cd ../../server
go build -tags web_release -o edu-agentd ./cmd/edu-agentd
cd ../clients/web

export WEB_LIVE_SEARCH=1
export WEB_LIVE_EVIDENCE_DIR=/tmp/edu-issue22-live.hkRhGf
export TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable'
export WEB_CHROMIUM_PATH='/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome'
export DEBUG=pw:webserver
export PLAYWRIGHT_JSON_OUTPUT_FILE="$WEB_LIVE_EVIDENCE_DIR/chromium-final.json"

/tmp/edu-web-baseline-6fe9a7a.5ZH8lk/webdatabase npm run test:browser -- \
  --config playwright.live.config.ts --project=chromium --reporter=line,json \
  --output="$WEB_LIVE_EVIDENCE_DIR/artifacts-final" \
  > "$WEB_LIVE_EVIDENCE_DIR/chromium-final.log" 2>&1
```

最终 JSON 的 `expected=1`、`unexpected=0`、`skipped=0`、`flaky=0`，没有收集错误。
每轮浏览器均由 `webdatabase` 创建独立随机 schema，结束后仅清理本轮 schema。
最终证据为 `/tmp/edu-issue22-live.hkRhGf/evidence-1791721109394.json`，调用账本为
同目录的 `brave-attempts.json`；服务端日志在各轮 Playwright `.log` 中，原始产物不提交。

另在 `server` 对实际修复运行：

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable' \
  go test ./internal/mentorrun \
  -run '^(TestPDFAttachmentsDoNotExpandModelPayloadBudget|TestPostgreSQLPDF.*|TestPostgreSQLResearchPipelineAdoptionAndIsolation|TestPostgreSQLResearchBudgetAndForgedCitation)$' \
  -count=1 -v
go vet ./internal/mentorrun
```

相关 5 个顶层回归全部通过（含 2 个预算/伪引文子场景），20.973 秒；本包 vet 与
服务重建通过。真实 RFC 9110 纯文本的既有公开网页 smoke 也通过：14 个片段、
`partial_text_limit`，仅用于正文格式诊断，不增加 Brave 计数。临时诊断测试已移除。
前端类型检查、独立 live 配置检查、定向格式检查与 `git diff --check` 通过。
未运行全量浏览器矩阵、全项目 Go/race、发行门禁或 Compose；构建仅出现既有
依赖注释和 bundle 大小提示。

Key 从仓库外的 0600 环境文件只读加载，不导出为服务环境变量。禁用 trace、视频、
截图、浏览器 API 调试及自动页面快照；密码输入和脱敏断言不在失败输出中打印 Key。
最终轮 8 个设置 API 响应均检查到零 Key 回显。结束时经设置页清除测试服务凭据；
超时轮停止后的临时设置文件也已清理，原始环境文件保留。所有服务日志、Playwright
JSON/产物及证据目录内容均做 Key 字节匹配检查，结果为零；两个提交前均将
`git diff --cached` 接入 `grep -c`，匹配数为零。

### 给真人的来源核对清单（待真人完成）

- [ ] **唯一采用来源：** https://www.rfc-editor.org/info/rfc9111/ 。标题应为
  `HTTP Caching`，RFC 编号 9111。获取时间与正文指纹见上表；网页更新时应记录差异。
- [ ] **被引用片段：** 原文片段 1，ID `d2ab60bf-e7a4-43e3-8e7e-ae7239161510`，
  已保存静态文本的 UTF-8 字节范围 **[0, 1200)**。完整片段和逐字引文在最终证据 JSON
  的 `adopted.fragments[0].text` / `citation.quote` 中；页面支持链接定位为
  `#fragment-d2ab60bf-e7a4-43e3-8e7e-ae7239161510`。这是抽取文本范围，不是 HTML 文件
  源码或原网页字符偏移。
- [ ] **原网页应能找到的位置：** 页面顶部导航后，RFC 标题及作者元信息，随后是
  `Abstract` 和 `Status of This Memo` 的开头。搜索短句
  `This document defines HTTP caches`，以及 `This document obsoletes RFC 7234.`；
  与完整引文逐项比对，排版空白和段落标记允许按抽取器行为核对，正文不能凭空增加。
- [ ] **覆盖和结论边界：** 核对片段确实只覆盖页面起始部分，`partial_text_limit`
  没有冒充通篇阅读。PDF 未采用，不属于本轮采用来源清单。固定模型夹具的偶数示例
  不作为该 RFC 支持的知识结论；真人来源核对结果、核对人、日期和差异仍需另行记录。

第 1 项和第 12 项的真实搜索部分已补齐；真人审阅仍未完成，因此不宣称 Issue #22
全部验收完成，也未推送、创建 PR 或修改 issue。
