# Issue #37：在线发布候选记录

日期：2026-09-19；基线：`b0ddfac`；任务分支：`task/58`。
方案与先确认的代码位置见 [web-release.md](../design/web-release.md)。

## 发布结论

**未达到完整在线发布验收，不能据此关闭 #37 或发布生产版本。** 已修复确认的四个页面
刷新 404 与九条代理 API 漏放，补充自动化候选入口及三浏览器配置。
真实提供商、完整浏览器核心脚本及下面列出的外部/人工项目仍需取得实际证据。
“自动化候选 passed”只描述该命令的覆盖，不代表所有本表项目通过。

此处没有使用旧原型 32 项检查，也没有把历史任务摘要或截图算作当前生产验收。
本机桥接 #38、浏览器离线 #39 不属于本轮在线前置；不声明网页等价于全部原生 CLI。

## 复现与修复

新增回归后，在修改生产代码前运行：

```bash
cd server
go test ./internal/transport/httpapi ./api -run '^TestWebRelease' -count=1
```

真实 `webAsset` handler 对 `/app/progress`、`/app/memory`、`/app/settings/data`、
`/app/settings/devices` 返回 404。原因是 `web.go` 的页面白名单未随前端路由扩展。
已补齐四个确定的页面，未开放任意 settings 子路径。

从 Web 实际请求提取路径并匹配部署配置，发现以下 API 全部落到代理默认 404：
`/v1/learning/progress`、`/v1/learning/reviews`、
`/v1/knowledge/imports/previews`、`/confirm`、`/operations/:id`、
`/v1/knowledge/reference-sources`、`/v1/knowledge/scopes/:id`、`/:id/export`、
`/v1/knowledge/revisions/:revision/documents/:document/pages/:page`。
已按正式 API 边界补齐，直接导入 `/v1/knowledge/imports`、内部/管理/未知写路径仍不公开。
回归从 `main.tsx` 与 Web 消费路径取样，后续页面或 API 漏接会再次失败。

## 可复现检查入口

先按已有锁文件安装依赖（需要操作者授权），无需升级依赖：

```bash
cd clients/web
npm ci --no-fund
cd ../..
make web-release-check
# 显式提供可销毁的专用测试库，并准备三个 Playwright 引擎之后：
make web-release-candidate
```

`web-release-check`：生成一致性、类型/单测、受影响 Go 契约/vet、Web + Go 发行构建、
缺资产失败。普通 Go 测试里没有数据库的 skip 不作为数据库证据。
`web-release-candidate` 再构建真实 CLI，串行运行真实 PostgreSQL 集成与所有浏览器文件，
每个文件重启夹具；三个引擎分别运行，禁止将 Chromium 路径传给 Firefox/WebKit。
通过 `WEB_RELEASE_MATRIX=1 npm run test:browser -- --project=firefox` 可定向调试，
定向结果不能代替完整候选。脚本不安装依赖、不拉取镜像、不调用真实付费提供商、不部署。

候选需要 `TEST_DATABASE_URL` 指向独立测试库；浏览器包含全局清除，不得指向既有实例数据。
本地测试监听 32929–32931；同一主机不能同时运行其他占用这些端口或数据库资源的矩阵。
证据写入命令输出的临时目录：提交 SHA、全部相关源码输入摘要、工具/宿主环境、
执行时间、命令日志摘要和浏览器 JSON 报告。输入中途变化、没有用例、失败、skip、
意外“预期失败”或浏览器重试均不生成候选通过。报告保存在本机，不提交私人测试数据。
通过记录只能在相同输入/工具/环境复用，旧文档不是可复用的机器证据。

## 本次实际结果

