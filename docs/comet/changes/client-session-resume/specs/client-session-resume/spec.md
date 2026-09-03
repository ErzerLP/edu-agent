# 客户端历史 Session 恢复完整规格

## 产品结果与正式边界

交互式 Go CLI Agent 必须能够在进程退出、崩溃或用户主动切换后，恢复历史 Session 的已提交对话、稳定 transcript 和有界会话上下文。恢复能力面向“找回并继续本地 Agent 对话”，不创建 Knowledge、Learning、Review、Assessment、Memory 或 workspace 文件的第二份业务真值。

本规格是后续 capability，明确取代 `learning-agent` 与 `go-cli-m1` 中“交互式 Agent 普通对话只在当前进程有效、退出不创建聊天历史文件”的旧限制，但不放宽它们对学习业务正文、离线业务队列、服务端权威状态和长期偏好准入的其他限制。只有本规格列出的 Agent Session DTO 可以落盘；Goal、Route、Activity、Attempt、Assessment、Evidence、Review 和 Memory 的权威状态仍只能通过既有公开服务边界读取或写入。

历史 Session 默认自动保存。用户可以用 `edu-agent agent --no-save` 让当前新 Session 保持进程内有效，也可以在设置页关闭后续新 Session 的自动保存；被恢复的 Session 始终是已存在的持久 Session。UI 必须持续区分“已加密保存”“正在保存”“未保存降级”和“保存失败”，不能把无持久化会话显示成已可恢复。

## 用户命令

命令面增加：

```text
edu-agent agent [--workspace PATH] [--no-save]
edu-agent agent resume [SESSION] [--last] [--all]
edu-agent agent sessions delete <SESSION> --confirmed
edu-agent agent sessions clear --confirmed
```

`edu-agent agent` 创建新 Session；零个 committed 用户轮次的空 Session 在正常退出时不得污染历史列表。`--no-save` 只影响当前新 Session，不改写持久默认。无参数 Dashboard 的“AI 学习助手”入口必须复用同一 dispatcher，并提供“新建 Session”和“恢复历史 Session”，不得复制 Session 加载或存储逻辑。

`edu-agent agent resume` 未指定 `SESSION` 且没有 `--last` 时，在 TTY 中打开恢复选择器。`SESSION` 接受 canonical lowercase UUID 或标题；如果输入可以解析为 UUID，UUID 匹配优先于标题。标题以 NFC 与 Unicode case-fold 后做精确匹配，允许重复标题；匹配不唯一时返回 `session_name_ambiguous` 并打开或提示使用 picker，不能任意选择最近一项。

`--last` 跳过 picker，选择当前规范化 workspace、当前 server profile 中最近更新且可恢复的 Session。`--all` 关闭 workspace 过滤并在 picker 显示安全 workspace 标签；它不跨 server profile。`SESSION`、`--last` 与 picker 默认路径必须严格互斥；指定了当前 scope 以外的 Session 时必须要求 `--all`，不能因为知道 UUID 就绕过 workspace 确认。

`sessions delete` 永久删除一个非当前、未被其他进程占用的 Session；`sessions clear` 永久清除当前 server profile 下全部历史 Session。两者都要求精确 `--confirmed`，失败时返回稳定错误和下一动作。首版没有 archive、trash、undo、fork、import 或 export。

Agent 仍要求 TTY 才进入完整交互界面。`agent --help`、`agent resume --help` 和 `agent sessions --help` 在非 TTY 中成功输出，不初始化模型、server、workspace、key backend 或 Session store。

## Session 范围与身份

每个 Session 使用随机 canonical UUID，并记录 schema version、created/updated/last-opened、checkpoint revision、committed user turn count、稳定 transcript count、server profile fingerprint、规范化 workspace identity、加密 workspace path、历史 provider identity/model 标签、标题值、标题来源、标题 revision 和生命周期状态。

server profile fingerprint 由规范化 server origin 派生，不包含 token 或设备 secret。设备重新配对但 server origin 不变时，历史 Session 仍可用；不同 server origin 的 Session 不在普通 list/resume 范围中，首版不提供跨 server profile 迁移。

