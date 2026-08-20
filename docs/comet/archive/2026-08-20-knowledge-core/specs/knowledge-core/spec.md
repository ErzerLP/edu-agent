# Knowledge Core 完整规格

## 模块边界

`server/internal/knowledge` 拥有 canonical Markdown、catalog snapshot、document identity、node identity、node lineage、派生 knowledge artifact 和检索 trace 的业务语义。端口由 knowledge 应用层定义，PostgreSQL adapter、LLM selector adapter 和 HTTP transport 只实现这些端口；其他模块不得直接读取 knowledge 表。

knowledge 使用普通事务模型，不写 Learning Event Log。它不读取客户端文件系统；客户端或后续 CLI 把文件与目录转换成带规范化相对路径的 Markdown 文档批次。服务端只返回数据和导出内容，不代表用户落盘。

## Catalog 与不可变 revision

系统维护一个单用户 knowledge catalog。`KnowledgeRevision` 是 catalog 的完整线性快照，包含 UUID、从 1 开始单调递增的 `revision_no`、可空父 revision、SHA-256 manifest hash、来源、创建设备、创建时间，以及 canonicalizer、parser、indexer 和 identity policy 版本。head 由 catalog 单例行保存并在导入事务中更新。

每个 snapshot 通过 immutable mapping 引用按规范路径排序的 `DocumentRevision`。导入是基于父 snapshot 的 upsert：请求中的文档替换或增加对应 document identity，未提交文档继续引用父 snapshot，省略文档不表示删除。snapshot 内 canonical path 和 DocumentID 分别唯一且一一对应；一个已确认 DocumentID 以新路径出现时，提交事务移除该 snapshot 中的旧路径映射并写入新路径，这属于 move。批内重复 DocumentID 返回 `duplicate_document_identity`，新路径已由另一 DocumentID 占用返回 `path_occupied`。完全相同的排序 manifest 返回当前 revision 和 `unchanged=true`，不递增 revision number。

`DocumentID` 和 `NodeID` 是跨 revision 的稳定语义身份。`DocumentRevisionID` 由固定 UUIDv5 namespace、document ID、canonical hash 和 parser profile 得到；`NodeRevisionID` 由固定 UUIDv5 namespace、document revision ID、node ID 和 indexer version 得到。同一 canonical document revision 重建时 ID 不依赖数据库插入顺序或随机数。

PostgreSQL migration `000002_knowledge_core.sql` 创建 catalog、import operation、knowledge revision、document、document revision、snapshot document mapping、node、node revision、lineage、lineage member和 node artifact 表。哈希、revision number、operation ID、snapshot path、stable ID 和 artifact version 具有数据库唯一约束；普通 store API 不提供对历史 revision 的 UPDATE 或 DELETE。

canonical 正文与 metadata 分开建模，以便未来受审计 privacy redaction 清除正文而保留最小 tombstone、hash 与引用。当前 child 不实现 redaction；任何普通导入、导出或检索都不能修改历史 payload。

## 导入命令与并发

导入命令包含 UUID `operation_id`、必填但可为 null 的 `expected_parent_revision_id`、来源说明、一个到一千个文档、可选 `identity_review_basis_hash`，以及可选 document/node identity resolutions。每个文档包含 canonical 相对路径和 Markdown；调用方不能指定 knowledge revision ID、document revision ID 或 node revision ID。resolution 只能引用服务上一次为相同 review basis 返回的稳定 locator 和 expected parent 候选。

服务为规范化后的完整正式请求计算 request hash，其中包含 resolutions。只有成功提交或 `unchanged` 的 import 才持久化 operation：相同 operation ID 与相同 hash 返回第一次结果，相同 operation ID 与不同 hash 返回 `idempotency_conflict`。expected parent 与事务中锁定的 catalog head 不同返回 `revision_conflict`，响应给出当前 head，不能自动 rebase 或覆盖。

