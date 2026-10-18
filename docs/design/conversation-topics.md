# 对话话题索引与上下文压缩：设计

状态：Shape 已确认（2026-10-18），T-A 已实现并通过本批检查，T-B 实现覆盖见 §10。对应 [brief](../comet/specs/conversation-topics/brief.md)、[spec](../comet/specs/conversation-topics/spec.md)，前置为 [U](model-usage.md)。T1 已确认采用建议默认值。

## 1. 现状与选择

`server/internal/mentorrun/conversation.go` 的 `prepareConversation` 会读取所有旧 turn、解密、拼接同来源／同方向的 `Messages`，并受 `MaxBody` 限制；`core.go` 的 `Prepare`、`source_selection.go` 的 `prepareSelected` 又按模型窗口返回 `ErrLimit`。必须同时改变历史装载和请求组装，不能只截断最后一层消息。

采用持久话题与消息锚点、增量整理水位、请求时有界投影三部分。固定轮次切段已被用户否定；每次重做全对话摘要开销随历史增长，也不采用。CLI [上下文压缩设计](agent-context-compaction.md)中的水位、原子消息组和派生数据失效思路可以借鉴，服务端实现仍位于 mentorrun，使用数据库租约与隐私 owner，不复用 CLI 内存账本或更改 agentcore 协议。

## 2. 数据与稳定消息锚点

以下表和字段为 migration 草案，拟文件 `000042_conversation_topics.sql`，实际编号在 Build 分配。

| 表／数据 | 内容与约束 |
| --- | --- |
| `learning_conversation_messages` | `conversation_id, run_id, message_slot, message_id, ordinal, privacy_generation`；只映射可展示消息，不复制正文；`(run_id,message_slot)` 唯一，ID 稳定，ordinal 不复用。 |
| `learning_conversation_topics` | `id, conversation_id, privacy_generation, revision, updated_at, ciphertext`；密文含标题、要点、状态观察和范围标签，不存为明文全文索引。 |
| `learning_topic_anchors` | `topic_id,message_id` 多对多唯一；锚点必须属于相同对话和代次，按 ordinal 分页；原文仍由已有 turn 读取。 |
| `learning_topic_index_heads` | conversation 与 generation 唯一；`revision, covered_through, status, lease_id, lease_until, last_failure`；失败只保存固定分类，不保存模型原文。 |
| `learning_topic_index_scopes` | `(conversation_id,privacy_generation,scope_fingerprint)` 唯一；保存该范围的扫描位置、成功覆盖水位和未处理区间。不同来源／方向范围不能共用一个成功水位。 |
| 增量 job | `from_exclusive,to_inclusive,input_revision,scope_fingerprint,configuration,operation_id`；可放入 mentorrun 的新任务类型，结果附在一次 CAS 提交内。 |

话题密文中的每条要点／状态观察必须带 `anchor_ids`、来源 manifest 指纹、方向修订、引用的练习／记录 ID 及各依赖 generation。话题身份可跨多处复用，但不同范围的事实分项保存；不能先把 A/B 来源混成一段文字，再只给它打一个“可读”标签。

状态建议为观察列表：`understood_in_chat`、`question_open`、`exercise_error_observed`、`unassessed`，附时间和证据锚点。后续理解改善可更新当前展示，过去错误仍能定位；明确区分用户自述、导师说明和实际评估记录。传给模型的练习项目包含“以正式记录为准”，UI 对应“练习结果以学习记录为准”。无记录支持时不生成已通过状态。

消息锚点映射由 `saveTurn` 的展示投影生成；当前流式片段未提交前不进入整理水位。`message_slot` 绑定已提交 turn 中的稳定消息位置，索引版本变化不能让旧 ID 指向别的文字。为旧 turn 延迟建立映射，无需批量重写密文。工具结果不直接成为 UI 消息锚点，引用它时挂到所属可展示消息，同时保留内部原子组关联。

已保存对话的索引随对话保存；临时对话只在原进程／原有效期内保留索引和有限原文，不新增永久正文或标题。旧无 conversation 的 mentor run 也用同一上下文组装策略的临时投影，但不伪造一个可重开的对话。