workspace identity 使用平台正确的规范化与文件身份策略。默认 picker 只列出当前 startup cwd 对应 workspace；新 Session 的 `--workspace PATH` 仍按既有固定 root/no-follow 规则打开并把其 identity 绑定到 Session。加密 metadata 可以保存绝对 root 以便恢复，但任何错误、列表或日志只显示清理后的 basename/缩略标签，不能泄漏未选择 Session 的完整绝对路径。

标题不是唯一键、授权或业务状态。人工标题与自动标题都允许重复，Session UUID 始终是唯一稳定标识。所有排序使用 `updated_at DESC, session_id ASC` 的稳定顺序，不依赖设备壁钟之外的用户输入时间。

## 加密存储

新增独立 `internal/agentsession` store。它不得把 Session 内容放入 `config.json`、workspace、日志、Shell history、模型 credential、设备 credential、Nocturne 或服务端数据库。

每个 server profile 具有独立随机 profile wrapping key，由当前支持平台的 key backend 保护：Linux Secret Service 与 macOS Keychain。每个 Session 再生成独立 data-encryption key；Session key 由 profile key 认证包装，metadata、checkpoint、transcript/index cache 均使用 per-Session key 的认证加密。名称、用户/assistant 文本、workspace path、provider 标签和搜索摘要都是敏感数据，不得以明文 sidecar 保存。

认证加密容器必须绑定 schema、profile fingerprint、local privacy generation、Session UUID、record kind、record revision 和 nonce。nonce 永不复用；错误 key、header 不匹配、ciphertext 篡改、截断、尾随数据或 record swap 均 fail closed。生产随机源失败时不创建 Session 或 key。

存储 root 位于 `os.UserConfigDir()/edu-agent/agent-sessions` 或等价用户私有数据目录，在 Linux 与 macOS 上目录必须为 `0700`、文件必须为 `0600`。所有安全读取和发布拒绝 symlink、非普通文件、路径逃逸和过宽权限，使用同目录临时文件、file sync、原子 replace 与 directory sync。Native Linux/macOS 验证必须覆盖私有权限、no-follow、replace 和删除行为；交叉编译不算原生安全证据。

profile manifest 与 index 只可作为版本化、认证的数据结构。index 不是唯一数据源；丢失或损坏时可以扫描仍可解密、schema 合法的 Session records 重建。单个损坏 Session 被隔离并在 picker 标记 `corrupt`，不能让其他 Session 消失，也不能被静默当作空记录。

每个 Session 同时只允许一个 writer。跨进程锁必须由平台原生文件锁或等价机制实现，并结合 checkpoint expected revision；第二个 writer 返回 `session_in_use`。锁丢失、revision 冲突或 stale writer 不能覆盖新 checkpoint、标题或删除结果。读 picker 可以查看 locked 状态，但不能恢复为 writer。

默认没有时间 TTL，也没有基于最近使用的自动淘汰。实现设置集中、可注入的每 Session 明文/密文、Session 数量、总密文空间、transcript 条目和搜索摘要上限。到达上限时不得自动删除其他 Session：已有数据保持可读，安全 compaction 后仍无法保存时返回 `session_store_full` 或 `session_save_failed`，并持续显示当前新内容未持久化。

平台 key backend 不可用、profile key 丢失或 store 无法安全打开时，绝不写明文或使用配置文件 key。普通 `edu-agent agent` 可以在持续可见的 `session_store_unavailable` 状态下启动 unsaved Session；`resume`、delete 与 clear 失败并提供恢复 key service 或使用新 unsaved Session 的下一动作。

## 稳定 Checkpoint

持久化使用版本化、严格、大小有界的 DTO，不直接 JSON 序列化 `agentloop.Session`、`ContextRuntime` 或 Bubble Tea model。unknown field、非法 enum、重复 ID、断裂引用、越界计数、非法 UTF-8、terminal/bidi control 或多余顶层 JSON 都必须拒绝。

可恢复 checkpoint 包含：

