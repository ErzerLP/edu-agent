# 对话话题索引与上下文压缩：设计

状态：Shape 待审（2026-10-18）。对应 [brief](../comet/specs/conversation-topics/brief.md)、[spec](../comet/specs/conversation-topics/spec.md)，前置为 [U](model-usage.md)。T1 暂按建议默认值。

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

旧数据按 T1 惰性补建，不在 migration 中调用模型。首次只创建必要锚点并按原水位分批整理；历史格式不可识别时保留现有 schema 错误，不能解析成空历史。现有客户端不需要新字段；新增 topics API 独立，原历史 DTO 仅可增加稳定 message 标识，不改变旧 Messages 意义。关闭新 UI 不删除已保存索引，也不回退到无界历史组装。

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