## 3. 增量整理与并发

建议内部默认：每新增 4 个完成轮次，或未整理输入估算达到窗口的 15%，排队一次整理；这只是调度分组，不是用户可见切段。阈值可在 Build 校准，不成为产品设置。

1. 取得当前已提交消息最高 ordinal 和最后成功水位；以有限页抓取 `(covered_through, target]`，同一对话只允许一个整理租约。
2. 按来源／方向范围分组，读取相关已有话题的**该范围投影**及新增消息。先按标题和锚点做确定性候选筛选，不把无限话题列表一次塞给模型，也不调用全集合／向量检索。
3. 模型返回新增话题、已有话题更新和新增锚点；只允许引用输入候选 topic ID 和本批真实 message ID。新名称不等于授权，输出作为受限数据验证。
4. 同一事务中检查租约、generation、对话未删除、输入 head revision 未过期，更新话题、锚点和水位；失败不提交部分输出、不推进水位。
5. 新轮次继续追加，不等待后台任务。迟到任务只能更新自己的受理区间，CAS 失败后读取当前结果，不把旧回复覆盖新话题。

更新话题读取旧要点可以，重新扫描已覆盖原文不可以。旧话题非常多时分批匹配相关候选；没有安全匹配就新建话题，后续可凭新增消息的明确证据合并身份／别名，合并保留所有锚点。模型整理超窗时把尚未整理区间拆为有限批次；每次成功只推进本批水位，不以“只取前 N 条”谎报全部完成。

改选来源后，当前无外发权限的旧范围记为未处理，不借整理任务重发。各 scope 的水位独立推进；head 的 covered_through 只表示所有应处理消息均已成功覆盖的连续前缀，另提供已覆盖尾部和缺口。之后重新选回旧范围时只补其缺口，不重做其他范围已经完成的消息。scope 状态和锚点一并清除、轮换检查，不能把“跳过未授权内容”记作“已摘要”。

失败后不无限自动重试：本次标记失败，后续新增轮次或用户主动整理时重试原区间。台账保留失败尝试。后台失败与对话回答失败分离，不让后台摘要失败终止主任务。

## 4. 来源与方向隔离

现有 `sameSourceScope` 和 `sameDirectionScope` 的保守匹配语义保留。UI 可以在原历史权限内看各话题，但发给模型的每条要点、标题和状态都必须来自本次允许的范围；跨范围主题的公共 ID 不意味着混合摘要可外发。若无法安全拆出同范围内容，就省略并说明未纳入，不能用全局标题泄漏未选来源。

新摘要也必须遵守模型目的地确认，不能因为后台执行就跳过 `ConfirmDestination`。整理范围同时受原消息可读性、当前选择和依赖 owner gate 限制。更换来源后旧索引不删除，但旧范围不参与新请求；更换方向后不把旧方向理解状态当新方向掌握。

有全局 memory 或课堂来源的旧 mentor 路径，摘要保留依赖的 memory／knowledge 代次及记录 ID；来源撤回或 memory generation 改变后，对应项整体失效。新的来源模式不因此引入 memory、companion 或旧课堂资料。

## 5. 上下文组装与有界装载

### 5.1 窗口计算

内部建议值，均由 `context_tokens=C` 派生：预留配置输出 `O=output_tokens`；安全余量 `M=max(256,ceil(0.05*C))`；固定输入 `F` 包含 system、方向、工具和 schema。可变输入容量 `B=C-O-M-F`。输入估算加 O 达到 `0.75*C` 时启用压缩；准备后的总估算始终不超过 C。

压缩后的 B 初始分配：话题投影最多 20%，最近原文约 45%，相关来源约 35%；没有来源时剩余空间给原文，任何池空余可向其他池转移。按完整轮次／工具调用组从近到远装载，不按固定轮数硬保留。当前用户输入、当前进行中的工具组优先；必要时先缩短旧来源和旧工具的语义投影，再减少旧轮次。