- 非在途的 committed model messages、tool call/result 原子组和 message-to-turn 关系；
- completed/protected/outcome-unknown turn 的稳定元数据与下一 turn sequence；
- tool history projection、ServerReference、WorkspaceReference 和其 invalidation/supersession 状态；
- Context Sources、RecallText/availability/hash/retention、Observations、Reflections、coverage edge、watermark、supersession、tombstone、authority、freshness 和已分配 opaque ID；
- 当前 Session 的 reasoning-effort override、context-compaction mode 标签和必要迁移元数据；
- presentation-safe durable transcript；
- 可能未知的 preference operation receipt 与可能未知文件副作用的非可执行摘要。

以下内容禁止持久化：

- 固定 system prompt、workspace system prompt、完整工具定义与 JSON Schema；
- 模型 API key、设备 token、bearer token、pairing code、credential backend secret 或 profile plaintext key；
- hidden reasoning、reasoning continuation、Observer/Reflector/title prompt、内部 chain-of-thought 或 provider 私有帧；
- 原始工具参数、未投影原始 provider/OS body、绝对路径错误、running Activity 或 spinner tick；
- `activeTurnID`、可执行 pending sibling calls、question/preference/file resolver、旧 mutation preview/BaseVersion、旧批准或 YOLO；
- tokenizer EWMA、cache-hit 统计、goroutine、channel、context、worker backoff 或其他进程瞬时对象。

恢复时由当前二进制重新生成 system prompt、workspace prompt、工具 Schema、限制和安全合同。Checkpoint 中的历史用户或工具文本始终是 evidence，不能成为 system instruction。旧工具组必须保持协议完整；失效正文以结构化 placeholder 替换而不是产生 orphan tool result。

`StableCheckpoint()` 只导出最后一个安全线性化点。active turn、普通 pending interaction 或正在 commit 的 mutation 不得被导出为“可继续”；export 可以返回上一稳定 revision，并由 dirty marker 表达有未完成工作。Context worker 必须先停止或使用已提交不可变 ledger snapshot，晚到 Observer/Reflector/title 结果不能写入已经切换或关闭的 Session。

## 自动保存与崩溃恢复

Session 在首个用户 turn 开始前获得 ID，并在该 turn 发送前原子写入加密 dirty marker。dirty marker 只记录 Session/turn/revision、开始时间、操作类别和是否可能包含副作用，不包含正文、参数、授权或凭据。

完成 ordinary assistant、终止/cancelled 的可见 turn、完成或 unknown 的副作用 turn、标题更新、人工改名和安全 Session switch 时发布原子 checkpoint。模型历史仍遵守现有 rollback：未完成 assistant draft 可以留在 durable transcript 并标记 stopped/failed，但不能作为完整 assistant message 进入下一模型请求。

正常关闭在清理内存前尝试保存最新稳定 checkpoint；保存失败必须在 TUI 中可见并使 close/switch 路径诚实报告“最近内容未保存”。`Session.Close` 不能先清空 ledger 再导出。空 Session 在没有 committed 用户文本、没有副作用 receipt 且没有人工标题时正常关闭后删除其临时 metadata。

恢复发现 dirty marker 时只载入上一稳定 checkpoint，并追加本地 `session_interrupted` transcript card。客户端不得自动重发模型请求、执行剩余工具、提交 question answer、批准文件 mutation、切换 YOLO 或创建/决定 Memory candidate。

可能未知的 preference 写入在调用前以加密 write-ahead receipt 保存原 create/admit/reject operation IDs、candidate reference 和阶段；恢复后只能显示 retry-only 核对入口，并复用原 IDs。不得自动核对、自动重试、回到“仅本次/不采用”或生成新 operation ID。

可能未知的文件 publication 只保存相对路径、operation、stable code、publication outcome 和必要 reference，不保存可复用批准。恢复后同路径 WorkspaceReference 标记 stale/unknown，后续 mutation 必须重新 read/prepare/preview/authorize。规格不承诺文件 publication 与 Session checkpoint 跨资源 exactly-once。

## Resume 加载与兼容性

恢复顺序必须先锁定并完整验证目标 Session，再保存当前 Session、停止其 worker 并切换 UI。目标解密、schema、migration、privacy fence、provider gate、workspace open 或 lock 任一步失败时，当前 Session 保持活跃且未被关闭。

Store 支持明确的 schema version 与有界逐版本 migration。Migration 使用新临时 ciphertext 和原子 publication，旧记录在新记录确认前保持可恢复。未知未来版本返回 `session_version_unsupported`；损坏返回 `session_corrupt`，不得尝试宽松解析、字段猜测或把失败文件重置为空。

