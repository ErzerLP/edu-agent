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
