# 网页搜索与资料搜集：Shape 拆分与交付顺序

状态：Shape 文档定稿（2026-10-20），未实施。调查基线为 `docs/shape-search`、`e2a764a`，最新 migration 为 `000050_remove_mentor_budget_hash_version.sql`。已读取 [#165](https://github.com/ErzerLP/edu-agent/issues/165)、[#166](https://github.com/ErzerLP/edu-agent/issues/166) 正文与确认评论；本次任务的全局约束优先于历史设计中的预算、兼容和自动采纳条款。

## 1. 拆分选择

按[开发工作流](../development/development-workflow.md)建立两个 child，本文件作为 Supervisor 协调设计，不额外复制验收或创建 Runtime 状态。

| 顺序 | Child | 独立用户结果 | 文档 |
| --- | --- | --- | --- |
| M | `mentor-web-search`，#165 | 导师自动搜索，引用网页，用户手动加入来源 | [brief](../comet/specs/mentor-web-search/brief.md)、[spec](../comet/specs/mentor-web-search/spec.md)、[设计](mentor-web-search.md) |
| R | `source-research`，#166 | 按需求搜集候选与报告，勾选导入并继续补充 | [brief](../comet/specs/source-research/brief.md)、[spec](../comet/specs/source-research/spec.md)、[设计](source-research.md) |

```mermaid
flowchart LR
    M[M-A 导师搜索、引用与加入来源] --> RA[R-A 首次搜集、双结果与导入]
    RA --> RB[R-B 继续原轮与再找一些]
```

评估过三个方案：合成一个 change 会混合两个可独立演示的工作流；另建共享基础 child 会先交付没有用户入口的抓取／存储层。选择共享基础随 M-A 垂直交付，再由 R 复用。两个 child 分别 22、28 条正式验收；共享 settings、OpenAPI、migration、knowledge 写入及隐私 owner，按依赖串行，不并行修改公共模型。

## 2. 调查结论与 #22 边界

- `server/internal/research/fetch.go` 的 `Fetcher.Fetch` **已经**处理 `application/pdf` 并调用 `pdfsource.Parse`；用户上下文中“不支持 PDF”是较早状态。本次复用当前实现，沿用网页 1 MiB 传输／2 MiB 解码限制，不把上传可支持 4 MiB 误写成网络抓取上限。
- `server/internal/research/discovery.go` 的 `Discover` 仍是一请求、无正文、最多四候选的发现路径。R-A 直接替换 `POST /v1/knowledge/source-discovery` 和网页搜索入口，移除相关 capability 预算字段；不做别名或旧发现结果转换。`reference_sources.go` 对 `DiscoveryRequest.Validate` 的借用应改为独立的现有 `Policy` 校验职责，不能因删入口损坏普通 URL 导入。
- #22 的 `mentorrun.runResearch` 被目标研究与开始学习使用；`clients/cli-go/internal/command/goal.go`、`internal/api/study.go` 也有真实调用方。保留该独立产品流程，新增 `source_research` 编排；这是功能并存，不是给新搜集保留旧接口别名。复用网络、PDF、引用校验和受限知识写入，不把四候选、目标绑定或 `AutoAdopt` 带入新流程。
- `prepareSelected`／`selectedTool` 当前只允许来源内问答。M-A 同时更新这条路径及普通导师路径，但练习反馈、评估和 Studio 不获得新的网络事实来源；不能直接全局放宽所有引用校验。
- 现有 `MaxBody=256 KiB`，PDF 密文上限与 `research.MaxSources=4` 耦合。新任务使用独立、分页、加密的网页资料记录；不把 30 个网页和 PDF 全塞进一个运行 checkpoint。

## 3. 已冻结的共享合同

| 合同 | M-A 提供 | R 使用方式 |
| --- | --- | --- |
| 网络执行 | `search_web`、`read_web`；服务端登记资源、逐次外发门禁、安全抓取、PDF | 迭代规划下调用同一宿主，不新建通用 HTTP 工具 |
| 网络资料 | 同 owner 内的不可变正文快照、片段、覆盖缺口及 PDF 附件 | 每轮结果引用快照，跨轮只在同任务内复用 |
| 引用 | 网络与正式来源明确区分；只引用实际提供给模型的片段 | 报告按节引用，同样验证，不用搜索摘要作证据 |
| 导入 | 服务端预览回执、明确确认、当前区私有来源、幂等结果 | 批量逐项采用相同命令，报告另走派生资料保存 |
| 用量 | `modelusage` 内模型台账与搜索请求明细，同任务归集 | 任务各轮共用根归属，所有真实请求独立记录 |
| 生命周期 | learning 管临时网络内容，knowledge 管正式导入内容 | 同一代次、删除屏障和迟到结果规则覆盖报告及各轮 |

工具和 DTO 的规范定义在 [M 设计](mentor-web-search.md)；R 仅增加轮次、筛选和报告，不复制基础协议。此处的新增名称均为设计草案，不表示仓库中已实现。

## 4. 范围、默认值与数据所有权

- S1/S2、R1–R4 保持：导师自动搜索且可关闭；不自动加入；搜集不确认计划；候选默认全选，报告可单独导入；规模可设置；无累计预算；支持 PDF。
- 设计具体化为每轮最多 10 个新增外部候选、尝试读 30 个不同 URL，可额外保存 1 份报告。报告默认不选；继续同一轮不重置规模，“再找一些”新开一轮。数量口径在设置和结果页直接说明，不藏在实现中。
- “关闭联网”不意味着关闭发送给已选择模型的正常对话。新增搜集由用户点击开始独立授权；对话开关不替代搜集面板的停止操作。
- 网络检索模型上下文只接收本次输入和公开资料；服务端不向该上下文加载私人知识、成绩、记忆或完整聊天。页面不能增加工具权限，所有导入只来自用户界面操作。
- identity 管真实身份与撤权；settings 管连接、资源上限及搜集参数；learning 管运行、查询、网页暂存、报告、选择、导入操作关联和用量；knowledge 管正式来源、原 PDF、报告引文附件和索引。跨 owner 的输入依赖纳入读许可与提交门禁。
- 暂存沿现有运行的七天尺度，具体起算点见两个设计；到期不再提供正文和导入。正式来源依其自身保存周期，不因原运行到期而消失。全局隐私清除同时覆盖两类数据。
- CLI、OCR、动态浏览器及累计预算均非目标。不转换旧数据、不加接口别名；这里的 migration 草案仅指新数据库结构，不是旧数据兼容迁移。

## 5. Build 顺序与数据库草案

| 批次 | 完整结果 | Schema 草案（不预占编号） |
| --- | --- | --- |
| M-A | 自动搜索、可关闭、网络引用、PDF、手动加入及可查用量 | `000051_mentor_web_search.sql`：对话开关及版本、网页资源／正文／附件、导入回执、搜索调用台账 |
| R-A | 首轮搜集、真实进度、停止、候选和报告、确认导入及设置 | `000052_source_research.sql`：搜集任务／轮次／候选／报告／选择，扩展运行关联与清除登记 |
| R-B | 继续原轮、补充轮、跨轮去重与报告版本 | 沿 R-A 已定义的轮次合同；只在有实际 schema 缺口时追加后续编号 |

每批严格采用“结果／范围／延期／退出标准／重型证据”模板，详见各 child 设计。Build 合入前重新核对编号，新增 SQL；本次不修改 SQL、OpenAPI、代码、测试或已实施历史设计。不生成 `verification.md`。

## 6. Ready 与核对范围

两项已确认方向均无阻塞产品问题，M-A 可以先进入 Build；R-A 以 M-A 公共合同实现并通过对应验证为依赖。工程文件和验证位置已抽查；正式路径的示例均来自当前工作树，新文件／接口明确标为草案。

本次只核对文档相对链接、引用路径／符号、验收覆盖与 `git diff --check`，不执行代码测试。设计中的 Build／Candidate 检查是后续退出要求，不是本次验证结果。不得把“文档定稿”解释为用户已审查全部实现细节或 Runtime 已接受验收。

需返回 Shape 的变化：把私人来源自动纳入搜索、自动导入、增加计划确认门槛、改变停止后继续外发的承诺、改变保留／引用证据范围、加入累计预算、移除 #22 产品流程或扩大至 CLI。