恢复使用当前全局 provider、base URL、model、context window、reasoning capability、tool definitions 与 model credential。历史 metadata 只用于显示“原 provider/model”。模型名在同一 endpoint identity 内变化只显示提示；provider preset 或规范化 base URL identity 变化时，Session 可以本地打开，但任何历史正文、自动标题输入或下一模型请求在一次明确确认前均不得发送到新 endpoint。拒绝确认后用户仍可查看 transcript、重命名、删除或退出。

恢复的 Session reasoning-effort override 可以保留，但如果当前模型明确不支持该值，沿用现有 `reasoning_effort_unsupported` 恢复语义，不静默降级。模型 API key、provider continuation 和 cache calibration 永不从 Session 恢复。

文件授权模式在所有跨进程 resume 和 TUI Session switch 后固定为 `confirm`。即使 metadata 来自同一 Session，YOLO 也不能跨恢复边界生效；TUI 必须显示已重置。普通 question、preference selector 和 file confirmation panel 不恢复为 active UI。

## Workspace 恢复

默认 picker/`--last` 使用当前 startup cwd 的规范化 workspace identity 过滤。`--all` 才能选择其他 workspace 的 Session，并显示加密 metadata 解密后的安全 workspace label。跨 workspace 选择必须在打开目标 root 前显示确认，说明历史文件内容可能再次发送给当前 provider。

恢复跨 workspace Session 时尝试以其保存的绝对 root 重新建立现有 root-confined workspace executor。所有既有路径、symlink/junction/reparse、ADS/device 和权限规则保持不变。root 不存在、已移动、无权限或不安全时，Session 仍可恢复本地对话，但五个文件工具全部不注册，并显示 `session_workspace_unavailable`；不得自动使用当前 cwd、父目录或相似路径。

首版不提供 workspace relocation。用户需要在新 workspace 继续时创建新 Session；未来 fork/relocate 必须单独设计历史 evidence 与 authority 迁移。

恢复后历史 workspace tool results 是本地历史 evidence，不代表磁盘当前内容。write/edit 仍需重新读取、expected hash、Prepare/Commit 和当前授权。历史文件正文不能启用 YOLO、扩展 root 或授权其他工具。

## 服务端 Authority 与 Privacy Generation

历史 ServerReference 必须保持 revision/generation/reference。恢复后所有 server snapshot 默认是 historical，不得被描述为当前 Learning、Route、Review、Knowledge 或 Memory 状态；需要行动时继续通过既有 authenticated tools 重新读取。

受 learner/memory privacy generation 或 redaction 保护的正文及其派生 assistant/source/Observation/Reflection 在注入模型前必须通过现有服务端边界核对。generation 相同才可作为有版本历史 evidence；generation 不同、`content_redacted` 或 `privacy_clear_in_progress` 立即使正文失效并持久化新的 invalidation checkpoint。

无法联网或无法完成 privacy fence 时，受保护的 server body 不进入模型上下文和 title 输入；对应 tool result 用保持协议完整的 `session_privacy_revalidation_pending` placeholder 替代。用户自己提交的本地文本可以继续恢复，但不得用它恢复已清除的 server 正文。

本地 Session clear 维护独立、单调 local privacy generation。每个加密 record 绑定 generation；全量 clear 销毁旧 profile key 可达性、推进 generation、删除 ciphertext/index/temp 并验证旧 generation 不能重新注册。individual delete 删除 wrapped Session key 和全部 records，使残留 ciphertext 在当前 store 中不可解密。

## Durable Transcript

持久 transcript 使用独立版本化 DTO，而不是序列化私有 `transcriptEntry`。允许的 entry 包括清理后的用户消息、完成 assistant 消息、stopped/failed assistant draft、terminal tool activity、稳定 error card、context compaction/degraded/source-unavailable card、file/preference outcome card、provider/workspace/session interruption notice。

不保存 running activity、tick、selector focus、composer draft、原始工具参数、工具全文、hidden reasoning、provider raw body、绝对 workspace root、credential、未清理 OS 错误或后台 Observer/Reflector/title 内容。Entry 在写入和恢复渲染两端都执行 UTF-8、字节、行数、显示宽度、terminal/bidi control 与类型边界。