identity review 使用独立 `identity_review_basis_hash`：它只覆盖 expected parent、canonicalizer/identity policy 版本和规范化文档 path/content，不包含 operation ID、review hash 或任何 resolution。`identity_review_required` 响应回显 basis hash 和由它导出的 locator，不写 import operation、revision 或 head。调用方加入 resolutions 重试时必须使用新的 operation ID 并回显 basis hash；服务重算不一致时返回 `stale_identity_review`。

解析、UTF-8/大小检查、AST 构建、身份候选和 manifest 预计算在事务外执行。提交使用 `READ COMMITTED`，锁 catalog 单例行后再次检查 expected parent，并在一个事务中写入 operation、revision、document、node、lineage、snapshot mapping 与 head。任一错误或故障注入都回滚全部 canonical 写入。

请求 JSON 最大 16 MiB，单文档最大 4 MiB，文档数最大 1000，单批节点数最大 100000。路径转换为 NFC、使用 `/`、必须为非空相对路径；拒绝绝对路径、空段、`.`、`..`、反斜杠、NUL、控制字符以及 NFC + Unicode case-fold 后重复。限制错误返回 `413 payload_too_large` 或 `400 invalid_path`。

## Canonical Markdown envelope

canonicalizer profile `edu-markdown-v1` 要求合法 UTF-8、移除单个 UTF-8 BOM 并把 CRLF/CR 统一为 LF。它不 trim 正文，也不通过 Markdown renderer 重写内容。除换行规范化和插入或更新保留 envelope 外，用户 Markdown 字节不改变。

frontmatter 使用 `go.yaml.in/yaml/v3 v3.0.5` 解析为 mapping 并保留用户键。服务拥有三个顶层保留键：`edu-agent-format`、`edu-agent-document-id` 和 `edu-agent-root-node-id`。格式版本为 1，两个 ID 是 canonical identity；重复键、非 scalar 保留值、非法 UUID 或用户占用不兼容值使导入失败。无 frontmatter 时服务创建一个最小 YAML envelope。

导出视图在 frontmatter 增加 `edu-agent-source-revision-id`，该字段说明导出来源但不属于 canonical hash；再导入时先校验并剥离该 view metadata。导出不得叠加重复键或 marker。

每个 Markdown heading 正前方使用独占一行的 `<!-- edu-agent-node:v1 {"id":"UUID"} -->` 标记 NodeID。只有 AST 中与 heading 同级且中间只有空白的 HTML block 才是保留 marker；代码围栏、引用块、inline code 和普通文本中的相同字符串不被识别。重复 ID、一个 heading 多 marker、孤立 marker、非法 JSON/UUID 或当前 document 无权声明的 ID 返回 `invalid_identity_marker`。

parser profile 固定为 `github.com/yuin/goldmark v1.8.5`、CommonMark 0.31.2 和 GFM。Obsidian wiki link、callout 与其他未识别扩展按原始文本保留。parser/profile 升级必须使用新版本号，并以 corpus 对比证明不会静默改变旧 revision 的重建含义。

## 确定性树与 source range

每个文档有一个 synthetic root node，其 ID 来自 frontmatter。heading node 使用最近的较低 level heading 为父节点；level 跳跃不补虚拟节点。首个 heading 前的 preamble 属于 root，文档没有 heading 时只有 root。

每个 node revision 保存有序 sibling index、heading level、标题、祖先标题、`heading_range`、`local_body_range` 和 `section_range`。范围使用 canonical UTF-8 字节的半开区间 `[start,end)` 并附 1-based 起止行；Setext heading 范围覆盖完整物理行。section 截止于下一个 level 小于等于当前 node 的 heading，local body 排除子 section。

全量重建输入只包括 document ID、canonical Markdown、冻结 profile/version 和 identity envelope。重建结果包含完全相同的 document/node revision ID、父子关系、顺序、hash 与范围；数据库已有 summary、模型输出或检索轨迹不属于重建输入。

