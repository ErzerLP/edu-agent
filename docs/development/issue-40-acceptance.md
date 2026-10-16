# Issue #40：实施、验收与方向变更最终跟踪

整理日期：2026-10-16（UTC）；基线 `617f8b509c9d4a7bb3faf5890bfc3bd081d1f293`（PR #113），分支 `docs-37-40-final`。本次只读核对 #37/#40、issue 状态、PR、Git 历史及验收文档，只更新本文件和 [#37 最终验收记录](issue-37-acceptance.md)，未重跑产品测试、部署或修改 GitHub issue。

## 总体结论

**#40 的实现导航与覆盖核查已完成，建议在 #37 剩余项移入后续人工验收单并回链后关闭。** 原在线实现 #18–#36 全部 CLOSED，独立专项 #38/#39 也已关闭。#37/#40 当前仍 OPEN，本文件建议不是 GitHub 状态变更。实现存在、功能验收完成、全部发布签收完成须分账。

PR #96 确认新方向为**学习方向→对话讲解→内嵌练习→自动反馈，两侧保留来源与 Studio**。旧目标／课堂／内容／辅助功能和 CLI 按兼容合同保留，不要求新用户手工创建目标或先生成路线。现行依据为 [spec 1–30](../comet/specs/notebook-workspace/spec.md)、[方向变更、旧测试迁移与 C1–C5 记录](../design/notebook-workspace.md#方向变更2026-10-12)。

## 子任务实现与验收分账

“验收”综合 owner 记录与后续补验，历史“未安装依赖／浏览器待运行”不自动成为当前缺口；issue 关闭也不抹掉外部／真机未运行。C5b 原始报告缺件限制统一见下文。