完整索引可以很大，但模型投影不能无限增长：按当前问题匹配话题名／要点，优先相关疑问和最近状态，裁掉重复表述及低相关条目。必要时为本次请求生成有锚点的短投影，仍走台账；不删除 UI 索引、不回读旧原文再摘要、不宣称全部旧话题都在本次上下文中。

### 5.2 不能只改 Prepare

`prepareConversation` 改为取得有界历史引用和索引快照，再反向分页读取必要尾部；run checkpoint 只保存本轮新消息、所用投影版本、必要续跑状态。`turnBody`／`saveTurn` 的历史边界改用显式本轮起点或独立本轮消息，避免压缩后的数组下标把摘要写成原文或漏存新消息。原始 turn 表不压缩、不改写。

当前 turn 的模型工具往返也可能增长，必须使用有界旧工具投影和同样的上下文组装，不能等到 `checkpointJSONSize` 先失败。模型消息的投影与用户可读历史分别保存；存储配额仍是独立约束，不以“无限对话”为由取消 StorageMiB。

### 5.3 来源本身过大

`prepareSelected` 目前也发送全部来源片段。T 中为当前提问加入 manifest 限定的章节筛选和片段切片，否则仅压缩历史无法保证窗口。拟新增 knowledge owner 端口：读取确切 manifest 内的分页目录、指定 node revision／range 的片段；T/S 共用。**不得直接调用可能读取全集合的 `Retrieve` 代替范围校验。**

片段过长时按原段落／行区间切片，保留原 document/node revision、range 和切片 hash；引用核验针对实际发送的片段。目录分页也有界，先匹配目录再取正文，无需在一次模型请求中发送整棵树。用户问题超出实际选取片段时说明资料覆盖，不声称读完全部来源。这部分只消费已导入结构，不新增 PDF 导入或 OCR。

新 reader 同时返回可复核的子范围证明：原片段身份、规范化行／字符区间和切片 hash。发布时从确切源版本重新取该范围核对，不能要求子切片必须等于旧 `SourceFragments` 的整片文本，也不能只验证模型提供的 hash。`VerifyStudioSources` 等消费者保留旧整片校验路径，并为新范围格式增加明确 schema 分支；不会通过放宽等值检查接受任意摘录。

### 5.4 降级和硬限制

整理失败、超时或尚未追上时，当前压缩请求退到可放入的最近原文，提示“较早内容未纳入”；UI 仍可显示上次成功索引，但不把它当作本次完整上下文。已有来源筛选和权限校验继续有效。

正常长历史不能触发 `history_context_limit`。如果单条当前输入很大，先按有界片段整理为临时投影，计入台账并明确正文未完整纳入；如该整理也失败，保留可容纳的最近输入并标明未纳入范围，允许用户拆开发送，不能静默回答整个超大问题。固定 system／schema 加输出预留本身已超过 C 属于配置无法容纳的情况：给出可操作的配置错误，而非承诺无限缩减至损害协议。该例外不是旧历史增长导致的失败。

## 6. API 与 UI 草案

沿 `server/internal/transport/httpapi/tutor_history.go` 的 `/v1/learning` 前缀扩展；下列为拟新增 suffix，其中 `{id}` 对应既有 conversation ID：

| API | 内容 |
| --- | --- |
| `GET /conversations/{id}/topics` | cursor、limit；返回 index revision、covered_through、整理状态、话题摘要和锚点数量；不触发模型。 |
| `GET /conversations/{id}/topics/{topicID}/anchors` | 前／后 cursor，返回真实 message ID、ordinal 与加载目标；不一次返回无限锚点。 |
| `GET /conversations/{id}/messages/{messageID}` | 定位包含该消息的有界历史页；权限／来源失效时返回不可用，不回退别的消息。 |
| `POST /conversations/{id}/topic-index` | `operation_id,expected_index_revision,privacy_generation` 及必要目的地确认；主动整理到已提交快照水位，返回已有 run 回执。 |

话题内部读取端口供 S：`ReadTopicScope(conversation, topic_ids, index_revision, through_message, current_manifest, direction_revision)`，返回有界要点、学习关注点、覆盖缺口及锚点身份，**不返回原始 Messages**；引用仍只能由来源 reader 提供。topic IDs 必须属于该对话，选择与 revision 绑定，后续索引更新不扩充已受理范围。