## 身份匹配与 lineage

身份解析必须先完成 document stage，再完成 node stage。document locator 是 basis hash 与规范化请求 path 的确定性组合；优先级依次为合法显式 DocumentID、expected parent 中双向唯一相同 semantic document hash、调用方 `preserve/new` resolution、新 DocumentID。同路径、标题和模糊相似度只生成审阅候选，不能自动继承。semantic document hash 从去除保留 frontmatter 键与 node marker、统一换行后的 Markdown 计算。未知显式 DocumentID 仅在 catalog 历史中从未出现且批内唯一时作为可移植新 identity 接受；历史存在但不属于 expected parent、批内重复或跨父 revision 引用均返回稳定冲突。

缺少显式 DocumentID 且未形成双向唯一 exact semantic hash 时，只要 expected parent 中存在同路径或其他候选，服务就返回 `identity_review_required`；完全没有候选时才生成新 DocumentID。响应中的 document locator、候选 DocumentID/document revision、reason code 与 evidence 均按 stable ID 排序。调用方重试可选择 `preserve` 某个 expected-parent 文档或 `new`；完成 document ownership 后，领域层才允许该文档中的 node marker 声明属于该 document 的 NodeID。

node 身份顺序固定为：有效显式 marker、双向唯一确定性匹配、调用方 resolution、新身份。markerless node 只有在旧 snapshot 的已归属 document 集合中存在唯一相同 semantic local-body hash，且反向也唯一时才自动保留 NodeID；路径、标题、祖先、邻居或模糊相似度只用于生成候选，不能单独自动继承。

`identity-policy-v1` 的 similarity 输入不包含标题、ancestor、marker 或 summary。它把 Goldmark local-body 的可见文本做 Unicode NFKC 与 case-fold，连续字母/数字为一个 token，每个 CJK rune 为 token，丢弃标点和空白；不少于 5 个 token 时比较唯一 5-token shingles，否则比较唯一 token，分数是集合 Jaccard intersection/union。相同 semantic hash 记 1；一空一非空记 0；两者都空只允许显式 marker 的一对一标题/移动保留。带 marker 的一对一编辑在分数至少 0.50 时自动保留，低于 0.50、任一非空正文少于 8 token 且 hash 改变、复制 marker、一个旧节点对应多个新位置或多个旧节点对应一个新位置时返回 `identity_review_required`。阈值和 token 规则随 policy version 冻结。

审阅响应包含 document path、preorder node locator、reason code、旧 node revision 候选、分数和确定性 evidence，不写任何 revision。调用方重试时可按 node locator 提交 `preserve`、`new`、`rewrite`、`split` 或 `merge` resolution，并提供非空理由；`preserve` 表示用户确认语义连续，其他 lineage 动作表示用户确认重大变化。`preserve` 保持 NodeID；`new` 生成新 NodeID；`rewrite`、`split` 和 `merge` 为目标生成新 NodeID 并记录 approved lineage group、source/target node revision、actor device、理由、policy version 和时间。resolution 只能引用 expected parent 中的候选以及本次 AST locator，领域层拒绝未知、重复或跨 revision 引用。

纯文件移动不改变 canonical document/node revision；snapshot mapping 只改变 path。标题改名、章节重排或普通编辑保留稳定 ID，但 canonical 变化时产生新的 document/node revision。lineage 获批只表达历史关系，不迁移、折扣或复制任何学习证据。

## 派生 artifact

`NodeArtifact` 是可丢弃、不可覆盖的版本化派生记录，至少包含 node revision ID、kind、producer/prompt/model version、input hash、content、created time 和 status。当前 child 不生成摘要；测试或后续模块可以提供已钉扎 summary snapshot。artifact 写入和替换永远不能改变 canonical hash、tree、NodeID、NodeRevisionID 或 lineage。

## 分层无向量检索

