# Outcome

交付 M1 的受控长期记忆与隐私清除边界。Go 服务以 PostgreSQL 保存 Memory Candidate、准入决定、无正文的 Memory Record 元数据、Outbox 意图、generation fence 和逐存储删除回执；固定版本的 Nocturne Memory sidecar 保存已准入的长期记忆正文。Nocturne 故障不能阻断知识、教学、评估、查询或学习状态重放。

# Scope

本 child 实现 `memory` 领域与应用服务、Nocturne REST 适配器、事务 Outbox consumer/worker、跨模块 privacy erasure 编排、PostgreSQL migration、HTTP/OpenAPI、配置、readiness 和 Compose 部署。它承接父级 A12、A13、A16、A30、A53-A59。

Memory Candidate 支持用户明确陈述、模型推断、长期背景和生成摘要等来源，保存来源、提议者、敏感级别、稳定性、有效期、理由、policy version 和审阅状态。版本化准入策略只允许明确、稳定、非敏感的交互偏好或时间约束自动准入；其他候选等待显式 admit/reject。

Nocturne 固定为 release `2.5.6`、commit `54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254`。项目overlay固定base digest、Python artifact hash、Debian snapshot、BuildKit、platform和source epoch，生产Compose只消费registry platform manifest digest或验证过的离线OCI layout。部署使用独立数据库、账号、volume、API/maintenance token和仅内部可达网络；不把namespace当作授权边界。

隐私清除是独立于业务纠错的全局单用户操作。它先提交 tombstone 和 generation barrier，再通过各数据 owner 的窄事务端口清理本地正文、索引、缓存、Inbox/Outbox payload 和所有 projection generation，最后解除 Nocturne 路径、逐个永久删除 deprecated orphan/history 并重新查询验证。操作生成逐存储回执和最多 30 天的备份不可恢复截止时间。

# Non-goals

本 child 不实现 Go CLI、离线同步、Fast Note Sync、MCP transport、知识自动维护、Web UI、多用户或 Nocturne Dashboard。它不把 Nocturne 变成目标、路线、掌握度、答题、误区、复习或同步状态的权威存储，也不从原始聊天或完整答题中自动抽取长期记忆。

本 child 不复刻 Nocturne 的图数据库、版本系统或管理界面，不让 Go 服务直接读取 Nocturne 数据库，不用 Nocturne namespace 代替认证，不依赖其 changeset 文件作为审计真值，也不把普通 `delete_memory` 或 REST node delete 描述为永久删除。

隐私回执只承诺受管理活动存储的逻辑清除和受管理备份生命周期。它不承诺擦除 PostgreSQL WAL、宿主磁盘空闲页、虚拟机快照、用户自行复制的导出、终端日志或已发送给外部模型供应商的数据；在验证未完成或备份截止时间未到时不得显示“彻底清除”。

# Acceptance examples