入口显示话题列表，理解状态采用简短文字；分页、目标缺失和失败有实际状态。点击加载旧页后保持输入草稿，聚焦目标消息；上下处操作不抢回底部。index revision 与 conversation version 分离，后台整理不会导致每个正常发送都版本冲突。

## 7. 隐私、迁移与生命周期

原文／话题由 learning owner 清除；涉及来源的阅读和外发同时锁 knowledge gate／链接，涉及 memory 的旧路径同时锁 memory gate。拟新增表纳入 `redactLearningTypedPayloads`、`verifyLearningTypedPayloads`；新增密文纳入 `mentorrun.RotateKeyTx` 和 app 轮换组合。对话删除时同步清除 topic、anchor、head 和未完成整理 job；缓存以身份、origin、区、对话、generation 隔离。

worker 在读取、外发前、结果提交和响应输出均检查 gate。清除屏障取消在途调用，旧 generation 的摘要即使完成也不能提交。撤销来源链接后不从旧摘要恢复正文；响应与 UI 同样不能显示失效摘要。用量由 U 按其 owner 清除。

按已确认的 T1，升级或仅打开页面不自动外发旧对话内容，migration 不调用模型。用户继续该对话、主动“整理话题”或使用“整理本次对话”时，才在当前授权范围内补建必要锚点并从水位分批整理；界面显示整理进度和未覆盖范围。历史格式不可识别时保留现有 schema 错误，不能解析成空历史。现有客户端不需要新字段；新增 topics API 独立，原历史 DTO 仅可增加稳定 message 标识，不改变旧 Messages 意义。关闭新 UI 不删除已保存索引，也不回退到无界历史组装。

## 8. 垂直批次与退出标准

### T-A：可用的增量话题索引

- **结果**：新对话聊过几个话题后可从工具栏查看并跳到多处消息，旧对话可以主动补建。
- **范围**：锚点／话题／水位、整理 worker、用量接入、查询和 Web 列表／导航、清除及轮换一起交付。
- **延期**：超窗组装、受限来源切片和 Studio 整理入口；不能在此批宣称已解决长对话。
- **退出标准**：跨多处话题和一消息多话题的生产入口可运行；失败不推进水位、并发 CAS、分页定位、旧数据惰性补建、临时数据不落盘及清除迟到结果有相关验证。相关 package、migration 与页面检查通过，更新接口文档。
- **重型证据**：真实模型话题质量和多浏览器交互留给 T 候选；本批用确定响应验证索引语义。

### T-B：长对话继续且请求有界

- **结果**：历史超出窗口仍可交流，能利用相关早期话题；失败时明确降级并继续。
- **范围**：历史按需装载、Prepare 两路径、工具组投影、manifest 限定来源目录／切片、上下文提示，以及给 S 的话题投影端口。
- **延期**：Studio 产品入口由 S 实施，#111 导入不纳入。
- **退出标准**：构造超过窗口且超过原累计 checkpoint 体积的已保存对话，原文可完整分页、请求有界；零来源／换来源／换方向不泄漏旧摘要；大量话题、摘要失败和工具组不拆散有最小检查；相关 Go／数据库／Web 检查通过。端口与兼容约束更新。
- **重型证据**：T 候选用真实模型抽查早期要点召回、摘要漂移和推理输出余量，再检查完整 spec；不能以一次成功代表所有历史事实完整保留。

## 9. T-A 实现覆盖

T-A 实现位于 `mentorrun`，migration 为 `000044_conversation_topics.sql`。本节只记录增量话题索引；§5 的上下文装载、超窗组装及来源切片仍属于 T-B，不能据此宣称长对话问题已解决。

### 数据与提交