Transcript 具有独立总大小和条目上限。达到上限时只允许按稳定规则把最老、非关键 presentation entries 替换为计数 placeholder；不得删除模型 checkpoint、unknown side-effect receipt、用户约束或没有 Context exact coverage 的必要 evidence。任何缩减必须在恢复界面显示“较早展示记录已收起”，不能伪装为完整 transcript。

## TUI Session Picker 与切换

Agent 空闲时 `F2` 打开 Session picker。存在 active turn、正在停止、pending question、preference confirmation、file mutation、retry-only unknown outcome 或正在保存/切换时，`F2` 不打开 picker，当前状态不改变，并显示有界不可切换原因。

Picker 默认 current-workspace scope，按最近更新时间排序；`Tab` 切换 current/all scope，all scope 显示 workspace label。直接输入进行有界 fuzzy 搜索，匹配解密后的标题、Session UUID、首条与最近有界用户摘要、workspace label；首版不搜索完整 transcript，不支持 regex、quoted phrase 或 threaded view。`↑/↓`、`PgUp/PgDn` 移动，`Enter` 恢复，`Esc` 返回，`Ctrl+R` 重命名，`Ctrl+D` 进入永久删除二次确认。

列表显示自动/人工标题、最近更新时间、committed user turn count、workspace label，以及 current/locked/corrupt/unavailable 状态。绝对路径、server URL、provider URL、opaque tool/reference ID 和正文不直接显示。`46×18` 下仍能看到当前选择、标题、恢复/取消提示；次要字段按 workspace、消息数、时间顺序降级。

人工 rename 输入经过 NFC、单行、字节、rune、显示宽度和 control 清理；空输入清除人工 override 并恢复自动标题。rename 使用 expected revision，不能覆盖并发更新。当前 Session 的标题更新在 footer/status 有界显示，不改变 top product identity。

删除必须二次确认且不能删除当前 active 或 locked Session。删除失败保持列表与当前 Session；成功后从 index 消失并验证 wrapped key/records 已删除。Picker 不提供默认永久删除快捷旁路或无确认操作。

切换目标前先建立目标锁、解密和验证目标，再保存当前稳定 checkpoint。成功后替换 Conversation、transcript、context event channel、workspace、Session title 和 controller generation，清空 composer/selector/running UI；旧 worker、late delta、Activity、context event 或 title result 按旧 generation 丢弃。目标与当前相同是 no-op。

Picker 包含“新建 Session”入口。选择后先安全保存当前 Session，再创建同一当前 workspace 下的新 Session；若保存失败或 store full，不关闭当前 Session。首版不从旧 Session fork 上下文。

## 模型自动标题

自动标题使用当前 Agent provider/model 的独立、有界、无工具请求。输入只包含当前自动标题、首条 committed 用户文本和最近有限个 committed user/final-assistant 文本的清理截断；不包含 tool result、workspace 文件正文、server snapshot、Source RecallText、hidden reasoning、工具参数、错误 raw body、credentials 或 unknown receipt。

首个稳定 completed turn 后调度首次标题；仅在标题来源为 `auto` 时，至少新增固定数量 committed user turns 且满足最短时间间隔后才可再次调度。一个 Session 同时最多一个 title run；普通 Agent turn 不等待标题。切换、关闭或人工改名后，旧 title result 必须因 Session/title revision 不匹配而被拒绝。

标题响应使用严格结构化 schema，只允许一个 `title` 字段；不提供 workspace/server tools。标题必须是单行、非空、有界 UTF-8、无 terminal/bidi control、无 credential/opaque-ID 样式泄漏并满足显示宽度上限。非法响应、provider 错误、timeout 或 content filter 不修改旧标题、不影响 model history，也不触发无限重试。

标题调用失败或尚未完成时，display fallback 使用首条用户文本的本地有界单行摘要；没有 committed 用户文本时显示“新 Session”。Fallback 不声称为模型标题。人工 rename 将 `title_source=manual` 并阻止后续自动覆盖；用户清空人工标题后，下一稳定边界可以重新调度自动标题。

