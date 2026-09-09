# 资料集合、显式引用与冻结范围（Issue #3）

## 现状确认

核查基线为 `68d507d`（已包含 Issue #2）。
`server/internal/transport/httpapi/learning_space.go:168` 拒绝所有非默认区业务；
`server/internal/knowledge/postgresstore/store.go:75` 从单例 catalog 读取 head；
`server/internal/knowledge/retrieval.go:83` 对完整 revision 的文档评分后取前八篇；
`server/internal/knowledge/service.go:424` 的身份候选只有全局父版本，没有来源集合。
因此该请求确实尚未实现，不能用客户端过滤修复。

## 开发方案与公共契约

一个垂直批次交付资料创建、导入、引用、浏览、冻结和检索。集合是来源和相对路径的命名空间，拥有独立线性版本；学习区引用集合，不复制正文。旧 catalog 和所有旧版本归属固定默认集合，默认区继续引用它，保留全部旧身份和历史引用。

所有知识入口复用 `X-Learning-Space-ID`。显式集合选择用 `X-Knowledge-Collection-ID`；省略时旧导入固定默认集合，不根据当前界面猜测来源。集合 ID 必须属于当前区引用；创建集合自动关联创建区。共享必须由用户明确开启，其他区才可关联；解除引用只删除关系。删除集合与隐私清除分别处理，不能借解除引用删除正文。

集合保留创建区归属，独立于当前引用关系；创建区解除引用后仍能重新关联私有集合，避免把“解除引用”变成无法找回的删除。`list --shared` 的发现结果也包含本区创建但尚未关联的集合；发现元数据不授予正文读取，必须先关联。

冻结范围独立于 knowledge revision：归属一个学习区，包含多个集合的确定版本以及文档/章节选择。创建时服务端核对集合引用、版本、document/node 所属关系；使用时只读取冻结条目。解除引用后历史范围仍有效，隐私清除后失效。正文以 document revision/node revision 去重。浏览显示最新集合版本与冻结版本的差异，禁止隐式更新冻结条目。

检索先解析并校验范围，再生成候选、摘要输入和排序。树、预览和导出复用同一范围解析器。无范围、无权限、已清除、上游失败必须区分，不得回退全库。旧维护与 NoteSync 仅允许其已明确映射的默认集合，新增来源没有安全映射时拒绝使用。

## 验收标准

1. 真实 PostgreSQL 中两个区、两个集合、同名 README 共存，列表/树/预览/导出/检索不串区。
2. 显式共享、关联、解除关联可重试；解除 A 的引用不改变 B 和历史范围。
3. 多集合冻结范围准确保存版本，更新后旧正文仍可读取并显示有更新。
4. 伪造 collection/document/node/revision/snapshot 和跨区请求失败，候选截断之前生效。
5. 旧数据迁移保留 ID、版本、引用；身份审阅、哈希和词法回退不变。
6. 并发导入、关联与冻结使用事务校验；隐私清除覆盖新增数据并阻止旧范围恢复正文。
7. HTTP、MCP、管理页、CLI/TUI 使用一致范围；NoteSync 默认映射兼容且不推断活动区。
8. 更新 OpenAPI、帮助、使用说明；通过受影响测试、真实数据库场景及最终 build/vet。

## 使用链路

```sh
edu-agent space create --name 'Go 后端'
edu-agent --space 区ID knowledge library create --name 'Go 仓库' --source 'repository:go'
edu-agent --space 区ID knowledge import --collection 集合ID ./go-notes
edu-agent --space 区ID knowledge library list
edu-agent --space 区ID knowledge library browse
edu-agent --space 区ID knowledge library share --id 集合ID --version 1
edu-agent --space 另一区ID knowledge library list --shared
edu-agent --space 另一区ID knowledge library link --id 集合ID
edu-agent --space 另一区ID knowledge library unlink --id 集合ID
```

一个来源使用一个明确集合；两个来源即使相对路径均为 `README.md` 也互不占用。重新导入传相同集合 ID，继续沿用身份 marker、父版本 CAS 和身份审阅；未指定集合的旧导入始终使用默认资料库。首次创建可自行传 `--id UUID` 供网络重试；共享修改使用集合元数据版本。取消共享只阻止后续关联，不撤销既有引用。

`library tree --collection 集合ID --id 版本ID` 返回稳定文档/节点 ID；`preview` 返回正文。`browse` 可查看正文和树，选择文档或章节冻结范围、开启共享或解除本区引用。管理页输入区 ID 和集合 ID 后使用同一服务读取；搜索仅搜索已授权加载的集合。

```sh
edu-agent --space 区ID knowledge library freeze --entries '[{"collection_id":"集合A","revision_id":"版本A"},{"collection_id":"集合B","revision_id":"版本B","document_id":"文档ID","node_id":"章节ID"}]'
edu-agent --space 区ID knowledge library scope --id 范围ID
edu-agent --space 区ID knowledge library preview --scope --id 范围ID
edu-agent --space 区ID knowledge library retrieve --id 范围ID channel
```