- `learning_conversation_messages` 只存消息身份、位置、范围指纹与覆盖标记；原文继续由 `learning_tutor_turns` 解密读取。稳定 ID 绑定 run 与展示 slot，用户消息沿用原数组位置，`-1` 专用于该轮展示回答；工具消息不单独成为可见位置。ordinal 使用固定的轮次／slot 顺序空间，因此旧轮次惰性补建不会改变后续 ID 或复用位置。
- `learning_conversation_topics` 只保存加密正文，包含标题、带消息证据的要点和理解观察。`learning_topic_anchors` 用同对话、同代次的复合外键约束多对多关系。候选仅在同来源／方向范围内匹配；不把跨范围文字合成一个可外发标题。
- `learning_topic_index_heads` 与 `learning_topic_index_scopes` 分别保存索引 revision、连续成功前缀、范围扫描与成功水位；每条消息的 `covered=false` 表示未处理区间。API 同时返回已覆盖尾部、已覆盖／映射消息数量及未建立映射的旧轮次数，尾部成功不代表中间已覆盖。
- 整理使用 `topic_index` 导师运行，复用持久租约、并发配额与运行回执；head 只关联一个活动任务。读取保持完整提交快照；提交在同事务中检查当前授权范围、隐私代次、有效租约、run 身份与输入 revision，再原子保存话题、锚点和水位。索引 revision 不改变对话发送 version。

### 触发、授权与失败

- 新的已保存回答提交时持久化整理触发标记。每新增四个完成轮次、未整理输入达到窗口的 15%，或失败后有新轮次时受理整理；普通对话优先取得执行槽位。服务重启只恢复这些新触发及已受理任务，不为升级前历史制造触发。
- 用户主动 `POST topic-index` 时冻结已提交轮次快照、来源 manifest、方向修订、配置和目的地授权。仅打开历史或话题列表不会调用模型或补写旧消息映射。后台以每页 16 轮补建缺失映射、每批最多 32 条消息整理，并按窗口估算缩小输入；只提交实际处理的批次。
- 新对话动作改变当前授权范围后，旧范围任务在续租、外发和提交门禁失效。话题候选最多读取 32 项、投影最多 8 项，按标题匹配及最近修订确定候选；无法安全匹配时新建话题。输出不拥有执行权限，只接受本批消息 ID、候选 topic ID 和限定状态；练习不生成“通过”或掌握度记录。
- 读取、外发、提交与响应沿用 learning／knowledge／memory owner 和来源链接门禁。旧格式缺失记忆依赖时，只从仍可读取的原 checkpoint 恢复依赖标识；无法核验来源的旧课堂工具内容或依赖已经丢失的旧记录不外发，保留未覆盖状态。不会从当前来源或当前记忆猜补旧授权。
- 模型失败保留上次成功结果和水位；不自动循环重试失败区间，用户主动整理或后续新轮次才重试。超大单条消息无法在当前窗口内整理时保留缺口；T-A 不添加 T-B 的单条临时压缩或上下文降级。
- 每次实际模型 HTTP 调用进入用量台账，`kind=topic_index`、`source=server`，origin 关联 conversation；用量页可返回仍有权限读取的原对话。无模型外发的 GET 不增加调用次数。

### API、Web 与生命周期

四个接口为 `GET topics`、`GET topics/{topicID}/anchors`、`GET messages/{messageID}` 与 `POST topic-index`。topics 使用有界游标分页；anchors 支持前后游标，位置端点只返回包含确切消息的一轮历史。历史 DTO 新增 `message_anchors`，原 `messages` 的语义保持不变。

Web 工具栏提供只读话题面板、整理状态与覆盖进度、主动整理及目的地确认。选择话题后聚焦真实消息，“上一处／下一处”按消息顺序导航；移动端可返回列表，方向键／Home／End／Esc 可操作列表。定位页与原对话共同渲染，输入组件与标签页草稿保持挂载；后台进度刷新不抢回底部。界面固定提示“练习结果以学习记录为准”。

话题、锚点、scope/head 均纳入 learning 清除和验证；对话删除同时删除派生数据并取消其运行。密文话题纳入导师密钥轮换和停机锁边界。临时对话不会创建消息映射、话题或整理任务，本批整理入口仅面向已保存对话。