| 检查 | 当前结论 |
| --- | --- |
| 先失败的页面/代理回归 | 确认 4 个页面与 9 条 API 漏接 |
| 修复后 `TestWebRelease*`、`TestWorkspaceProxyAllowlist`、`TestWebProduction*` | 通过；错误路由、管理边界和 Cookie 配置保留 |
| `go test ./api ./internal/transport/httpapi ./internal/research -count=1` | 通过；未配置 PG 的 skip 不计数据库证据 |
| `go vet ./api ./internal/transport/httpapi` | 通过 |
| `make cli-build` | Linux 生产 CLI 构建通过 |
| `go build ./cmd/edu-agentd` | Go 开发构建通过，不等于带资产发行 |
| `go build -tags web_release ./internal/webassets` | 缺资产按预期失败：`pattern dist/assets: no matching files found` |
| `node --test scripts/web-release-results.test.mjs` 与脚本语法 | 通过；验证 skip/空选择/失败不能冒充通过 |
| `make web-release-check` | 预期失败：未安装 Web 依赖，未自动安装，未宣称前端通过 |
| 真实 PG/CLI 定向集成 | 25 个顶层用例、含子例 57 项通过，0 失败、0 skip；不是浏览器证据 |
| Web 类型/单测/生成/发行及三浏览器 | 未运行；依赖安装授权待回复 |

环境：Linux amd64、Go 1.26.6、Node 24.20.0；独立临时 PostgreSQL 17.11，
现有本机镜像 ID `sha256:67f41722b7a8cbdb868a44a4995c846eddfdc2973bccb291ce937dce88ad5675`，
仅监听 `127.0.0.1:32958`。数据库集成每例使用独立 schema，真实 CLI 使用临时客户端目录。
未修改现有部署或用户配置。测试结束后删除了本次可销毁 PostgreSQL 容器及数据库，未保留恢复副本。

真实 PG 命令（`TEST_DATABASE_URL` 与 `EDU_AGENT_INTEGRATION_CLI` 指向上述隔离环境）：

```bash
cd server
go test -json -p=1 -count=1 -timeout=10m ./internal/transport/httpapi ./internal/mentorrun \
  -run '^TestPostgreSQL(Web|StudyCLI|StartLearning|Research|Adaptive|MentorCookieHTTPAndSSERecovery)'
```

本机 JSONL 日志 `/tmp/edu-agent-task58-postgres.jsonl`，SHA256
`a760dfddb5d236c70ff87a40c66854cb8195943323bea64a03eddefc1efe17b0`；该路径不是随仓库交付的证据附件，
可按命令重现。覆盖 Web 配对/目标/导入、SSE 恢复、原 CLI 与 Web 同状态、三类目标空库开学、
失败/预算恢复、同名跨区、具体变更审批/补偿、暂停/撤销/清除竞态。模型/搜索/来源均为 HTTP
fixture；数据库和 CLI 是真实实现。HTTP 两端通过不能代替浏览器呈现与未保存草稿验证。

## 在线能力矩阵

`v1` 指已有 `/v1` HTTP；若能力没有独立协商版本则明确标注，不能杜撰版本号。
所有入口均为仓库已有生产代码；测试文件是证据入口，不代表本次已运行。