- A1：准入矩阵证明只有用户明确陈述、稳定、非敏感的交互偏好或时间约束可以按 `memory-admission-v1` 自动 admit；模型推断、敏感信息、长期背景和生成摘要保持 pending review。
- A2：Candidate 查询返回状态、来源事件或操作、提议者、理由、policy version、敏感级别、稳定性、有效期和 candidate URI；并发或陈旧 decision 使用 expected revision 返回冲突而不静默覆盖。
- A3：原始聊天、完整答题、目标、掌握度、误区结论、路线、复习队列和同步状态在准入与 adapter 两层均被拒绝，且不创建 delivery、Outbox 或 Nocturne 内容。
- A4：pending Candidate 和 queued delivery 的正文只存在于可清理的有界 payload 表；pending详情可审阅正文，Candidate/Delivery expiry worker在启动补扫并以不超过5分钟间隔执行CAS cancel/scrub，曾sent的到期attempt继续用无正文URI/hash任务对账和删除，reject、applied、permanent rejection或verified expiry后Go Record仅保留hash、external URI/ID、revision、generation、状态和回执。
- A5：部署预检验证 Nocturne `2.5.6` 的 source commit、完整依赖/apt snapshot、BuildKit/platform输入和registry manifest/config digest；Compose只消费锁定image，使用独立数据库、账号、volume、token和内部网络，Go与Nocturne数据库账号不能互读。
- A6：真实固定Nocturne契约覆盖Bearer成功/失败、health、browse node create/read/update/delete、search、global orphan和positive-integer memory ID permanent delete；兼容maintenance capability完整枚举node引用、boot epoch和backup inventory，任何响应漂移会阻止升级。
- A7：记忆写入在响应时读取 durable delivery 状态并返回 `applied / queued / rejected`；相同 operation/hash 重放原 Record 与状态，不创建第二个 Candidate、Record、delivery 或远端节点。
- A8：Nocturne 下线时教学、知识、评估、查询和重放继续工作，readiness 为 HTTP 200 degraded，记忆写入为 queued；恢复后同一确定性 URI 被对账并应用。
- A9：远端已提交但响应丢失时，重启后通过确定性 URI 和 canonical content hash 对账为 applied，不盲目 append、覆盖未知内容或生成重复节点。
- A10：两个 worker、乱序 revision、lease 过期和 delete 并发下，同一 logical memory 最多一个有效远端 attempt；attempt进入sent/unknown后重领只能对账，旧 revision、旧 generation 或 tombstoned delivery 在调用前后均被 fence，不能复活已删除内容。
- A11：瞬时错误超过自动预算进入dead letter且保持queued；payload在有效期内可受fence保护地人工replay。到期后正文立即scrub，但任何sent/unknown attempt继续以无正文URI/hash/boot-epoch任务对账，匹配远端内容会被永久删除，absence verified前不终态expired。
- A12：privacy barrier先关闭所有正文read/write gate并等待或取消旧read permit，再在独立事务递增全局generation、建立tombstone和冻结旧attempt；提交后各owner独立幂等scrub Candidate、delivery、Outbox/Inbox及业务payload，barrier前启动的旧generation写在提交CAS时失败，旧read不能在barrier后发布正文。
- A13：隐私清除与在途或结果未知的远端请求并发时，回执保持 pending/partial；只有 attempt 排空并完成 URI/hash 对账及后续清除后才能进入 verified。
- A14：单条删除和全局清除按“完整枚举全部path/alias/reference -> 解除路径 -> 确认deprecated orphan -> 逐个permanent delete history -> browse/search/orphan/reference复查”执行；active ID的409或任何不可枚举引用使回执保持partial。
- A15：真实 PostgreSQL 扫描证明 identity 可识别标签、knowledge 正文与索引、learning/tutoring typed payload、event payload、Inbox/Outbox、memory payload 及 active/retired projection generation 不含原正文，只保留冻结的最小审计字段和 tombstone。
- A16：从零重放先识别持久 redaction barrier，对已清 payload 使用版本化 no-op/补偿语义，能够完成并且不恢复 Evidence、正文、旧 focus、旧 route 或退休 projection 内容。
- A17：Nocturne 故障期间本地 scrub 可以完成，但总回执为 partial；恢复后同一 erasure operation 幂等续跑远端清理，不重复删除其他记录或错误宣称完成。
- A18：所有managed Nocturne migration backup只以generation专属key envelope encryption落盘；barrier事务销毁旧generation wrapped key并验证不可恢复，使实际时间不晚于`erasure_requested_at + 30d`，周期inventory/prune另负责普通retention和文件清理，operator/external backup与模型限制持续单列。

# Constraints and invariants

Go/PostgreSQL 是 Candidate、decision、Record metadata、operation idempotency、delivery、receipt、generation 与 privacy workflow 的权威源。Nocturne 在 applied 后是长期记忆正文的唯一权威源。pending Candidate 和未完成 delivery payload 是有 TTL、可 scrub 的暂存意图，不是第二份长期记忆库。

Nocturne 没有业务幂等键、expected revision、ETag 或 generation fence。Bridge 必须使用不含 PII 的确定性 URI、canonical content hash、每 record 串行 attempt、调用前后 fence 和超时后读回对账；不能把至少一次投递安全性委托给上游。

所有 Go 本地权威变更、Candidate decision、delivery payload pointer 和 Outbox enqueue 在调用方拥有的 PostgreSQL 事务内提交。Outbox JSON 只保存 payload ID、hash、revision 和 generation，不保存长期正文。远端网络调用不持有普通业务事务。