| 顺序／issue（当前状态） | 实现状态／当前入口 | 验收状态与证据 |
| --- | --- | --- |
| W01 #18（CLOSED） | 已落地：Web 身份／区／目标，新首页名称／方向，旧目标兼容。 | 功能验收完成：[#18 实际检查](issue-18-acceptance.md#实际通过的检查)、C5b `learning.spec.ts`／`notebook-workspace.spec.ts`。真实 IME 后续人工承接。 |
| W02 #19（CLOSED） | 已落地：CLI／服务端共用 agentcore。 | 范围内验收完成：[#19 核心／CLI](issue-19-acceptance.md#实际执行的检查)；C5b 同服务 CLI。macOS 交叉构建不等于真机通过。 |
| W03 #20（CLOSED） | 已落地：秘密配置、预算／探测／capabilities；settings 独立。 | 功能验收完成：[#20](issue-20-acceptance.md)、C5b `settings.spec.ts`；#83/#94/#95 真实 Responses／超时／引用修复。 |
| W04 #21（CLOSED） | 已落地：持久运行／SSE／原回执恢复，新对话复用。 | 功能验收完成：[#21](issue-21-acceptance.md)、C5b `mentor.spec.ts`／`tutor-history.spec.ts`、#81 真实 SSE、#110 幂等停止及 Chromium。 |
| W05 #22（CLOSED） | 已落地：研究／抓取／出处，新搜索／网址来源；旧研究页兼容。 | 技术闭环完成：[#22 真实搜索](issue-22-acceptance.md#真实搜索浏览器闭环补验2026-10-11)、#84 Brave／正文／PDF、C5b 来源／网络边界；真人来源核对仍待签收，关闭状态不等于人工通过。 |
| W06 #23（CLOSED） | 已落地：版本内容／答案／续学，旧课堂兼容，新卡片共用状态机。 | 功能验收完成：[#23](issue-23-acceptance.md#已执行验证2026-09-16)、C5b `workspace.spec.ts`、C3/C4 自动反馈／真实模型；软键盘另列。 |
| W07 #24（CLOSED） | 已落地：无上传研究开学；新方向替换手工目标／开课门槛，旧 API 保留。 | 现行等价验收完成：[#24](issue-24-acceptance.md)、#94/#95 真实研究开学、C2/C3b live、C5b 对话／练习；零来源不伪造有依据练习。 |
| W08 #25（CLOSED） | 已落地：内容／块／版本／协作，旧选段兼容，新 Studio 独立产物。 | 功能验收完成：[#25](issue-25-acceptance.md)、C5b `workspace.spec.ts` 选段／草稿／补偿、`notebook-studio.spec.ts`；不把聊天追问说成原位改正式块。 |
| W09 #26（CLOSED） | 已落地：具体审批、安全排队／立即切换／补偿；旧课堂入口兼容。 | 功能验收完成：[#26](issue-26-acceptance.md)、C5b `workspace.spec.ts` 变更／焦点及 `AdaptiveQueueImmediateAndCompensation`；新方向不改旧题标准。 |
| W10 #27（CLOSED） | 已落地：参考角色／集合／范围、单批审阅；新左栏决定后续轮次。 | 功能验收完成：C5b `references.spec.ts`／`notebook-sources.spec.ts`、原方向／来源 PG；#113 修复决定丢失及同名修订保留旧引用 Chromium 场景。 |
| W11 #28（CLOSED） | 已落地：大任务／任务中心，原操作核对。 | 功能验收完成：[#28](issue-28-acceptance.md)、C5b `tasks.spec.ts` 两场景／引擎，分段／丢响应／重启／取消／归档／设备归属。 |
| W12 #29（CLOSED） | 已落地：有限文本层 PDF、覆盖报告／物理页引用。 | 声明范围技术验收完成：[#29](issue-29-acceptance.md#已执行并通过)、#41 预编译及 #108 race 补验、C5b PDF、#112/#113；独立阅读器／字体／页码仍待真人。整书／精准检索另为 #111，OCR 未承诺。 |
| W13 #30（CLOSED） | 已落地：结构／冲突／维护／回滚，辅助页保留。 | 功能验收完成：#75 的[#30 浏览器补验](issue-30-acceptance.md#浏览器补验2026-10-11)，三引擎图／列表／草稿／清除；C5b `knowledge-structure.spec.ts`。 |
| W14 #31（CLOSED） | 已落地：评估／复核／继承，新反馈读同一决定／Evidence。 | 功能验收完成：[#31](issue-31-acceptance.md)、C5b `workspace.spec.ts` 复核／覆盖／继承、C3b/C4 live accepted；三类评分质量仍待人工。 |
| W15 #32（CLOSED） | 已落地：进度／复习／跨目标，新方向不展示伪完成比例。 | 功能验收完成：#76 的[#32 补验](issue-32-acceptance.md#端到端补验2026-10-11)，正式复习＋生产 CLI 同事实；C5b `review.spec.ts`／`workspace.spec.ts`。 |
| W16 #33（CLOSED） | 已落地：历史／临时／外发确认，study 持久对话与旧 mentor 分别保留。 | 功能验收完成：[#33](issue-33-acceptance.md)、C5b `tutor-history.spec.ts`／`notebook-chat.spec.ts`，C2 live、C3 卡片重开／TTL 恢复。 |
| W17 #34（CLOSED） | 已落地：记忆审批／纠正／删除／导出、隐私／设备，记忆仍全局。 | 功能验收完成：#77 的[#34 最终结论](issue-34-acceptance.md#按-issue-验收项的最终结论)，三引擎及额外 WebKit；[#67](issue-67-acceptance.md#验收结果)真实 Nocturne／受管备份；C5b `memory.spec.ts`。 |
| W18 #35（CLOSED） | 已落地：默认区／集合 NoteSync、冲突／任务／导出。 | 本地／受控上游功能验收完成：C5b `notesync.spec.ts` 真实 Go/PG／REST fixture；[#35 实际远端 smoke](issue-35-acceptance.md#待验收没有宣称通过)仍未运行（外部），启用实际远端前另验，不称全部外部集成通过。 |
| W19 #36（CLOSED） | 已落地：CLI 同教学研究／内容／变更／恢复，新 Web 不要求 CLI 改聊天 UI。 | 同版本兼容验收完成：[#36 双端](issue-36-acceptance.md#真实生产-cligo-http-与-pg)、C5b `review.spec.ts` 及 CLI 具名 PG/race；#86 同步升级，macOS 原生待真机。 |
| W20 #37（OPEN） | 已落地：发布 runner／边界／子项及新流程证据汇总。 | 已执行技术检查完成，**全发布签收未完成**；A 表 35 通过、A33 真机／人工未运行，附加项／缺件见[#37](issue-37-acceptance.md#发布附加项)。建议转后续单后关闭，未实际关闭。 |
| X01 #38（CLOSED） | 已落地：可选 companion，本机工具由独立程序和明确设备授权执行。 | [#38 已执行检查](issue-38-acceptance.md)、旧 #40 真实 Nginx 核查见下节；不扩大为所有原生 OS／真实 Shell 新候选全验收。非在线前置。 |
| X02 #39（CLOSED） | 已落地：授权加密离线库／可靠同步，不改变在线草稿不落盘。 | [#39 实际验证](issue-39-acceptance.md#实际验证)、#78 flake 修复、C5b `offline.spec.ts` Chromium 7、Firefox/WebKit 各 3 个能力降级场景；非后两引擎完整持久离线，真机离线未验。非在线前置。 |

