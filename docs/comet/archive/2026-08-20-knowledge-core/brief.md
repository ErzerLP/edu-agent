# Outcome

交付教学 Agent 的知识核心：由 Go `knowledge` 模块和 PostgreSQL 权威保存任意领域的 canonical Markdown、不可变知识快照、稳定文档与节点身份、节点 lineage 和可追踪的层级检索结果。文件系统只作为客户端导入与导出视图，运行时不依赖向量数据库。

# Scope

本 child 增加知识数据库 schema、Markdown canonicalizer 与 AST 索引、导入/导出应用用例、稳定身份匹配、lineage 记录、分层无向量检索、受设备令牌保护的 HTTP/OpenAPI 契约，以及确定性和 PostgreSQL 集成测试。

导入由客户端提交相对路径和 Markdown 内容的批次。服务端在一个事务中把批次叠加到指定父快照，生成新的完整 catalog snapshot；省略的文档沿用，当前 child 不通过省略执行删除。导出返回指定 revision 的 Obsidian 可读 Markdown，不读取或写入客户端任意路径。

# Non-goals

本 child 不实现教学状态机、学习事件、掌握度、路线、CLI 文件遍历、离线同步、Nocturne、Fast Note Sync、MCP、PDF/网页解析、向量检索、知识自动维护或文档删除。它不生成 LLM 摘要，也不批准学习证据迁移；只为后续模块提供不可变 revision、可选派生摘要工件边界和可审计检索接口。

首版按 CommonMark 0.31.2 与 GFM 构建结构。Obsidian wiki link、callout 等未被 GFM 结构化解析的语法保持原始 Markdown，不承诺理解其专有语义。

# Acceptance examples

- Parent A4：单文件或多文件 Markdown 批次在 PostgreSQL 原子创建不可变 canonical snapshot；按旧 revision 导出仍得到 Obsidian 可读 Markdown，并携带可移植且唯一的文档与节点身份。
- Parent A5：同一 canonical document revision 反复全量构建得到相同节点、父子顺序、范围和 revision ID；显式身份或唯一确定性匹配可在移动/改名时保留身份，重大改写、拆分和合并使用新身份与 lineage，歧义不会自动提交。
- Parent A6：固定语料的分层检索返回预期文档和节点、逐层候选与选择轨迹、固定 knowledge/document/node revision、正文范围和降级标记；无模型和无向量数据库时仍能确定性工作。
- Parent A28：canonical Markdown、catalog revision、document revision、稳定 node ID、node revision 和 lineage 全部归 `knowledge` 模块及 PostgreSQL 所有；文件和 HTTP payload 不是第二权威源。
- Parent A32：PostgreSQL 保存 snapshot parent、单调 revision number、manifest hash、来源、创建设备、canonicalizer/parser/indexer 版本、canonical Markdown 和内容哈希；导出身份 envelope 不破坏 Obsidian 阅读。
- Parent A33：再导入先校验显式身份；没有 marker 时只自动接受唯一确定性匹配，路径、标题祖先或相似度只生成审阅候选，`identity_review_required` 不产生部分 revision。
- Parent A34：Markdown AST 决定标题树、正文范围、源引用和节点 revision；摘要、LLM 选择和检索理由是可替换派生数据，改变它们不会改变 canonical hash 或任何身份。
- Parent A35：纯路径移动、标题改名和普通一对一编辑保留稳定 ID 并创建需要的新 document/node revision；重大改写、拆分或合并生成新 ID 和可查询 lineage，旧 revision 永远可按原引用读取。
- Parent A36：一次检索冻结一个 knowledge revision，先确定性缩小文档候选，再逐层选择节点并读取 canonical slice；结果包含 context/retriever/selector 版本、每层轨迹、revision 引用、范围、slice hash、截断和降级信息。
- Parent A37：验收使用固定 Markdown corpus、固定 selector 和 fake model；只比较候选/选中 ID、结构、范围、revision 与状态，不把真实模型文案或理由文本相同作为通过条件。

# Constraints and invariants

PostgreSQL 是唯一权威源。catalog revision、document revision 和 node revision 对普通业务不可原地修改或删除；未来隐私清除只能通过单独受审计的 redaction 边界处理正文，不能重写历史身份和哈希含义。