验证范围：真实 PostgreSQL 的受影响包 `go test -p=1`／`go vet -p=1`，确定性模型响应覆盖多对多、失败水位、并发 CAS、分页定位、惰性补建、临时不落盘、隐私清除迟到响应及轮换；Web 类型检查与单个 Chromium 话题导航用例。真实模型话题质量、三浏览器交互及 T-B 长对话验收仍未包含在本批证据中。

2026-10-18 检查结果：`mentorrun`、`modelusage`、`learning/postgresstore`、`transport/httpapi`、`app`、`migrations` 的包测试与 vet 通过，使用指定真实测试库和 `-p=1`；`mentorrun` 使用 `-timeout=20m`，149 个顶层测试通过，其中新增话题测试 8 个全部通过。3 个既有真实模型用例和 1 个外部 CLI 用例因未显式启用而跳过，不属于本批验证范围。`npm run check`、前端构建、单个 Chromium 话题导航用例、文档相对链接和 `git diff --check` 通过。OpenAPI schema 已生成，生成器仍提示既有 `ActionExposureRequest` discriminator 警告。浏览器使用同一测试库内的独立 schema，完成后已清理。


## 10. T-B 实现覆盖

### 窗口与历史

- `mentorrun/context.go` 共用组装器覆盖 `Prepare`、`prepareSelected`、学习方向问答和 `max_tool_steps` 的文字收尾。使用 `C=context_tokens`、`O=output_tokens`、`M=max(256,ceil(0.05*C))`；固定输入计入 system、方向、工具定义和结构化 schema。输入加输出达到 75% 或尾部装载发现缺口时使用话题投影。话题池初始按 B 的 20% 分配（另设 8192 Token 投影上限），来源最多 35%，剩余约 45% 给本轮及最近原文；完整原文轮次装不满的空余可转给话题池，转移后仍受 8192 Token 上限及总窗口约束，话题／来源池空余返还原文。本轮问题先预留，来源选择相应缩小，完整工具组从近到远装入。
- `prepareConversation` 仅确认会话、目的地、临时正文可用性并更新会话版本。新 checkpoint 的 `context` 保存已提交轮次快照、本轮起点、所用话题 revision、临时输入投影及续跑依赖，不再包含跨轮累计 Messages。`context_history.go` 用 ordinal 反向游标，每页至多 8 轮，按窗口停止读取；异范围扫描也受输入估算限制，不以固定保留轮数决定请求。临时对话通过原有效期内的 checkpoint 引用装载，没有新增持久正文。
- `turnBody` 使用显式 `context.turn_start`，旧 checkpoint 保留 `history_count` 兼容分支。已保存旧 turn 不迁移、不压缩、不改写；原历史 API 继续完整分页。可放下的最新工具结果保留原文，较旧或超大的结果使用带“仅纳入部分内容”说明的有界摘录；checkpoint 达到整理阈值时单独保留可展示消息，淘汰已完成的旧内部工具组，保留最新／未完成组及执行状态。
- 普通历史增长不再走 `history_context_limit`。固定输入和输出预留无法容纳时使用独立 `context_configuration_invalid`，Web 提示增大窗口或减小输出上限。`MaxBody` 与 `StorageMiB` 仍为独立存储约束。

### 来源读取与引用兼容

- knowledge 新增 `ValidateSourceManifestTx`、`ReadSourceDirectoryTx` 和 `ReadSourceSliceTx`。manifest 校验只读取确切版本的身份、链接、代次和服务端计算的 canonical hash；目录每页最多 64 个节点，仅返回有界标题、节点身份和原区间。候选按本轮问题与标题匹配，分页扫描时仅保留至多 16 个候选，再读取正文；没有调用 `Retrieve` 或集合导出。
- 切片请求绑定完整 `SourceIdentity`、node ID／revision 和半开 UTF-8 字节区间，单次最多 4096 字节；读取端规范化字符边界及行号，PDF 只接受已提取文本的真实页区间。SQL 只返回指定正文区间，未将全篇 canonical Markdown 装入服务端请求组装器；文档 hash 与前缀行数的计算仍需 PostgreSQL 访问对应文档载荷。
- `SourceFragment.proof.schema=2` 保存原节点区间、实际规范化区间和切片 SHA256；原 document／node 身份由已有 source／fragment 字段携带。`verifyStoredCitations` 和 `VerifyStudioSources` 显式区分无 proof 的旧整片与 schema 2。发布和后续读取通过 owner 从确切版本重新取片，比较身份、区间、正文、原区间和 hash；未知 schema、篡改行／字节区间及跨 manifest 身份均拒绝。
- 外发前校验 owner 代次，网络期间仅保留来源链接锁，清除屏障使用既有 permit／取消机制；不把 owner 读取锁持有到模型响应结束。已有 Studio／练习消费者支持新证明，没有新增 Studio 产品入口。

