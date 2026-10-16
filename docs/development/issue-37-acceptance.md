# Issue #37：在线发布验收最终状态与人工交接

整理日期：2026-10-16（UTC）；核查基线：`617f8b509c9d4a7bb3faf5890bfc3bd081d1f293`（PR #113）；分支：`docs-37-40-final`。本次只核查文档、Git 历史及 GitHub 只读信息，没有重跑产品测试、调用提供商、部署、修改 issue 或生成新的候选验证结果。

## 发布结论

**实现及已执行的技术验收已落地，建议建立后续人工验收单并回链后关闭 #37；当前不能把原 issue 的全部发布门禁勾为通过，也不能据此声明已获生产发布验收。** #18–#36 均已关闭；旧浏览器缺口、真实研究、模型协议、升级恢复、容器与 SSE 已有后续证据。现行主流程以 PR #96 确认的对话式学习区及 [spec 1–30](../comet/specs/notebook-workspace/spec.md) 为准，不再要求用户手工选目标、先生成课程路线才能学习。

剩余必需工作集中于真机／人工：真实 IME、软键盘／安全区／工具栏、读屏器、三类教学质量与来源审查，以及尚未实测的交互性能和候选证据完整性复核。后两项也必须保留，不能因自动化计数全绿而省略。可选真实 NoteSync 上游仍未运行（外部），与受控上游通过分开报告，启用前另补实际环境确认。整本书 PDF／大文档精准检索属于 [#111](https://github.com/ErzerLP/edu-agent/issues/111)，OCR 尚未承诺，不倒列为 #29 或本轮的实现缺陷。

## 证据口径与索引

“通过”表示对应现行合同有已执行证据，不表示本次又跑过或所有环境均通过。表中状态仅使用“通过／未通过／未运行（外部）／未运行（真机或人工）”；已修复的历史失败仍保留在原记录中。fixture、真实 PG／CLI／浏览器、真实提供商及真人签收分别记账，原型与截图不充当生产测试。

| 引用 | 已执行证据与适用范围 |
| --- | --- |
| E1：旧任务补验 | PR #75、#76、#77：[#30 浏览器补验](issue-30-acceptance.md#浏览器补验2026-10-11)、[#32 端到端补验](issue-32-acceptance.md#端到端补验2026-10-11)、[#34 最终结论](issue-34-acceptance.md#按-issue-验收项的最终结论)。分别覆盖知识维护／草稿／清除、真实复习与同服务 CLI、记忆纠正／导出／删除／重放。PR #78 的[离线核对竞态](issue-37-offline-flake.md)已闭合；#38/#39 仍是独立专项。 |
| E2：真实研究与模型 | PR #84 的[#22 真实搜索浏览器闭环](issue-22-acceptance.md#真实搜索浏览器闭环补验2026-10-11)：真实 Brave、RFC 正文／PDF、预算续接及采纳，模型仍为 fixture，真人来源核对未完成。PR #83（issue #82）的[Responses smoke](issue-82-acceptance.md#真实-smoke)、#85/#94 的[真实开学修复](start-learning-e2e-fix.md#本地真实端到端结果)、#95 的[连续两次真实端到端](real-model-robustness.md#连续两次真实端到端)补齐真实模型运行；#95 两轮置信度 720，正确保留 provisional。 |
| E3：升级与部署 | PR #79 的[升级、回滚、备份恢复](issue-37-upgrade-backup.md)，#86 的[CLI 同版本与外部备份决定](issue-37-upgrade-backup.md#用户决定2026-10-12)；#81 的[容器、版本、缓存与路由](issue-37-release-deploy.md#容器版本缓存与路由结果)及[真实 Nginx SSE](issue-37-release-deploy.md#真实-nginxsse-时间与连接回收)。[#67 验收结果](issue-67-acceptance.md#验收结果)另证明真实 Nocturne Compose full、受管备份销毁及销毁后恢复拒绝。 |
| E4：现行 C1–C4 | [Notebook 方向变更与实施记录](../design/notebook-workspace.md#方向变更2026-10-12)：复用 #87–#93 的壳层、来源、引用与 Studio 底座；#97（C1）、#98/#99（C2）、#101/#102（C3a/b）、#103（C4）；#100/#104/#105/#107 为公式、弹窗／Markdown、独立评估预算、GFM 修复。C2 `TestPostgreSQLStudyLiveResponsesSmoke`、C3b `TestPostgreSQLStudyExerciseLiveResponses`、C4 真实模型测验均通过；C3b 出题→作答→自动评估 accepted 有真实模型证据。 |
| E5：C5a/b 候选 | PR #106/#108：[C5b 候选验收记录](../design/notebook-workspace.md#c5b-候选验收记录2026-10-1415)。三引擎 223 项、真实 PG 七个稳定分片 374 项（另有生命周期补验 2 项）、定向 race 399 项最终通过；不是全仓 race。父 spec 原记录仍是 **24 通过、0 未通过、4 未运行（外部）、2 未运行（真机）**。下表浏览器文件见该节逐文件结果；PG 具名测试见其分片／必需名称映射。 |
| E6：候选后修复 | PR [#109](https://github.com/ErzerLP/edu-agent/pull/109)：类型／构建、28 项相关单测、Chromium 25 场景；[#110](https://github.com/ErzerLP/edu-agent/pull/110)：停止幂等、30 项单测、Go 停止／HTTP 测试及 Chromium 聊天／Studio 停止；[#112](https://github.com/ErzerLP/edu-agent/pull/112)：10 项单测、Chromium PDF／来源 5 场景；[#113](https://github.com/ErzerLP/edu-agent/pull/113)：13 项单测、类型／构建、受影响 Go test/vet／发行构建、Chromium 来源／参考／PDF 7 场景。局部通过不改写 C5b 输入或计数。 |
| E7：演示站补充观察 | 来源为本次任务提供的走查结果：真实模型讲解、出题、评估（置信度记录为 930/900、自动采纳）、Studio 指南／简报均已走通。仓库未找到该次走查日期、SHA、操作 ID 或原始日志，仅作补充观察，不伪造日志／测试名，不替代质量签收或冻结候选。 |

**候选证据限制：** E5 原 `make web-release-candidate` 退出 2，其后按文件补验汇总为绿，不能改称原命令一次通过。44 份原始浏览器 JSON 及原发行前置详细日志缺失，现有汇总／哈希与部分新报告不等于完整可重放证据包。41 个必需 PG 名称中，36 个有新 PG／race 直接证据，5 个仅有原 guard 历史记录。需人工核定复用是否充分、恢复原件或只补必要证据。C5b 基于 `0d77554` 及 #108 测试修复，不直接代表 #109–#113 后整套矩阵重跑；后续使用 E6 局部结果，正式发布仍须绑定实际候选 SHA。

## 在线能力矩阵与方向映射

`v1` 指既有 `/v1` HTTP；协议版本沿用既有协商值。入口及旧测试迁移见设计的 G／J 节。

| 能力／任务 | 现行入口、兼容与版本边界 | 已执行证据 |
| --- | --- | --- |
| 身份／学习区／目标／共享核心 #18/#19 | `/app/` 卡片墙、名称／方向创建；`/app/spaces/:id/manage` 设置；旧目标详情／历史兼容保留。学习动作惰性维护系统目标，CLI `space/goal` 和 agentcore 保留。 | E4/E5 `learner`、`learning`、`notebook-chat`；[#19 核心检查](issue-19-acceptance.md#实际执行的检查)。 |
| 配置／运行／对话 #20/#21/#33 | 设置仍需 settings；主区统一输入、保存重开、临时模式和恢复；learner 无 admin／清除授权。capabilities schema 1、原运行／conversation 协议保留。 | E2/E4/E5/E6；`settings`、`mentor`、`tutor-history`、`notebook-chat`。 |
| 研究／参考／导入／PDF #22/#24/#27–#29 | 左栏 PDF／文本／网址／搜索，先审阅保存再勾选，不自动外发方向。旧研究、知识页／任务中心兼容；PDF 为有限文本层，非整书／OCR。 | E2/E5/E6；`notebook-sources`、`references`、`tasks`、`pdf`；#113 同名修订不循环、旧引用读旧版。 |
| 内容／版本／动态调整 #23/#25/#26 | 对话讲解、内嵌练习／反馈；新方向影响后续轮次。旧 `/learn/:session`、`/app/content/:id` 的选段／版本／变更兼容保留，不宣称聊天消息可原位改写正式块。content/change protocol 1。 | E4/E5；`notebook-exercise`、`workspace`；C3 单题发布／评估任务。 |
| 知识维护／评估／进度 #30–#32 | 菜单知识维护、评估与证据、进度／复习；新卡片共用既有事实，不按聊天次数计掌握。structure protocol 1、进度投影 2。 | E1/E4/E5；`knowledge-structure`、`workspace`、`review`。 |
| 记忆／隐私／设备 #34 | 全局长期记忆、独立清除 grant 与设备撤销保留；草稿仅标签页内存。 | E1/E3/E5；`memory`；真实 Nocturne／受管备份用 #67 证据。 |
| Studio／NoteSync／导出 #25/#35，#91–#93/#103 | 四种真实产物及指定版本；测验取消选目标门槛，笔记转来源为审阅后的独立副本。旧内容库、默认映射 NoteSync／导出兼容保留。 | E4/E5；`notebook-studio`、`notesync`；真实 NoteSync 上游仍未运行。 |
| 同教学跨端继续 #36 | `study/learn/assessment/progress/reviews` 共用服务事实；服务端／CLI 同版本升级，不承诺旧二进制或 CLI 新聊天 UI。 | E3/E5；`review`、`TestPostgreSQLStudyCLIResearchAndWebContent`／`TestPostgreSQLStudyCLIAndWebShareChanges`；[#36 双端](issue-36-acceptance.md#真实生产-cligo-http-与-pg)。 |

浏览器测试短名均指 `clients/web/tests/browser/<名称>.spec.ts`。本地 Shell／PTY、浏览器离线为 #38/#39 独立交付，非 #37 前置；不声明网页等价全部原生 CLI。OCR、Office、音视频生成、代码执行判题、证明验证、多用户学校系统不在本轮声明内。

## A01–A36 总追踪

行为定义来自 [#40](https://github.com/ErzerLP/edu-agent/issues/40)，按现行 spec 映射。“兼容保留”指旧入口仍受回归保护，不是新主界面必经步骤。测试名定位已执行记录；E5 证据缺件限制统一适用，不因每行写“通过”而消失。

| 编号／行为 | 责任与现行入口／spec | 当前结论 | PR／章节／测试证据 |
| --- | --- | --- | --- |
| A01 空库无上传首活动 | #24；spec 3/10/14/15：方向→对话，按需搜索保存来源后出题；旧研究开学兼容。无上传不等于零来源可生成有依据练习。 | 通过 | E2 #84 真实 Brave 空库起步、#94/#95 真实研究开学；E4/E5 `TestPostgreSQLStartLearningEmptyLibrary`、`TestPostgreSQLStudyExerciseAtomicFailureAndZeroSources`、C3b live。新合同不要求对话自动研究。 |
| A02 保存不建会话／调模型 | #18/#24；spec 2/3：保存方向不启动教学，旧仅保存目标兼容。 | 通过 | E4 #98、E5 spec 2/3；`learning`、`notebook-chat`、`TestPostgreSQLStudyModeIsExplicitAndSaved`。 |
| A03 已知复用／未知可追问跳过 | #24；spec 3/14/15：方向／轮次上下文，普通交流无需答完澄清；旧开学澄清兼容。 | 通过 | E2 #95、E4 C2、E5 `start_test.go`／`notebook-chat`；追问质量另列人工，不据自动化推断教学质量。 |
| A04 候选／获取／解析／采用准确 | #22/#29；spec 7–10/13，左栏来源／覆盖／引用。 | 通过 | E2 #84 真实正文和 partial 报告；E5 `pdf`／`notebook-sources`；E6 #112/#113 超限／无文本／同名修订。 |
| A05 失败／预算停止恢复 | #20–#22；spec 14/25/29，聊天／研究／Studio 恢复。 | 通过 | E2 #95、E4 #105 独立评估预算；E5 `tasks`／`mentor`、`TestPostgreSQLStartLearningBudgetRecovery`；E6 #110。 |
| A06 查询去标识／外发授权 | #20/#22/#27；spec 10/12/27，明确公开关键词／网址／当前选择。 | 通过 | E2 #84；E5 `notebook-sources`、`TestPostgreSQLResearchRequiresSpecificConsentAndKnowledgeScope`、`TestPostgreSQLStudySourceAndDirectionIsolationReopen`。 |
| A07 参考角色影响检索 | #27；spec 8/11/12，新轮用当前选择；旧目标参考角色兼容。 | 通过 | E5 `references`／`notebook-chat`；E4 `TestPostgreSQLExerciseOriginalDirectionAndSources` 验证旧题不随改选漂移。 |
| A08 审阅／确认／未知回执 | #27–#29；spec 8/9/17/29，添加来源／任务核对。 | 通过 | E5 `references`／`tasks`／`pdf`；E6 #113 文档／章节决定累计及同名平移修订场景。 |
| A09 大任务恢复不重复发布 | #28；spec 25/29，任务中心兼容保留。 | 通过 | E5 `tasks`“大清单分段、刷新重启、来源变化与提交响应丢失按原任务恢复”及取消／归档场景。 |
| A10 概念／来源分离、冲突可见 | #24/#30；spec 7/13，知识维护辅助页。 | 通过 | E1 #75 的 #30“浏览器补验”；E5 `knowledge-structure` 三场景／引擎及 PG 多来源／主张／关系检查。 |
| A11 内容身份／版本／块定位 | #23/#25；spec 13/15/20/23，新卡片／Studio；旧内容块兼容。 | 通过 | E5 `workspace` 版本／选段、`notebook-studio` 指定版本；E4 C3 session/activity/content 事务映射。 |
| A12 半成品不可作答 | #23–#25；spec 15/17/20/25，题发布后可提交。 | 通过 | E4 #101/#102；E5 `TestPostgreSQLStudyExercisePublishesOneCard`、`TestPostgreSQLExercisePublicationRollbackAndHelpState`、`notebook-exercise`。 |
| A13 局部改写保留阅读／草稿 | #25/#26；spec 6/28/29，旧选段兼容；对话追加不覆盖原题。 | 通过 | E5 `workspace`“选段模型加工、答案保留、Studio、来源、固定与补偿版本”；`notebook-workspace` 草稿隔离。 |
| A14 普通解释无需逐轮审批 | #25/#26；spec 14，连续对话；旧解释调整兼容。 | 通过 | E4 C2 live；E5 `notebook-chat`、`TestPostgreSQLAdaptiveApprovalVersionsAndExplanation`；E7 真实讲解补充观察。 |
| A15 标准变化确认具体差异 | #26/#27；spec 2/3/11/29，明确保存方向版本；旧具体变更审批兼容。 | 通过 | E5 `notebook-chat` 方向冲突／修订、`workspace` 具体变更、`TestPostgreSQLAdaptiveGoalRevisionAndQueueFences`。 |
| A16 旧批准不用于新候选 | #21/#25/#26；spec 2/29，原批准绑定具体版本。 | 通过 | E5 `TestPostgreSQLAdaptiveApprovalVersionsAndExplanation`、`workspace`／`notebook-studio` 冲突。 |
| A17 作答中安全／立即接入 | #23/#26；spec 11/15/28，新方向不改旧题；旧排队／立即切换兼容。 | 通过 | E5 `workspace`“教学变更显示具体差异、排队、立即切换与原题草稿焦点恢复”；E1 #75；E4 原方向／来源 PG。 |
| A18 补偿保留既有证据 | #26/#30；spec 6/19/29，旧变更／知识维护兼容。 | 通过 | E1 #75；E5 `knowledge-structure`、`workspace` 补偿版本、`TestPostgreSQLAdaptiveQueueImmediateAndCompensation`。 |
| A19 建议／规则／正式证据分开 | #31；spec 18/19，反馈卡／评估详情。 | 通过 | E5 `workspace` 复核／覆盖、`notebook-exercise` 待复核；E4 C3b accepted，E2 #95 provisional 未冒充 accepted。 |
| A20 揭示／自述不升掌握 | #26/#31/#32；spec 16/18/19，帮助下限、accepted 才计证据。 | 通过 | E4/E5 `TestPostgreSQLExerciseSubmissionAndHelp`、`TestPostgreSQLExerciseProvisionalAndAllowedHelp`、`TestPostgreSQLStudyExerciseHelpSubmissionConcurrency`、`notebook-exercise`。 |
| A21 分母变化不冒充能力下降 | #32；spec 3/19，无伪方向完成比例，旧进度保留。 | 通过 | E1 #76“端到端补验”；E5 `workspace` 动态进度、`TestProgressExplainsRouteVersionsWithoutInventingMastery`。 |
| A22 投影失败不是没有复习 | #32；spec 19，进度／复习菜单。 | 通过 | E1 #76；E5 `workspace`“动态进度真实跨区续学、版本、范围、失败与原答案入口”、`review`。 |
| A23 双标签／双区无串扰 | 横向 owner；spec 11/12/17/28，区／对话／卡片草稿隔离。 | 通过 | E5 `learning`、`workspace` 迟到响应、`notebook-workspace`／`notebook-studio`；`TestPostgreSQLStudySourceAndDirectionIsolationReopen`。 |
| A24 服务端拒绝跨区实体 | 各 owner；spec 11/26–29，正式 HTTP／PG。 | 通过 | E5 PG core/privacy 分片、`TestPostgreSQLStudyCLIResearchAndWebContent`；E4 C3b/C4 原方向／来源、跨设备读取检查，不只依靠前端筛选。 |
| A25 恢复不重复副作用 | #21/#24/#26/#28/#31/#33/#36；spec 14/17/25/29。 | 通过 | E5 `tasks`／`mentor`／`notebook-exercise` 丢响应、`TestPostgreSQLExerciseLeaseRecovery`、`TestPostgreSQLExerciseRecoveryChecksAssessmentOperation`。 |
| A26 草稿不默认明文落盘 | #18/#23/#33；spec 28，未发送／提交／保存输入。 | 通过 | E5 `learning`／`workspace`／`tutor-history`／`notebook-workspace` 存储断言；授权离线库另计。 |
| A27 临时要求不自动长期保存 | #34；spec 26/28，记忆明确审阅。 | 通过 | E1 #77 的 #34 最终结论第 2 项、`TestPostgreSQLMentorMemory*`；E5 `memory` 内嵌批准。 |
| A28 清除后不复活正文 | 各 owner/#34；spec 28/29，新方向／卡片／评估任务同样纳入。 | 通过 | E1 #75/#76/#77；E5 privacy 分片、`TestPostgreSQLStudyExerciseClearCannotRevive`／`TestPostgreSQLExerciseGlobalErasureCancelsInFlight`；E3 #67 受管备份。外部快照边界见下文。 |
| A29 Key 不入页面／存储／日志 | #18/#20/#21/#33–#35；spec 27/28。 | 通过 | E5 `settings` Key 不回显、`memory`／`notesync` canary 脱敏；E2 真实调用凭据检查；E1 #34 成功导出。 |
| A30 学习登录无 admin、撤销拒写 | #18/#20/#34/#35；spec 26–28，learner 只组合学习 scope。 | 通过 | E4 #97 `TestPostgreSQLLearnerAdminIssuanceAndHTTPPermissionMatrix`；E5 `learner`／`memory`；E3 #81 撤销 SSE 后 401。 |
| A31 CLI 兼容或明确受限 | 协议 owner/#36；spec 6/19/22/30，同版本，非旧版或全原生平台保证。 | 通过 | E3 #86；E5 CLI／资产小节、`review` 生产 CLI；[#36](issue-36-acceptance.md#真实生产-cligo-http-与-pg)同教学调整／幂等。macOS 另列。 |
| A32 四视口无整页横溢出 | 各页面；spec 4/30，主区及兼容页。 | 通过 | E5 #106/#108 `notebook-accessibility` 五宽度 390/768/1024/1280/1440、双主题、公式／代码／长标题，三引擎；`learning` 四视口。 |
| A33 IME／焦点／键盘 | 各页面；spec 16/30，自动化 composition／键盘／焦点通过，真机未签收。 | 未运行（真机或人工） | E5 spec 16/30 明列未运行；`notebook-accessibility` 键盘全流程、`learning` 合成 IME 不能替代原生中文输入、软键盘和读屏器。 |
| A34 XSS／来源工具注入阻断 | #22/#23/#25/#29；spec 13/27，安全 Markdown／公式／PDF。 | 通过 | E5 `workspace` 来源安全、`notebook-chat`“学习区回答渲染 Markdown、公式与引用，代码和 HTML 保持安全”、`pdf`；E2 research 伪引用／来源拒绝。 |
| A35 DNS／跳转／IPv6／压缩安全 | #22/#29；spec 10/27，抓取边界不随入口变化。 | 通过 | E2 #84 真实网络；[#29 检查](issue-29-acceptance.md#已执行并通过)、`TestFetchPublicBoundary`／`TestFetchRedirectRebindingAndScope`／`TestFetchParsingLimitsAndRestrictions`。恶意边界用受控测试，非公网逐项攻击重演。 |
| A36 缺资产失败／404 不回 SPA | #18/#37；spec 6/27/30，发行／错误路径。 | 通过 | E3 #81 Docker 缺资产负例、无 Node、直连／HTTPS 路由；E5 `release` 14 深链接／8 错误入口；E6 #113 发行构建。 |

A 表合计 **35 通过、0 未通过、0 未运行（外部）、1 未运行（真机或人工）**，只代表现行功能映射。三类质量、性能、原件等附加义务仍在下表，不以此合计替换 E5 父 spec 历史 24/0/4/2。

## 八步核心脚本的现行等价证据

以下是**新真实模型流程＋现行自动化＋兼容回归的组合证据**，不是宣称在同一次新空库、真实提供商、浏览器与 CLI 中连续跑完原八步。

| 原步骤 | 当前等价行为／结论 | 已执行证据及边界 |
| --- | --- | --- |
| 1. 空库配对、输入目标，无上传 | 通过：learner 配对、创建区／方向，保存不发模型。 | E4/E5 `learner`／`learning`／`notebook-chat`；E2 #84/#95 空库研究。不强制普通对话自动外发方向。 |
| 2. 真实正文、尽早首活动 | 通过：可先讲解，显式搜索／审阅后勾选出题，无整课生成前置。 | E2 #84 Brave／正文；E4 C2/C3b live、E7 讲解／出题。各链路组合，非一次连续走查。 |
| 3. 选段改解释、核对正式版本 | 通过：新对话形成新轮次、不覆盖原题；旧原位选段入口兼容。 | E5 `workspace` 选段、artifact/version/block 和原草稿，C2 对话／引用版本。未把追问说成更新旧正式块。 |
| 4. 作答中补前置、安全切换 | 通过：新卡片稍后／恢复，新方向不改旧题；旧排队／apply_now 兼容。 | E4/E5 `notebook-exercise`、`TestPostgreSQLExerciseOriginalDirectionAndSources`、`workspace` 排队／立即切换；E1 #75。 |
| 5. 接收答案后丢响应、不重复 | 通过：原 operation 核对，同一 Attempt／评估／Evidence。 | E5 `notebook-exercise`“C3b 出题、帮助、丢响应核对、自动反馈、引用、收起恢复与进度”；`ExerciseTaskFailureRollsBackAttempt`／`ExerciseRecoveryChecksAssessmentOperation`。已受理后丢响应，与发出前 abort 分开。 |
| 6. 刷新／重启恢复 | 通过：保存对话／卡片／反馈重开，评估任务恢复；草稿仅内存。 | E5 `learning` 进程重启、`mentor`／`tasks`、`TestPostgreSQLStudyExerciseOldContextAndRestart`／`TestPostgreSQLExerciseLeaseRecovery`。 |
| 7. CLI 同教学继续／调整、另区不变 | 通过：CLI 同事实，旧受支持调整合同兼容。 | E5 `review` 正式复习＋CLI；[#36 双端](issue-36-acceptance.md#真实生产-cligo-http-与-pg)两个 `StudyCLI*` 场景。CLI 不必拥有新对话 UI。 |
| 8. 暂停／撤销／清除与迟到任务竞态 | 通过：生命周期／generation fence 阻断非法发布和复活。 | E5 `StartLearningLifecycleRace`、`ExerciseRevocationAndRotation`／`ExerciseGlobalErasureCancelsInFlight`、`memory`／`knowledge-structure`；E3 #81 SSE 撤销、#67 受管备份。 |

**保留缺口：** 尚无绑定最终 SHA、同一次真实浏览器／CLI／模型的完整八步记录。按已确认新方向使用上述组合覆盖现行行为；若发布签收仍要求旧原文“一次贯通”，需在人工单中安排适配新入口的综合走查，不能事后把拼合证据写成已经执行。

## 发布附加项

| 发布附加项 | 当前结论 | 已执行证据／剩余边界 |
| --- | --- | --- |
| 真实搜索／模型运行可用性 | 通过 | E2 #83/#84/#94/#95，E4 C2/C3b/C4/#105，E7 补充。#80 的 schema／开学阻断已有修复及成功链路，不再列为当前实现失败；旧报告不改写。 |
| 三类模型教学质量签收 | 未运行（真机或人工） | [#80 三类样本](issue-37-model-samples.md#三类教学样本)概念／推导、代码／算法、长文阅读当时均阻断；后续调用未补齐三类质量审查。需审引用、针对性调整、目标保护、帮助／不确定性、延迟与实际 token／费用；置信度或一次 accepted 不是质量结论。 |
| 来源／真实教材独立核对 | 未运行（真机或人工） | [#22 真人清单](issue-22-acceptance.md#给真人的来源核对清单待真人完成)、[#29 页图核对](issue-29-acceptance.md#页图核对与发现的问题)；#112/#113 技术补验已过，仍需真人对照正文、页码、字体、缺口和旧版本引用。 |
| 真实代理 SSE | 通过 | E3 #81：真实 Nginx/TLS，心跳比较最大差 1.134ms、活跃流超过 75s、静默探针 75072ms 关闭、断开／撤销回收及管理隔离。C5b 自身未跑代理，不否定既有结果，也非最终站点全量重验。 |
| 三浏览器／视口／主题／长内容／键盘 | 通过 | E5 Chromium 77、Firefox 73、WebKit 73，五宽度、双主题、长公式／代码、焦点／axe；E6 仅后续局部 Chromium。C5b 原件缺失另列。 |
| 原生 IME／软键盘／安全区／辅助技术 | 未运行（真机或人工） | E5 spec 16/30：iOS Safari／Android Chrome 中文确认／Enter、工具栏、缩放／横竖屏、安全区、VoiceOver／NVDA 播报／MathML／焦点。合成事件／模拟视口／axe 不替代。 |
| 性能目标实测 | 未运行（真机或人工） | 未有本地反馈 <100ms、流式 30–60ms 合并更新的环境／样本数／p50/p95 记录；E5 分页行为、E3 SSE 时延和 E4 模型耗时不等于这些指标。人工采样并如实报告未达项，不编造测量或静默删除目标。 |
| 容器／资产／schema／缓存／404 | 通过 | E3 #81 无 Node、缺资产失败、63 资产直连／HTTPS 一致、schema/capabilities、敏感 API no-store；E5 `release`，E6 后续发行构建。 |
| 迁移／旧数据／同版本 CLI／离线签名 | 通过 | E3 #79 升级 19→33、原活动／答案／Evidence、Web 开关回滚、golden／签名；E4 后续迁移／清除及 E5 PG／CLI。#86 同版本同步升级；旧 CLI `progress` 拒绝 `data_cleared` 的失败保留于不保证的跨版本范围，不声称所有开关／升级组合均演练。 |
| 备份恢复／权限／Nocturne 受管备份 | 通过 | E3 #79 独立库恢复／解密、错密钥拒绝、密钥／设置 0600；#67 full 证明受管备份销毁／恢复拒绝。#86 明确 pg_dump/WAL/宿主快照在应用清除保证外，恢复会复活旧身份／正文；运维须自行销毁／轮换。 |
| 安装／升级／保存／权限／限制文档 | 通过 | E3 #86、[Web README](../../clients/web/README.md)、[环境配置](../../deploy/env.example)及本表。learner 不带 settings/admin/清除 grant；PDF 与 #111 边界明确。 |
| 候选证据完整性与最终签收 | 未运行（真机或人工） | E5 缺 44 份原始 browser JSON／前置明细、5 个 PG 名称仅历史 guard，需评定复用／补原件或最小检查。E7 缺日期／SHA／操作记录；#109–#113 后需绑定实际发布输入，非整套矩阵重跑。 |
| 真实 NoteSync 上游（可选集成） | 未运行（外部） | [#35 待验收](issue-35-acceptance.md#待验收没有宣称通过)、E5 `notesync`：真实 Go/PG、受控 REST 上游通过，实际远端 opt-in 未运行。默认区／默认集合限制保留，启用前另补 smoke。 |

E5 原列外部 spec 10/14/18/21 的真实调用部分已有 E2/E4/E7 补充，来源与讲解／评分／指南／简报质量仍需人工逐项签收。保留原候选 **24/0/4/2**，不据零散 smoke 改成父 spec 28 项通过。当前范围未发现有证据支持的未修复技术失败；未运行／证据不足不等于通过。

## 可复现入口与历史初验

沿用现有锁文件和已准备的依赖／三个浏览器引擎，技术检查入口为 `make web-release-check`；完整候选为 `TEST_DATABASE_URL=<独立可销毁测试库> make web-release-candidate`。后者构建真实 CLI、串行运行 PG 与逐文件浏览器矩阵，包含全局清除，不得使用已有实例数据。测试端口 32929–32931 及测试库不可与其他矩阵共用；脚本不安装依赖、不拉镜像、不调用真实付费提供商、不部署。补验时按 E5/E6 的输入／环境判断最小范围，不为本次文档整理重跑。

保留 2026-09-19 初验的历史定位：基线 `b0ddfac`、分支 `task/58`，修复四个页面刷新 404 与九条代理 API 漏放；`TestWebRelease*`、`TestWorkspaceProxyAllowlist`、`TestWebProduction*` 通过。真实 PG／生产 CLI 定向检查为 25 个顶层、含子例 57 项通过、0 fail/skip，模型／搜索／来源均为 HTTP fixture；当时前端依赖未准备，Web／浏览器未运行，不能倒改为当时通过。后续结果以前文为准。

原 PG 检查入口：`cd server && go test -json -p=1 -count=1 -timeout=10m ./internal/transport/httpapi ./internal/mentorrun -run '^TestPostgreSQL(Web|StudyCLI|StartLearning|Research|Adaptive|MentorCookieHTTPAndSSERecovery)'`，需设置独立 `TEST_DATABASE_URL` 与 `EDU_AGENT_INTEGRATION_CLI`。原日志 `/tmp/edu-agent-task58-postgres.jsonl`，SHA256 `a760dfddb5d236c70ff87a40c66854cb8195943323bea64a03eddefc1efe17b0`；本机路径不是随仓库交付的附件，本次未核定其仍可读取。

## 关闭方式与后续人工验收单草案

严格保持 #37 原全部勾选标准时，目前不可标为全部完成。建议先建立后续单、复制未运行项并回链，再以“实现与自动化收尾，剩余发布签收独立跟踪”关闭 #37；不等待 #111，也不把关闭当成发布授权。[#40 最终跟踪](issue-40-acceptance.md)采用同一口径。本次未创建或关闭任何 issue。

建议标题：**发布前人工验收：对话学习区真机、教学质量、性能与证据签收（承接 #37/#40）**。

建议正文：承接 #37/#40 的人工发布门禁，按现行 Notebook spec 1–30。已执行自动化见 PR #108／C5b（223 浏览器、374 PG、399 定向 race）及 #109/#110/#112/#113 局部补验，不重建已交付功能，不包含 #111 的整本书 PDF，也不扩张为 OCR。关闭原单不代表下列项目通过。

- [ ] 记录发布 SHA／资产摘要、站点、OS／浏览器／设备、操作者和日期；综合走查方向→讲解→选来源→出题→帮助／提交→反馈／复核→刷新恢复→Studio，按需核对 CLI 同事实。若仍要求原八步一次贯通，在可销毁隔离环境补兼容选段／路径调整与撤销／清除，记录操作身份。
- [ ] 真机中文 IME：iOS Safari／Android Chrome 及声明支持的桌面输入法，确认词、Enter／换行、切卡片不误发／误提交。
- [ ] 真机软键盘、工具栏伸缩、缩放／横竖屏、安全区及长内容；提交不遮挡，草稿与焦点正确。
- [ ] VoiceOver／NVDA：生成／停止／反馈播报、MathML、标签／菜单／弹窗和引用焦点返回，记录限制。
- [ ] 概念／推导、代码／算法、长文阅读各一例：独立核对正文／页码／缺口，审讲解、调整、目标保护、帮助、评分／待复核、Studio 指南／简报，记录模型、预算、延迟、已知 token／费用及不确定性。优先复用已授权结果；新增调用按实际授权执行，不用置信度 930/900 或一次自动采纳代替审查。
- [ ] 性能：记录设备／环境、样本数、p50/p95，实测反馈 <100ms、流式合并 30–60ms 和分页请求；如实登记未达项。
- [ ] 证据签收：恢复／审核 44 份原始报告及前置明细，核定 5 个历史 guard 名称；无法接受复用时只补必要检查。补演示站日期／版本／操作，明确 #109–#113 后的证据复用范围。

退出条件：每项有通过／未通过／未运行、设备／样本及证据链接；未通过需修复或明确范围决定，未运行不得默认放行。启用真实 NoteSync、声明 macOS 原生或真机离线支持等可选环境时另附 smoke／真机结果，不因未启用能力扩大核心范围。

## 安装、升级、保存与回退

安装配置见 [Web README](../../clients/web/README.md) 和[部署环境说明](../../deploy/env.example)。Node 仅用于构建，运行使用带 `web_release` 资产的 Go 服务及 PostgreSQL。配置精确 HTTPS Origin、配对 scope、模型端点允许列表和受保护 Key；教学配置保存后按说明重启。未发送／提交／保存草稿仅标签页内存，明确保存的对话／内容／产物在服务端受保护存储。

升级前分别备份 PG、配置、正文密钥和外部记忆数据并限制权限；先在隔离副本核对迁移、解密、身份和原课堂，再同步升级同版本服务端、CLI、资产与代理。外部数据库备份不受应用清除保证，运维自行销毁／轮换清除前快照；恢复旧快照会恢复旧身份与正文，密钥轮换前备份仍需旧密钥。依据见[#86 用户决定](issue-37-upgrade-backup.md#用户决定2026-10-12)。

回退优先关闭受影响能力，保留读取／清除，不删新表／改历史 checksum，不让旧代码强写未知 schema。已演练路径见 E3，不推断所有开关、历史版本或生产权限已逐一验收。