标题请求会把上述 bounded conversation snippets 发送到当前 provider。README、设置页和第一次显示 Session 保存状态的 TUI 帮助必须披露这一点。provider endpoint identity 变化时，标题请求与普通 resumed model request 共用同一个显式确认 gate。

## 错误与降级

稳定错误/状态至少包括：

| Code | 含义 |
| --- | --- |
| `session_store_unavailable` | 平台 key backend 或安全 store 不可用，仅可运行 unsaved Session |
| `session_store_full` | 数量/空间/单 Session 上限阻止新持久化，不自动淘汰 |
| `session_save_failed` | 当前稳定 checkpoint 未能安全发布 |
| `session_not_found` | 当前 server/workspace scope 内无目标 Session |
| `session_name_ambiguous` | 标题匹配不唯一 |
| `session_in_use` | 另一个进程持有目标 Session writer lock |
| `session_corrupt` | ciphertext、schema 或引用损坏 |
| `session_version_unsupported` | Session 来自未知未来 schema |
| `session_checkpoint_conflict` | expected revision 或 writer generation 冲突 |
| `session_interrupted` | dirty marker 表明上次 turn 未稳定完成 |
| `session_workspace_unavailable` | 原 workspace 无法安全恢复，文件工具禁用 |
| `session_provider_confirmation_required` | 新 provider endpoint 尚未获准接收历史正文 |
| `session_privacy_revalidation_pending` | server privacy generation 尚无法核对，相关正文被 placeholder 替代 |
| `session_delete_failed` | 删除或 clear 未完成，不能宣称已清除 |
| `session_title_failed` | 自动标题失败，继续使用旧标题或本地 fallback |

错误只显示稳定 code、清理后的 Session/workspace 标签和一个下一动作；不显示 ciphertext、key account、绝对路径、raw JSON、provider body、OS error、tool args 或 credentials。Store unavailable/title failure 不阻断普通 unsaved conversation；corrupt/in-use/provider-confirmation/privacy fence 只阻断不安全的恢复或发送路径。

## 配置与披露

普通配置增加 Agent Session history 开关，默认 `auto`，合法值为 `auto` 或 `off`。它不保存 title、Session ID、workspace path、conversation、key 或 index。`--no-save` 覆盖当前新 Session 为 off，不修改配置。

设置页和 README 必须说明：

- 默认在本机加密保存 Agent 对话与有界工具/context evidence；
- 默认不按时间自动删除，必须手动 delete/clear；
- 达到上限不会自动淘汰，但最近内容可能无法保存并会明确提示；
- 自动标题会把有界近期对话片段发送到当前 provider；
- 恢复后的下一模型请求也会把历史上下文发送到当前 provider，provider endpoint 变化需确认；
- workspace 文件正文可能已包含在加密历史中，恢复不代表磁盘当前内容；
- key backend 不可用时不降级明文，Session 只在当前进程有效；
- clear 只清除本地 Agent Session store，不清除服务端事件、Nocturne、terminal scrollback、Shell history、provider retention 或 OS backup；
- YOLO、旧文件授权和 pending interaction 不恢复。

`agent --help` 列出 `resume`、`sessions delete/clear`、`--last`、`--all`、`--no-save`、默认 workspace scope 和加密保存摘要。TUI footer 在宽度允许时显示当前 Session 标题与保存状态；窄终端必须优先保留运行状态、保存/未保存、模型与发送/退出提示。

## 代码边界

建议新增：

```text
clients/cli-go/internal/agentsession/types.go
clients/cli-go/internal/agentsession/store.go
clients/cli-go/internal/agentsession/crypto.go
clients/cli-go/internal/agentsession/checkpoint.go
clients/cli-go/internal/agentsession/transcript.go
clients/cli-go/internal/agentsession/naming.go
clients/cli-go/internal/agentsession/lock_unix.go
clients/cli-go/internal/agentsession/lock_windows.go
```

`agentloop` 提供显式 `StableCheckpoint`/`Restore` 转换和 stable commit hook；`agentui` 接受可替换的 Session controller，而不是把固定 Conversation 当作整个程序生命周期；`command` 注入 store/factory 并统一 new/resume/dashboard；`keybackend` 与 `securefile` 只提供通用保护原语，不让 Session store依赖 offline 业务对象或服务端内部包。