2026-10-16 只读核实：#21/#30/#32/#34/#41/#22/#29/#82 均 CLOSED；#41、#82 为已解决的 PDF race／Responses 阻断，不能继续列为待实现。

## 新方向交付与验收

| 批次／PR | 实现状态 | 已执行验收与边界 |
| --- | --- | --- |
| Shape #96 | 已确认并作为后续实施基线。 | spec 1–30、设计 G／J 节定义替代关系及旧 URL／测试保留；Shape 自身不是运行证据。 |
| 底座 #87–#93 | 已落地：卡片墙／三栏壳层、来源目录／四种添加方式、所选来源问答／引用、Studio 笔记／指南／简报／测验。 | 后续 C1–C5 复用与回归；旧中栏目标／课堂和手选测验目标被新方向替代，旧数据／深链接仍保留。 |
| C1 #97 | learner 一次配对组合学习权限。 | scope、旧设备不扩权、settings/admin/清除独立的 HTTP/PG 与浏览器通过；C5b `learner`。 |
| C2 #98/#99 | 学习方向、系统目标、持续对话和原型 v2 布局。 | 方向／来源隔离、版本／重开／零来源及真实讲解 smoke；C5b `notebook-chat`／`notebook-workspace`。 |
| C3a/b #101/#102 | 单题发布／帮助下限／Attempt、可恢复自动评估及练习／反馈卡。 | 真实 PG、Chromium、`TestPostgreSQLStudyExerciseLiveResponses` accepted；C5b 扩三引擎／PG／定向 race，聊天文本不冒充答案。 |
| C4 #103 | Studio 方向生成／共享卡片，旧测验／CLI 兼容。 | 真实模型三题、首题自动 accepted；冻结方向／来源、幂等／续评／清除；C5b `notebook-studio`／`review`。 |
| C5a/b #106/#108 | 手机／可访问性／门禁及候选收尾已实施。 | [C5b](../design/notebook-workspace.md#c5b-候选验收记录2026-10-1415)：223 浏览器、374 PG、399 定向 race 最终通过；父 spec 24 通过、0 未通过、4 外部未运行、2 真机未运行，原件不完整。 |
| #100/#104/#105/#107 | 公式／标题、弹窗／Markdown、独立评估预算、GFM 已合并。 | 各批局部检查及 C5b 对应输入；#105 真实模型三题生成、两题连续自动评估，旧冻结预算题组可能仍需续评。 |
| #109/#110/#112/#113 | 对话／来源体验、停止幂等、PDF 提示／集合名、同名修订审阅已合并。 | [#37 E6](issue-37-acceptance.md#证据口径与索引)逐 PR 局部单测／构建／Chromium／Go；不能称 223 项已对最终 SHA 全量重跑。 |
| 后续演示站走查 | 任务提供的观察覆盖讲解／出题／评估自动采纳（930/900）、指南／简报。 | 运行可用性补证，缺日期／SHA／原始记录，见 #37 E7；不替代三类教学质量或最终证据签收。 |