| 能力 / 实现任务 | Web 生产入口 | CLI / 能力版本 | 证据入口与限制 |
| --- | --- | --- | --- |
| 学习区、目标结构/历史/生命周期 #18 | `/app/`、`/app/spaces/:id`、`…/goals/:id` | `space`、`goal`；操作 envelope 1 | `learning.spec.ts`；Go Web 身份/目标集成 |
| 共享核心 #19 | 导师和学习服务使用共享核心 | CLI agent；无单独 Web 能力版本 | `packages/agentcore`；历史 #19 不自动重计 |
| 模型/搜索/预算 #20 | `/app/settings` | CLI 本地模型与服务器配置独立；capabilities schema 1 | `settings.spec.ts`；提供商未实测 |
| 导师运行/自由问答 #21、#23 | 目标/课堂侧栏、`…/chat` | `learn`、`study mentor`；v1 运行协议 | `mentor.spec.ts`、`workspace.spec.ts` |
| 研究/出处/无上传开学 #22、#24 | `…/goals/:id/research`、首页开始学习 | `study research/start`；start protocol 1 | `mentorrun/start_test.go`、`research_test.go`；浏览器核心路径缺证据 |
| 内容/版本/Studio #23、#25 | `…/learn/:id`、`/app/content/:id`、`…/studio` | `study content/context`；content protocol 1 | `workspace.spec.ts`；独立版本、选段、补偿 |
| 规划/动态调整 #26 | 课堂导师和具体变更面板 | `study change`；change protocol 1 | `mentorrun/changes_test.go`、`workspace.spec.ts` |
| 集合/共享/范围/单批导入 #27 | `…/knowledge`、目标参考入口 | `knowledge library/import`；v1，无独立 Web 协商版本 | `references.spec.ts`；代理漏接已修 |
| 大任务导入/任务中心 #28 | `/app/runs`、`/app/runs/import/:id` | `knowledge import`；v1 import-jobs | `tasks.spec.ts` |
| 文本层 PDF #29 | 知识来源、PDF 页面出处 | 既有来源协议；无独立 Web 协商版本 | `pdf.spec.ts`；不支持扫描件 OCR |
| 知识结构/维护/回滚 #30 | `…/knowledge` 维护面板 | 知识维护与 `study` 变更；structure protocol 1 | `knowledge-structure.spec.ts`；旧文档维护默认区限制 |
| 评估处置/继承 #31 | `…/feedback`、`…/feedback/:attempt` | `assessment`；v1，独立审批 scope | `workspace.spec.ts`；继承仍有默认区限制 |
| 进度/复习 #32 | `/app/progress`、目标详情 | `progress/reviews`；服务端投影版本 2 | `workspace.spec.ts`；不能把投影失败当空列表 |
| 导师历史/临时模式 #33 | `…/chat/:conversation` | CLI 本地加密历史不自动转换；v1 conversation | `tutor-history.spec.ts` |
| 记忆/隐私/设备 #34 | `/app/memory`、`/app/settings/data`、`…/devices` | `memory`、`privacy`、设备命令；v1，memory 配对档案 | `memory.spec.ts`；Nocturne 外部交付未实测 |
| NoteSync/导出 #35 | `…/notesync`、知识/内容导出 | 既有 NoteSync/知识导出；v1，无独立 Web 协商版本 | `notesync.spec.ts`；仅默认映射，真实远端未实测 |
| 同一教学跨端继续 #36 | 课堂/进度读取同一状态 | `study`、`learn`；协商 content/change 1 | `study_cli_external_test.go`；HTTP Web 不是浏览器证据 |

本地文件、Shell/PTY、原生钥匙串、浏览器离线、音视频、代码执行判题、证明验证、OCR、
Office、多用户学校/班级系统不在当前在线发布声明内。未知交互沿用明确升级提示。

## A01–A36 总追踪