实现可以复用现有 `internal/offline` 已验证的 AEAD header、nonce、原子 recovery 和 platform key backend 思路，但 Session record、binding、retention、lock 和 privacy generation 必须拥有独立类型与 schema，不能把 Session 假装成离线 Pack/Operation。

## 实施批次

### 批次 1：安全 store 与 Checkpoint

交付 per-profile/per-Session key、strict encrypted records、single-writer lock、revision、dirty marker、index rebuild、delete/clear generation，以及 agentloop/context/transcript stable round-trip。退出标准是压缩后的长 Session 可 restore，active/pending 不可执行恢复，store corruption/limits 明确失败。

### 批次 2：命令恢复、隐私与标题

交付 `agent resume`、`--last`、`--all`、`--no-save`、workspace/provider gate、privacy generation revalidation、unknown receipt 和模型自动标题。退出标准是 fake model/server 下退出重启后下一请求等价，provider 改变零发送直到确认，标题无工具且失败不影响主线。

### 批次 3：F2 切换与支持平台闭环

交付 F2 picker、搜索/scope、rename/delete/new/switch、late-event isolation、Dashboard/help/settings/README、hard quota UI 和 native Linux/macOS evidence。退出标准是繁忙/unknown 状态无法切换、目标失败保留当前 Session、两个支持平台的 required security cases 零 skip。

## 验收与验证

Agent-loop 测试必须覆盖：普通与取消 turn 的 checkpoint 线性化、20+ turn raw trim 后 round-trip、exact-ID recall、DropExactCoverage provenance、server/workspace authority/freshness、tombstone/supersession、redaction/generation invalidation、unknown preference receipt、unknown file publication、YOLO reset、system/tool regeneration和 active/pending export 拒绝。

Store 测试必须覆盖：create/list/load/save/delete/clear、per-Session key isolation、wrong key、record swap、bit flip、truncate/trailing bytes、unknown schema、duplicate IDs、broken references、nonce uniqueness、atomic crash points、temporary residue、index rebuild、expected revision、two-process lock、quota no-eviction，以及 Linux/macOS 的私有 mode、no-follow、原子 replace 和删除。

Command 测试必须覆盖：new auto-save、`--no-save`、empty Session cleanup、picker、UUID/name/ambiguous、`--last`、`--all`、scope、server mismatch、workspace unavailable、provider confirmation、store unavailable、delete/clear confirmation、non-TTY help、Dashboard mapping 和稳定退出码。

TUI 测试必须覆盖：F2 idle gate、busy/pending/unknown refusal、scope/search/sort、rename/manual override/reset、delete confirm、new Session、target prevalidation、save-before-switch、late delta/activity/context/title isolation、restored transcript、store-full/unsaved footer、provider/workspace confirm 和 `46×18`。

Title fake provider 必须证明请求没有工具或 tool schema，不含 raw tool/workspace/server evidence、credentials、hidden reasoning 或 unknown receipt；覆盖节流、并发、timeout、malformed/multiline/control/oversize、manual-name race、provider change gate 和 fallback。

候选 gate 运行完整 Go CLI test/race/vet/build、non-TTY help、`git diff --check` 与 diagnostics。原生 workflow 在绑定 candidate SHA 的 Linux 与 macOS 上执行 key backend round-trip/delete、私有 mode、symlink/no-follow、single-writer lock、atomic recovery、tamper、privacy clear 和 required skip-count 检查；交叉编译或静态源码审查不能替代支持平台的原生行为证据。

## 非目标

首版不实现云/服务端 Session 同步、跨设备恢复、共享、多用户、Web Session UI、fork/branch、archive/trash、undo、import/export、workspace relocation、跨 server profile migration、full-transcript/regex/threaded search、自定义 Session keybindings、非交互 Agent history、模型 chain-of-thought 持久化或旧 provider continuation 恢复。

当前版本只支持 Linux 与 macOS。Windows 构建、运行、凭据后端、文件权限、锁和 Session 恢复行为不在本规格的产品支持或验收范围内；仓库中存在的 Windows 条件代码仅为非承诺的兼容性准备，后续支持必须通过独立 change 重新 Shape 和验收。