隐私清除由 `privacy` 应用服务编排，但 identity、knowledge、learning、tutoring 和 memory 各自拥有 scrub 规则与表。第一barrier事务只通过各owner的generation-gate port关闭读写并推进generation；提交后编排器再为每个owner提供独立DBTX调用`RedactTx`/`VerifyRedacted`窄端口，步骤可幂等续跑。privacy store不直接查询或修改其他模块的私有表。

外部调用超时、进程崩溃或 sidecar 不可用时，未知结果不能转换为 success。回执必须保留可恢复的 pending/partial 状态，直到确定性对账和 absence verification 完成。

# Decisions

Nocturne 使用固定 REST capability，而不使用主要返回自由文本的 MCP tool 作为生产 bridge。锁定兼容镜像以`python main.py`启动并allowlist所需REST；在精确upstream commit上增加最小、受认证、internal-only的`edu-agent-maintenance-v1`，只提供build/boot epoch、完整node reference枚举、review-reference清理/复查和managed backup inventory/prune；maintenance token至少256-bit、独立且常量时间校验。extension不拥有准入或generation。固定namespace只用于URI路由；真正隔离来自专用API token、maintenance token、内部网络和独立数据库边界。Dashboard、SSE和MCP路由不部署也不暴露。

包边界为 `server/internal/memory`、`server/internal/memory/postgresstore`、`server/internal/privacy` 和 `server/internal/integrations/nocturne`。通用 Outbox 只管理投递状态，Nocturne consumer 负责 revision/generation fence、对账、业务 receipt 和正文 scrub。

外部 URI 使用固定 domain、受控 parent 和 logical memory UUID，revision 或 generation 变化不创建随机 sibling。所有内容修正先创建correction Candidate并经过相同准入/审阅；admitted correction才创建新的不可变Go Record revision，并通过Nocturne完整内容update产生上游版本；旧Go revision在新版本确认后superseded。

HTTP 使用 `memory:read`、`memory:write`、`privacy:read` 和临时effective `privacy:erase` scope。migration与新配对默认只授予前三者；`privacy:erase`必须由服务器本地命令为指定device生成高熵、短时、一次性、仅存hash的erasure grant，并在Bearer认证后消费。所有 proposer、actor 和 device 身份来自认证或服务端模型配置，不信任请求体自报。

Nocturne 是 optional availability dependency：配置和供应链锁错误阻止启用，运行时不可用只使 memory component degraded。隐私清除可以本地完成后等待远端，不因 sidecar 故障撤销 generation barrier。

# Open questions

无。

# Verification expectations

领域测试覆盖完整 Candidate/Record/delivery/privacy 状态机、准入矩阵、禁止内容、expiry、scrub 时点、公开状态、确定性 URI/hash 和回执措辞。HTTP/OpenAPI 测试覆盖四个 scope、strict JSON、operation/hash replay、expected revision、分页、错误 envelope、payload limits、日志与 secret 脱敏。

真实 PostgreSQL 测试从 migration `000001` 升到 `000004`，覆盖同事务 Candidate/decision/Outbox、并发 decision、worker lease、乱序 revision/generation、Candidate/Delivery expiry、sent-after-expiry无正文reconciliation、每个 scrub 故障点、barrier 与在途 attempt/read permit、所有 projection generation 清理和 redacted full replay。fake Nocturne 覆盖 timeout-before/after-apply、响应丢失、重复、冲突、延迟写、expiry后延迟写、orphan 链和永久错误。

固定 Nocturne `2.5.6` 的真实容器契约和 Compose E2E 必须验证认证、精确query/body/status、CRUD、soft delete、active/deprecated integer ID、完整引用枚举、orphan/history permanent delete、独立数据库权限、registry/OCI digest、`python main.py` route allowlist、sidecar down/restart、readiness degraded、read permit/barrier竞争、恢复投递，以及envelope-encrypted migration backup在key销毁前可恢复、销毁后不可恢复和普通retention prune。普通 Go test、race、vet、govulncheck、OpenAPI/YAML、gofmt、git diff 和错误级 diagnostics 始终运行。