定义沿用 [#40](https://github.com/ErzerLP/edu-agent/issues/40)。下表中的 G 是本次 Go/PG
定向检查，B 是 `clients/web/tests/browser/` 的生产浏览器测试入口，H 是前序记录。
“部分”表示只具备服务端或 fixture 证据，完整生产验收仍未通过；B/H 不自动计为通过。

| 编号 / 必须证明的行为 | owner | 生产入口 / 证据入口 | 本候选结论 |
| --- | --- | --- | --- |
| A01 空库无上传首活动 | #24 | 首页；G `StartLearningEmptyLibrary` | G 通过；真实浏览器/提供商未通过 |
| A02 保存不建会话/调模型 | #18/#24 | 首页仅保存；G `WebHTTPIdentityAndRealGoals`、B learning | G 通过；浏览器未通过 |
| A03 复用已知/未知可追问跳过 | #24 | 开学；G start_test.go | G 通过；浏览器未通过 |
| A04 候选/正文/解析/采用准确 | #22/#29 | 研究/来源；G research_test.go、B pdf | G 通过；真实搜索/PDF 浏览器未通过 |
| A05 失败/预算停止恢复 | #20–#22 | 运行页；G start_test.go/research_test.go | G 通过；外部提供商未通过 |
| A06 查询去标识/外发确认 | #20/#22/#27 | 研究确认；G research_test.go、H #22 | G 通过；真实外发未通过 |
| A07 参考角色影响检索 | #27 | 目标参考；B references、H user-references 设计 | 未运行，不计通过 |
| A08 审阅/确认/未知回执 | #27–#29 | 参考/导入；B references/tasks/pdf | 未运行，不计通过 |
| A09 大任务恢复不重复发布 | #28 | 任务中心；B tasks | 未运行，不计通过 |
| A10 概念与来源分离/待核实 | #24/#30 | 研究/知识；G start_test.go、B knowledge-structure | 部分，浏览器未通过 |
| A11 内容身份/版本/块定位 | #23/#25 | 课堂/Studio；B workspace | 未运行，不计通过 |
| A12 流式半成品不能作答 | #23–#25 | 课堂；B workspace、G start_test.go | 部分，浏览器未通过 |
| A13 局部改写保留草稿 | #25/#26 | 课堂选段；B workspace | 未运行，不计通过 |
| A14 普通解释无需逐轮批准 | #25/#26 | 导师/选段；G changes_test.go、B workspace | 部分，浏览器未通过 |
| A15 标准变化确认具体差异 | #26/#27 | 变更面板；G changes_test.go | G 通过；浏览器未通过 |
| A16 旧批准不能用于新候选 | #21/#25/#26 | 变更面板；G changes_test.go | G 通过；浏览器未通过 |
| A17 作答中安全/立即接入 | #23/#26 | 课堂；G changes_test.go、B workspace | G 通过；草稿浏览器未通过 |
| A18 补偿保留既有证据 | #26/#30 | 变更/维护；G changes_test.go、B knowledge-structure | 部分，维护浏览器未通过 |
| A19 建议/规则/正式证据区分 | #31 | feedback；B workspace | 未运行，不计通过 |
| A20 揭示/自述不升掌握 | #26/#31/#32 | 课堂/反馈/进度；B workspace | 未运行，不计通过 |
| A21 路径分母变更准确 | #32 | progress；B workspace | 未运行，不计通过 |
| A22 投影失败不是没有复习 | #32 | progress；B workspace | 未运行，不计通过 |
| A23 双区/标签页无串扰 | #18/#21/#23/#33/#36 | 目标/课堂/聊天；G StartLearningConcurrentGoals、B learning/workspace | 部分，标签页未通过 |
| A24 服务端拒绝跨区组合 | 各 owner | 正式 API；G Web/Research/StartLearning/StudyCLI | 部分，未重跑所有 owner |
| A25 恢复不重复副作用 | #21/#24/#26/#28/#31/#33/#36 | 运行/课堂；G StudyCLI/Adaptive、B tasks/workspace | 部分，浏览器及丢响应闭环未通过 |
| A26 草稿不默认明文落盘 | #18/#23/#33 | 全部编辑页；B learning/workspace/tutor-history | 未运行，不计通过 |
| A27 临时要求不是长期授权 | #34 | memory/导师；B memory | 未运行，不计通过 |
| A28 清除不复活正文 | 各 owner/#34/#37 | 数据页；G ResearchGlobalErasure/StartLearningGlobalErasure、B memory | 部分，全 owner 贯通未通过 |
| A29 Key 不进入页面/存储/日志 | #18/#20/#21/#33–#35 | 设置/导师；B settings/tutor-history/notesync | 未运行，不计通过 |
| A30 学习身份无 admin/撤销拒写 | #18/#20/#34/#35 | 配对/设备；G WebHTTPIdentityAndRealGoals、B memory | 部分，设备浏览器未通过 |
| A31 CLI 兼容或明确受限 | 协议 owner/#36 | CLI study/learn；G StudyCLI、H #36 | G 跨端通过；完整兼容与原生全平台未通过 |
| A32 四视口不整页横溢出 | 各页面/#37 | 全部 Web；B learning/workspace 等 | 未运行，三引擎配置不算通过 |
| A33 IME/焦点/键盘正确 | 各页面/#37 | 全部 Web；B learning/workspace | 未运行，原生输入法/软键盘未通过 |
| A34 XSS/来源工具注入阻断 | #22/#23/#25/#29 | 来源/正文；G research、B workspace、内容块单测 | 部分，浏览器渲染未通过 |
| A35 DNS/跳转/IPv6/压缩边界 | #22/#29 | 来源获取；G `internal/research` | HTTP fixture 通过，真实网络代理串联未通过 |
| A36 缺资产失败/404 不回 SPA | #18/#37 | 发行/代理；G WebRelease/WorkspaceProxy | 路由回归通过，完整发行/容器未通过 |

## 八步核心脚本与剩余发布门禁

使用新建测试数据库、研究档案配对的浏览器、独立 CLI 客户端和明确授权的搜索/模型配置。
记录 commit/input SHA、资产摘要、协议版本、浏览器/视口、服务/数据库版本、操作 ID 与时间。
先验证库内无资料；不能调用 legacySession 导入夹具代替这一前置。

1. 浏览器输入目标，仅保存时核对零新教学会话、零外部调用；再明确授权研究和开学。
2. 检查研究运行、获取成功的正文片段和引用身份，进入首活动；记录首活动时刻及缺口。
3. 选段改解释，核对 artifact/version/block/range；未选部分和原答案草稿保持原值。
4. 作答中提出补前置，分别检查排队与明确立即切换；恢复原活动时核对原题/草稿。
5. 代理在服务器接收答案后丢弃响应，记录原 operation，查询并重试；核对答案、评估及
   Evidence 的数据库身份/数量，没有第二份。不能在请求发出前 abort 冒充丢响应。
6. 刷新、停止并重启服务，核对原活动/正式结果；未保存草稿丢失须与提示一致。
7. CLI 配对后 `learn show --session ID`、`study context --session ID` 读取原教学；
   按 `study help` 发起支持的具体调整，Web 显示同一候选/结果；核对另一学习区未改变。
8. 运行排队期间分别暂停目标、撤销设备、执行授权清除；释放迟到响应后确认无非法新活动，
   旧页面、运行、索引和引用不能恢复正文。此操作只针对可销毁验收库。

当前 G 覆盖其中的服务端/CLI 部分；**尚无一份贯通八步的浏览器真实提供商证据**。
外部 smoke 只用明确允许外发的公开任务，不能借用用户已有部署中的秘密。

| 发布附加项 | 执行/记录要求 | 当前结果 |
| --- | --- | --- |
| 三类模型样本 | 概念/推导、代码/算法、长文阅读；逐项记录引用支持、针对调整、目标保护、费用、延迟、不确定性 | 只有已有三学科 HTTP fixture；真实提供商/人工样本未运行 |
| 代理 SSE | 部署候选 Nginx，观察心跳跨代理即时到达、75s 超时配置、禁缓冲、断开/撤销后连接回收；直接 SSE 用例不能替代 | 未运行真实代理 |
| 三浏览器与原生设备 | 390/768/1280/1440、主题、长内容、键盘/焦点/读屏阶段；原生 IME、软键盘、安全区另记录设备 | 未运行；没有声明支持实测范围 |
| 性能 | 环境/样本数/p50/p95，输入反馈目标 <100ms；流式合并目标 30–60ms；分页请求数量 | 未测量，不编造数字 |
| 容器/版本/缓存 | 无 Node 运行镜像、资产/schema/capability 一致、敏感 API no-store、404 边界 | 路由单测通过；发行/容器与部署未运行 |
| 迁移/旧数据/离线签名 | 旧版本库副本升级与只读/清除、关闭新能力后保留新数据，原离线 golden/签名回归 | 本次未改迁移；旧库升级与全部兼容矩阵未运行 |
| 备份/恢复与权限 | 独立库恢复演练，确认密钥/设置/正文都可解密，撤销/清除后不重新上线旧快照 | 未运行恢复演练 |

## 安装、升级、保存与回退

安装和配置见 [Web README](../../clients/web/README.md)、[部署环境说明](../../deploy/env.example)。
Node 只在前端构建阶段需要；运行使用带 `web_release` 资产的 Go 二进制和 PostgreSQL。
HTTPS 精确 Origin、独立配对档案、模型端点允许列表、服务器 Key 文件按既有文档配置。
教学配置修改后重启生效；未提交草稿仅进程/标签页内存，正式状态保存于服务端。

升级前在维护窗口备份 PostgreSQL、配置文件、正文加密密钥及相关外部记忆数据，分别保存
且限制权限；先在隔离副本恢复并检查版本、迁移、解密、登录与原课堂读取，再更新同一候选
的 Go、静态资产与代理配置。密钥轮换前的备份仍需要相应旧密钥，不把数据库快照当独立完备备份。
外部提供商、导出文件、WAL、宿主备份不属于应用清除保证，须按实际保存策略单独处置。

回退优先关闭受影响的新能力，保留可读和清除路径；不要删除新表、修改历史迁移 checksum
或让旧代码强写未知 schema。旧二进制不能确认读取新数据时保留当前服务并停用受影响功能。
真正回退方案需在副本验证；本记录不宣称现有所有开关已经通过只读/清除测试。