检索命令包含查询、可选 knowledge revision ID、query context schema version 和有界 limits。未指定 revision 时服务只读取一次 head 并立即冻结为本次 revision；后续所有候选、artifact 和 hit 必须属于该 snapshot，禁止混入较新的 head。

`retriever-v1` 的 tokenization 与 identity policy 相同，但查询和字段使用 token set。字段命中值是 `1_000_000 * |query_tokens ∩ field_tokens| / |query_tokens|` 的整数向下取整；空查询在校验阶段拒绝。document score 是 `4*path + 3*all_heading_titles + 1*first_2048_byte_body_excerpt`，node score 是 `4*title + 2*ancestor_titles + 1*direct_child_titles + 1*first_2048_byte_local_body`。所有字段先按 UTF-8 rune 边界截断；score 降序后按 stable revision ID 升序 tie-break。

服务取前 8 个 document 进入 document shortlist，即使全为零分也按 stable ID 补足。每个 parent 的 children 全部评分后，候选集截断到前 20；lexical fallback 选择前三个正分候选，全为零时选择第一个。候选无 children 时 action 为 `select`；有 children 且 local-body 命中为正时是 `select_expand`；有 children 且 local-body 命中为零时是 `expand`；没有合法候选时是 `stop`。

遍历按 document shortlist 顺序进行稳定 breadth-first queue。领域层逐项应用 decision：`expand`/`select_expand` 只把对应候选在当前 snapshot 的 children 入队，`select`/`select_expand` 按首次 trace index 把该候选加入 hit，并按 NodeRevisionID 去重；空 decision 终止当前 branch。深度 8、总候选 200 或 queue 为空时停止；最终 hit 按首次 trace index、深度、NodeRevisionID 排序并截断到 10。英文和数字通过 case-folded runs，CJK 额外加入相邻 rune bigram；实现不调用 PostgreSQL 英文全文分词，也不使用向量。

knowledge 应用层定义 `Selector` 端口。selector 每层接收固定 revision、query context、parent node revision、有序候选、可选已钉扎 summary 和预算；输出按 candidate order 排列的 decision 列表，每项包含 `node_revision_id` 和 `select`、`expand` 或 `select_expand` action。`stop` 只表示该层返回空 decision 并终止当前 branch。领域层逐项重新校验成员、父子关系、顺序、重复、revision 和总预算。

candidate set 使用 UTF-8 规范串计算 SHA-256：首行为 `candidate-set-v1`，随后依次是 frozen knowledge revision ID、parent node revision ID，再按候选顺序写 `ordinal|node_revision_id|score|title_sha256|summary_artifact_id-or--`，每项以 LF 结束。selector response 必须回显该 hash；不一致视为 stale response 并整层回退。

模型配置可用时，LLM adapter 复用现有 `llm.Client.Chat` 发送结构化 JSON；无模型、超时、上游错误、schema 错误、未知/跨 revision ID、超量选择或 stale artifact 时，整层结果丢弃并使用 `lexical-v1`，trace 标记 `degraded=true` 与稳定 reason code。坏的模型响应不能部分生效。

检索响应包含 frozen knowledge revision、retriever/selector/query-context 版本、summary snapshot、每层 parent、按序候选、按候选记录的 decision/action、candidate hash、reason code、degraded/truncated，以及最终 document ID、document revision ID、node ID、node revision ID、path、source ranges、canonical slice、slice SHA-256 和 provenance。默认最大深度 8、每层候选 20、最终 hit 10、总候选 200；超限按上述稳定顺序截断并显式标记。