省略 document_id 表示整个集合版本，省略 node_id 表示整篇文档；章节包含子树。`scope` 响应的 `entries` 是冻结版本，`updates` 是可更新集合的最新版本提示，不改变原范围。需要更新时重新冻结并显式使用新范围 ID。导出保留相对路径，同时用 collection_id 和 knowledge_revision_id 区分同名来源；章节导出是原文切片，不能当作整篇身份保留导入。

HTTP 集合管理为 `/v1/knowledge/collections`，范围创建/详情为 `/v1/knowledge/scopes` 及 `/{scopeID}`，冻结树/导出为 `/{scopeID}/tree|export`。检索新增 `scope_snapshot_id`，与 `knowledge_revision_id` 互斥。范围 ID 是独立资料关系对象；检索响应的 `knowledge_revision_id` 在范围检索中仅作为兼容的遍历标识，实际版本必须查看范围 entries 和命中的 document_revision_id，不能把它当作单一 catalog revision。后续目标/路线 owner 可保存该范围 ID；本项不改目标生命周期或复制学习证据。

冻结范围自带全部集合选择，不能再混用集合 header。集合管理和范围管理也不接受集合 header。`collection_id`、`scope_snapshot_id` 查询参数不是范围入口，会明确拒绝，不得静默忽略后读取默认资料。

MCP 的 `knowledge.retrieve` 接受 learning_space_id、collection_id 和 scope_snapshot_id，并调用相同服务校验。原资源 URI 仍固定默认范围；显式 HTTP 空间/集合 header 在 MCP 上明确拒绝，不能隐式切换其他工具的范围。没有新增共享、关联、导入或审批等 MCP 高权限写入口。

旧知识维护和当前单 vault NoteSync 保持默认区/默认集合映射。NoteSync 状态显式返回两个 ID，未映射的非默认来源不可用；新集合导入不会自动发布到旧 vault。既有远端路径冲突审阅不变，也不改变上游并发写入限制。非默认集合更新使用显式导入；资料移除与回滚不借用解除引用实现。

解除引用只移除关系，冻结范围仍属于创建它的学习区；隐私清除会删除新增引用，清空范围条目并标记失效，同时擦除集合名称/来源及全部原正文。仅恢复固定默认兼容引用，以允许清除后的新导入。

## 验证记录（2026-09-08）

修改前运行 `go test ./internal/knowledge -run '^TestIssue3UnrelatedLearningSpaceCannotReadCatalog$' -count=1`，未关联区检索返回一条默认区命中，导出返回一篇文档，两个子测试均失败。HTTP 当时拒绝全部非默认区，所以这里证明的是知识服务缺少范围能力，而不是现有 HTTP 已允许跨区访问。

已通过 server 与 cli-go 的全量 `go test ./...`、`go vet ./...` 和生产二进制构建；后续局部修复重跑对应 package 测试与 vet。管理脚本通过 `node --check`，工作树通过 `git diff --check`。

真实 PostgreSQL 17 使用独立测试容器、隔离 schema，固定镜像 `postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`。串行完整通过 migrations、knowledge/postgresstore、transport/httpapi；新增场景及隐私清除场景又通过定向 `-race`。

关键证据：

- `TestPostgreSQLKnowledgeSpacesIsolationAndFrozenVersions`：同名资料共存、跨区拒绝、共享、冻结、更新提示、解除引用、章节正文与祖先标题隔离、并发导入 CAS、跨集合版本拒绝。
- `TestKnowledgeSpaceUpgradePreservesLegacyRevision`：从迁移 12 升级，旧版本原字段及 head 不变，重复运行迁移无副作用。
- `TestKnowledgeRedactedRevisionTombstoneAllowsFreshImport`：真实隐私 barrier/scrub 后旧范围无法读取或导出，清除后新导入仍可用。
- `TestPostgreSQLKnowledgeSpacesHTTPContract`：真实服务层与数据库完成集合创建/导入/共享/关联/冻结/检索；跨区、混用范围和错误查询参数被拒绝。
- `TestMCPKnowledgeScopeIsForwardedAndNeverFallsBack`：范围参数进入同一服务；失败不重试全库，非法区 ID 不调用服务。该测试也发现并修复了非法 ID 导致错误路径丢失原 context 的问题。
- `TestFrozenKnowledgeScopeSurvivesStrictClientDecoder`：生产 CLI 验收发现新范围字段被严格解码器拒绝，补齐 DTO 后用独立响应夹具保留回归。
- `TestPostgreSQLNoteSyncCannotReadDetachedCollection`：先复现解除默认区引用后旧 NoteSync 预览仍可读正文，再将同事务引用校验加入预览、审阅和回执边界，阻止未映射资料通过旧入口读取。

生产二进制实际配对并创建“Go 后端”“英语”，分别导入 README.md；CLI 验证共享、关联、解除引用和原区引用保留。重启服务后，交互入口按序号选择文档及 Channel 章节，成功冻结并通过 API 导出原文；范围检索成功，另一区读取同一范围返回 not_found。临时客户端配置与数据库不影响用户现有服务。

未运行真实 NoteSync 远端、多平台原生 TUI、管理页浏览器交互和外部教学模型矩阵；旧同步协议未修改，模型关闭时沿用词法检索。本批不扩展目标生命周期、MCP 高权限写入或非默认区教学能力。