架构仍为 Go 模块化服务、PG 权威状态、同源 React Web、原生 CLI 和 agentcore；新方向组合既有 owner，不另建聊天评分表，不删除旧目标／课堂／Evidence。

## 剩余项、关闭顺序与范围

实现与技术回归已收尾，全部发布验收仍需以下交接：

- 真机：中文 IME、iOS Safari／Android Chrome 软键盘、工具栏、缩放／横竖屏、安全区、长内容及真实焦点。
- 人工：VoiceOver／NVDA 播报、MathML／引用焦点；三类教学任务、来源／教材页码、Studio 内容、评分／待复核与不确定性；交互性能采样。
- 证据签收：C5b 缺 44 份原始 browser JSON／前置明细、5 个必需 PG 名称仅历史 guard，需核定复用或补原件／最小检查。原 candidate 退出 2，补验汇总不改写原退出码；补演示站日期／版本／操作，确认 #109–#113 后实际发布 SHA 的复用范围。

真实 NoteSync 上游、macOS 原生、真机离线等可选环境继续单列，按实际启用或支持声明验收；真实 Nocturne 受管备份已有 #67，外部 pg_dump/WAL/宿主快照按 #86 运维边界。不能因这些边界重开已关闭的实现任务，也不能称未执行检查通过。

建议先建立[#37 的人工验收单草案](issue-37-acceptance.md#关闭方式与后续人工验收单草案)并回链，再以范围收敛关闭 #37，最后关闭导航用途的 #40。若坚持原正文“全部发布门禁完成”才关闭，两单目前均应保留 OPEN。关闭是跟踪范围交接，不构成生产发布或外部资源变更授权。

[#111](https://github.com/ErzerLP/edu-agent/issues/111)（OPEN）承接整本书 PDF／结构化大文档／精准检索后续 Shape；当前仍有单 PDF 4 MiB／100 页／抽取 16,000 字节等限制，不支持 OCR。其待设计内容不是本轮交付承诺，也不成为旧 #40 关闭前置。

## 历史核查：companion 部署修复（2026-09-19）

保留原 #40 专项结果。基线 `ada8e87`（`task/61`），`TestWebReleaseProxyCoversClientAPI//v1/companion/browser` 和新增 `TestWorkspaceProxyAllowlist` 先失败，确认 Nginx 漏放三个正式路径。修复提交 `4913be9` 仅开放 `/v1/companion/(browser|attach|channel)`，保留身份／CSRF／MAC／开关。

| 原批次检查 | 已执行结果／限制 |
| --- | --- |
| release／proxy／start 合同、Go API/HTTP、vet | 通过；普通 Go 顶层 134 通过、13 数据库 skip，skip 不计 PG。 |
| CLI `TestStudy*`／`TestWorkbenchStudy*`、结果门禁 | 5 个 CLI、2 个门禁用例通过，覆盖具体审批／原课堂与运行复用。 |
| 本机 Nginx 1.24.0 配置／真实 HTTP | 三个 POST 到测试上游，body、Host、Origin、CSRF/MAC、no-store 保留；7 个非法 companion 及 admin/internal/mcp 路径为 404、不抵达上游。 |
| 原批次边界 | loopback HTTP fixture，非 TLS／真实本机命令／macOS；当时未跑 Web/PG，后续证据按前文分账，不倒改历史结果。 |

复现入口：`cd server && go test ./api ./internal/transport/httpapi -run '^(TestStartLearningContract|TestWebRelease.*|TestWorkspaceProxyAllowlist|TestWebProduction.*)$' -count=1`。本次整理未重跑该命令，也未改代理或测试代码。