固定验收 corpus 至少包含 `go/concurrency.md` 的 `Concurrency -> Goroutine, Channel`、`db/index.md` 的 `Database Index`，以及 `systems.md` 的 `Systems -> Queue Overview, Messaging -> Queue Types`；Channel 与 Queue Overview 没有 child，Messaging 有 Queue Types child。查询 `channel` 且无模型时 document 顺序以 `go/concurrency.md` 在前；在 Concurrency 层候选顺序为 Channel、Goroutine，只产生 `{Channel, select}`，首个 hit 为 Channel，并标记 `selector_not_configured`。查询 `queue` 的 mixed layer 中，Queue Overview 排在 Messaging 前并产生 `{Queue Overview, select}` 与 `{Messaging, expand}`，随后 Queue Types 产生 `select`；hit 顺序为 Queue Overview、Queue Types。模型 timeout、malformed JSON、unknown ID、cross-revision ID 和 over-budget 使用相同候选顺序、per-node action 与 hit，只分别改变 degraded reason；超预算额外设置 truncated。

## HTTP、授权与错误

HTTP/OpenAPI 增加 `GET /v1/knowledge/revisions/head`、`POST /v1/knowledge/imports`、`GET /v1/knowledge/revisions/{revisionID}/tree`、`GET /v1/knowledge/revisions/{revisionID}/export` 和 `POST /v1/knowledge/retrievals`。export 返回按 canonical path 排序的 document 数组及 source revision，不打包 zip；CLI 后续负责落盘。

导入要求 `knowledge:write`，head/tree/export/retrieval 要求 `knowledge:read`。新配对令牌默认带这两个 scope；migration 为当前单用户 first-party 未吊销设备令牌补齐 scope。HTTP 继续在进入 knowledge use case 前执行 bearer 认证、设备限流、scope 和审计，日志不得写 Markdown 正文、查询正文、marker、模型 prompt 或导出内容。

JSON 继续拒绝未知字段。解析与 marker 问题返回 `400` 或 `422` 稳定机器码；revision、idempotency 和 identity review 返回 `409`；不存在或不属于 frozen revision 返回 `404`；body 上限返回 `413`；内部错误只记录 request ID 和脱敏类别并返回通用 `500`。`identity_review_required` 的候选是业务响应，不泄露其他 revision 的正文。

## 装配与兼容

`app.Run` 在 migration 后组合 knowledge PostgreSQL store、canonicalizer/indexer、可选 LLM selector 和 service，再通过 HTTP `Options` 注入。模型未配置或不可用不会让知识导入、导出、tree 或 lexical retrieval 不可用，也不会改变服务 readiness 的既有 required/optional 语义。

OpenAPI 版本随 knowledge endpoints 更新，保留既有 health、pairing、device 和 model 契约。现有 token、rate limiter、request ID、recoverer 和 error envelope 语义不得回归。

## 验证

parser golden corpus 覆盖 UTF-8/BOM、CRLF、ATX、Setext、跳级与重复标题、无标题文档、已有 YAML、围栏/引用内伪 marker、HTML、GFM 表格与 task list、wiki link、callout、CJK 和末尾无换行，并逐字验证 range 对应 canonical slice。

identity fixture 覆盖文件移动、路径和标题改名、章节重排、普通编辑、重大替换、marker 删除/复制、重复模板、split 和 merge。断言可确定变更保留 stable ID，重大变化建立新 ID/lineage，歧义返回审阅且数据库 head 不变。

检索使用进程内 deterministic selector 和严格 fake LLM，断言每层 candidate hash、选中 ID、最终 hit、revision、range、degraded 和 truncated；不同摘要文本不得改变 tree。模型 timeout、畸形 JSON、未知 ID、跨 revision ID和超预算输出必须稳定回退，不比较自由文本理由。

PostgreSQL 集成测试在 `TEST_DATABASE_URL` 的隔离 schema 中运行 migration、单/多文档原子导入、旧 revision 导出、same-operation 幂等、same-operation 不同 hash 冲突、同 expected head 并发、故障回滚、path 唯一约束和增量 snapshot 与全量重建比较。环境缺少 PostgreSQL 时明确 skip；普通 Go 测试、vet、漏洞扫描、OpenAPI/YAML 解析和错误级诊断仍必须通过。