导入必须带 `operation_id` 和显式可空的 `expected_parent_revision_id`。只有服务端生成 revision 与身份。相同 operation 和相同请求返回原结果；相同 operation 和不同请求返回冲突；竞争同一父 revision 的不同导入最多一个提交成功。

canonical 解析、身份物化和树构建是确定性代码。LLM 不参与 hash、ID、range、lineage 批准或事务提交。模型 selector 的输出必须由领域层再次校验；失败时整层回退且留下可观察 trace。

# Decisions

知识库使用单用户 catalog 的线性完整快照。导入批次是原子 upsert：未提交的路径沿用父快照，不因目录缺项删除；完全相同 manifest 返回 `unchanged` 而不创建空 revision。每个 snapshot 中 canonical path 与 DocumentID 都是一对一；已确认的 DocumentID 在新路径出现时原子替换旧路径映射，这属于 move 而非删除。一个批次重复声明同一 DocumentID 返回 `duplicate_document_identity`，目标路径已由另一 DocumentID 占用返回 `path_occupied`。回退或显式删除属于后续知识维护能力。

canonicalizer 采用 UTF-8、LF 和版本化 identity envelope。YAML frontmatter 保存格式、document ID 与 synthetic root node ID；每个标题前使用严格、独占一行的 HTML comment 保存 node ID。导出视图补充 source revision；再导入时该 view metadata 不参与 canonical hash。

Markdown parser 固定为 `github.com/yuin/goldmark v1.8.5` 的 CommonMark 0.31.2 + GFM profile，frontmatter 使用 `go.yaml.in/yaml/v3 v3.0.5` 结构化校验。服务不通过 renderer 重写正文；除 BOM/换行规范化与保留 envelope 外，用户 Markdown 字节保持不变。

每个文档有 synthetic root。标题按最近的较低级标题形成父子关系，跳级不补虚拟标题；首标题前正文属于 root。所有范围是 canonical UTF-8 字节的半开区间并附一基行号。

身份先按文档、再按节点解析。文档优先使用合法 frontmatter DocumentID，其次使用双向唯一相同 semantic document hash；同路径、标题和模糊相似度只能形成审阅候选。节点显式 marker 是首要证据；markerless 内容只在 semantic local-body hash 双向唯一一致时自动继承。其他候选返回稳定 document/node locator、候选与 reason code，由调用方用新的 operation ID 和 review basis hash 显式提交 document `preserve/new` 或 node `preserve/new/rewrite/split/merge` resolution。review 不写 revision 或已提交 operation；直接用户 resolution 是 lineage 的审批来源，但不迁移学习证据。

HTTP 增加 `knowledge:read` 与 `knowledge:write` scope，以及导入、revision tree、revision export 和 retrieval 端点。单次导入最多 1000 个文档、16 MiB JSON、单文档 4 MiB；相对路径统一为 `/`，拒绝绝对路径、`.`、`..`、NUL、控制字符以及 Unicode NFC + case-fold 后冲突。

`lexical-v1` 使用冻结的 Unicode token、整数权重、document shortlist 8、每层 fallback 3、深度 8、hit 10 和总候选 200；遍历、tie-break、candidate hash 与 action 规则由完整规格固定。无模型与任何无效模型输出都产生同一确定性候选和 hit，只改变稳定 degraded reason。

# Open questions

无。

# Verification expectations

领域和 parser 测试覆盖 CRLF、BOM、ATX/Setext、跳级/重复标题、无标题文档、YAML、围栏内伪 marker、HTML、GFM、CJK、尾部无换行和范围切片。固定 identity fixture 覆盖改名、移动、重排、轻微编辑、重大替换、重复 marker、拆分和合并。

PostgreSQL 集成测试使用 `TEST_DATABASE_URL` 的隔离 schema，验证 migration、不可变旧 revision、批次回滚、幂等、expected-parent 并发、旧 revision 导出和增量 snapshot 与全量重建一致。未提供真实 PostgreSQL 时必须明确 skip，不能用 SQLite 或 mock 冒充。

HTTP/OpenAPI 测试覆盖 scope、严格 JSON、body/document 上限、`revision_conflict`、`idempotency_conflict`、`identity_review_required`、解析错误、旧 revision 查询和错误脱敏。检索测试覆盖模型成功、无模型、超时、schema 错误、未知 ID、跨 revision ID、超预算选择和 deterministic fallback。