### 话题读取、隔离与降级

- `ReadTopicScope(ctx, actor, space, conversation, TopicScopeRequest, send)` 面向后续 S：请求含 topic IDs、index revision、through-message ID、当前 manifest、direction revision 和 MaxTokens。最多选择 64 个 topic，投影限额 128–8192 Token；选定 ID 必须全部属于当前对话及范围，版本变化返回冲突，message 身份在该对话内核验。只返回 Topic 要点／观察、证据锚点身份、连续覆盖水位和缺口，不返回原始 Messages，也不产生模型调用或资料引用。
- 自动上下文投影按同 scope、冻结 revision 分页，每页 32 个话题；展开要点与观察，按当前问题与标题／条目文本的相关度排序，同分时近期条目、仍有疑问的条目优先，去重后逐条装入预算，整个话题放不下时仍可纳入部分要点。模型只接收话题标题、条目文本及状态文字，证据与锚点等元数据留在服务端；说明前缀和消息开销计入同一话题额度，仍保留有界候选。
- 每项证据检查来源、方向、代次及消息上界；跨上界的标题不当作已受理内容。来源撤销或记忆依赖失效的项省略，旧课堂工具正文没有确切来源证明时不外发。新增依赖随 checkpoint 保存并在模型外发门禁重新核验。
- 索引未就绪或失败时退回能放下的最近原文，保留索引已有结果和水位。回答附加“较早内容未纳入”，历史与运行 DTO 的可选 `context_notice` 让 Web 在生成期间也能显示缺口；原 Messages 的用户输入语义保持不变。
- 单条当前输入超过可变容量时，按有界字符片段调用模型生成临时要点，stage 为 `input_projection`，经过原外发门禁并进入用量台账。失败时不使用失败输出，保留可容纳的末尾原文；提示原输入字符数、保留数量和未纳入前缀数量，并允许拆开发送。完整用户输入仍保存于原 turn。投影不写入练习、评估、证据或长期记忆。

### 验证与边界

- 确定性模型与真实 PostgreSQL 测试覆盖：累计超过窗口和旧 checkpoint 体积的对话继续、原文逐页完整、当前轮保存边界、完整工具组、有界多轮工具 checkpoint、大量话题相关匹配、失败降级、零来源／换方向隔离、超大输入失败继续及台账、目录分页／大来源请求／子范围篡改拒绝、ReadTopicScope 与 Studio 新证明核验。
- 真实 PostgreSQL、`umask 022`、`-p=1` 下，`mentorrun`（最终整包 863 秒）、`knowledge/...`、`learningcontent`、`transport/httpapi` 和 `api` 的 `go test` 均通过；同范围 `go vet` 通过。首次导师整包发现最新工具结果过早截短及零来源提示回归，修复后相关用例与导师整包均通过；旧整片引用的完整来源约束也在新 schema 分支中保留并通过检查。Web `npm run check` 与构建通过，Chromium 的 `conversation-topics.spec.ts`、`notebook-chat.spec.ts` 共 11 项通过；浏览器使用独立测试 schema。未运行全量项目测试、三浏览器矩阵或真实 provider 质量抽查。
- 当前候选筛选使用标题／要点的确定性词语与中文二元片段匹配；话题和来源目录虽然分页且内存有界，扫描时间仍随候选规模增长。模型质量、同义问题的早期召回和摘要漂移留给 T 候选验证；本批不承诺每项旧事实均进入请求。旧工具采用可说明缺口的摘录，未引入额外工具摘要模型调用。
