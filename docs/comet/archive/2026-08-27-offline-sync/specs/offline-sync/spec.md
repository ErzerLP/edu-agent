# Offline Sync Capability

## 产品结果

已经配对的 Go CLI 可以在线取得服务端签发的离线 Activity，在没有网络时安全阅读和作答，并在恢复联网后上传不可变 operation。服务端逐项存档、生成 canonical Learning Event、执行适用评估和 Evidence 接纳，并分别返回 archive 与 Evidence 状态。

PostgreSQL 始终是 Activity、Attempt、Assessment、Decision、Evidence、Mastery、Review、Learning Event、Inbox、设备序号和同步结果的权威来源。客户端队列不是事件日志副本，也不是 projection 重建输入。

## 交付阶段

完整 capability 按五个顺序阶段实现。S1 建立领域、事件、JCS、migration 和 transaction port；S2 交付可调用的服务端 Objective prepare/sync/status 闭环；S3 交付使用加密本地队列的真实 CLI 闭环；S4 增加 Open Assessment、多设备和在线/离线竞争；S5 增加设备隐私清除、系统 key backend、迁移和最终平台加固。

S5 保留 Linux Secret Service、macOS Keychain 和 Windows DPAPI 的生产适配。本 change 的原生候选门禁只要求 Linux；macOS/Windows 的原生权限、锁、原子替换、迁移和 purge 验证是明确的外部 follow-up，未运行时保持 not-run 且不标记为通过，不阻塞当前 capability 归档。

每个阶段必须满足 `docs/design/offline-sync-delivery-plan.md` 的退出标准。阶段完成代表可提交的增量，不代表整个 change 已通过；只有 S5 完成并经 Runtime 验收后 capability 才归档。

字段级签名对象、容器布局、状态矩阵和错误闭集保存在 `docs/design/offline-sync-technical-reference.md`。这些内容是实现参考而不是独立的用户验收项；与本文冲突时本文优先，未进入当前阶段的技术细节不得扩大当前 Build 范围。

## 权威边界与所有权

`learning` 拥有离线 Activity、Attempt、Assessment、Evidence、事件、投影和同步裁决。`tutoring` 只提供当前 Session/Route 权威上下文，离线流程不得直接写入或推进 tutoring 状态。`knowledge` 通过公开能力冻结和验证不可变 revision、node 和 canonical slices。`identity` 拥有设备、credential epoch、授权和设备序号资格。

调用方定义跨 owner port，并在需要原子快照时提供 caller-owned transaction adapter。learning PostgreSQL store 不直接查询 tutoring 或 knowledge 私有表，也不建立拥有所有业务实体的通用 Repository。

离线 Activity 是独立 practice/review 工作项。它保留来源 Session、Goal、Route 和 knowledge 引用供审计与 Evidence 使用，但同步不会推进、切换、完成或恢复在线 Session。

## 包与授权

`offline prepare`要求有效设备身份、`learning:write`、开放的 owner privacy gates 和具有冻结 Goal/Route 的有效 Session。服务端返回有界包，默认五项、允许一至二十项，并受单项、总字节和有效期限制；无法填满时返回较小包和稳定截断原因，不发布空包。

每项包含不可变 offline Activity、独立设备 submission、预分配 operation ID 和单调 device sequence。客户端不能自造、交换、复用或递增改写这些标识；未使用 submission 可以留下缺号，但不阻塞后续合法序号。

包、authorization 和响应使用版本化服务端签名并绑定 origin、device、learner generation、Activity digest、submission、operation 和期限。客户端只接受从配对建立的 trust root 连续推进的 signer manifest；回滚、分叉、未知 signer、跨 origin/device/generation 复制或 payload 篡改均失败。

相同 prepare operation 和 canonical hash 重放同一持久化结果，不重复模型工件、Activity、submission、sequence 或签名事实。prepare 的外部模型调用和最终发布使用可恢复 claim，最终响应只来自已提交 canonical bytes。

## 本地安全队列

Activity、knowledge slices、答案、operation、receipt 和 journal 始终经过 authenticated encryption 后落盘。最早可运行阶段使用隐藏口令派生 KEK 并包装随机 DEK，密钥和正文不得进入 argv、环境变量、日志或崩溃信息；密钥不可用时命令 fail closed，不允许明文后备。

本地对象绑定稳定 profile、normalized origin、device、learner generation、object kind 和 logical ID。存储使用受限根目录、拒绝链接和根逃逸、同目录原子替换、必要 durability 原语和跨进程 lease。损坏、未知版本、nonce 风险、profile 不匹配或未完成 journal 必须在读取正文前失败。

S5 增加 Linux Secret Service、macOS Keychain 和 Windows DPAPI，以及显式 `offline key migrate`。系统 backend 失效不能静默切换；迁移使用 durable journal，并保证每个崩溃边界至少一个 backend 仍可解密同一 DEK。

## CLI 行为

CLI 提供 `offline prepare`、`offline learn`、`offline status`、`offline sync` 和 `offline discard`；S5 增加系统 backend 选择和 key migrate。命令保持低颜色、中性提示和稳定非 TTY 文本，不输出答案、密钥材料、签名原文或完整上游响应。

`offline learn`只显示本地完整性验证通过且仍可开始的 Activity。它可以记录允许的帮助级别、答案和展示 observation，但不运行模型、评分器、自由问答、路线规划或 Evidence 接纳，也不在本地宣称 accepted。

operation 进入 queued 后不可修改。需要重答时必须显式丢弃并在恢复联网后取得新的服务端 submission。`logout`和`device forget-local`在存在不可安全处理的非终态队列、privacy purge 或 journal 时必须阻止远端吊销优先发生。

## 同步与幂等

`offline sync`按 device sequence 升序提交有界批次。请求级 schema、认证和大小在任何 item 处理前验证；业务结果按 item 独立事务处理。确定性 rejection 或 replay 后可继续，瞬态数据库或依赖失败停止当前批次并把后续项标记未处理，已提交项不回滚。

相同 `(device_id, operation_id)` 和 canonical operation hash 返回首次终态，不创建第二份 Inbox、Attempt、event、Assessment 或 Evidence。相同 operation 或 sequence 携带不同内容返回永久、机器可读冲突。`sync_request_id`只用于请求关联，不能覆盖 item 级幂等事实。

响应明确区分 archive、assessment 和 Evidence。存档成功不等于计分成功；accepted、provisional、pending evaluation、not eligible、not applicable、retryable、blocked、conflict 和 not processed 不能互相替代。只有持久化 ingest receipt 才能声明真实 aggregate version 和 event range。

## 期限、重复 Activity 与评估

所有期限、received time、Evidence winner、复习推进和 canonical 排序使用事务内数据库时间与 event sequence。设备 `occurred_at` 仅作为不可信展示值。

知识 head、Goal/Route 或策略变化，以及 eligible 期限过期时，服务端可以保存可验证 Attempt 供审计，但 Evidence eligibility 按稳定原因关闭，Mastery 和 Review 不被静默修改。privacy generation、redaction 或引用完整性失败优先拒绝正文回流并要求本地 purge。

同一 Activity/revision 只有一个数据库约束保护的 normal Evidence slot。在线 submit 与离线 ingest 在 Attempt 事务内竞争；首个合法提交成为 winner，其余 Attempt 保留审计但不能创建第二份正常 Evidence。调整设备时间、到达顺序重试或 sequence 缺号不能覆盖 winner。

Objective winner 使用冻结规则同步评估。Open winner 先原子存档并进入 pending evaluation，再由 transactional Outbox worker 使用冻结 Activity、Attempt、rubric 和 knowledge references 收敛。瞬态模型错误持久化重试，永久无效输出收敛为 provisional；内部完整性故障明确 degraded，不丢失或改写首次 ingest。

Open provisional Assessment 通过独立 offline assessment query 和 confirm/override/void decision 解决。Decision 和 Evidence 追加到 offline attempt aggregate，不依赖或修改当前 tutoring Session；不具 Evidence 资格的 Attempt 不得通过后续 decision 升级。

## 投影与查询

离线事件进入 Timeline、Node、Evidence、Mastery、Misconception 和 Review reducer，并携带来源 Session 和 device。Session 查询可以显示离线事实，但 SessionProjection 的 state、focus 和 route 不因离线 operation 改变。

projection schema、semantic fingerprint 和 event schema 必须显式版本化。历史 migration 不修改；兼容演进使用追加 migration、upcaster 或版本化 fingerprint。增量投影与从零重放必须得到相同语义结果和 Evidence winner。

operation status 合并 immutable ingest receipt 与可变化的 assessment/Evidence projection。异步 worker 不反写首次 Inbox payload/hash；响应丢失、进程重启和全量重放不能重复 Assessment、Decision 或 Evidence。

## Privacy

包、operation 和 profile 绑定 learner generation。privacy barrier 提交后旧 generation 正文立即不可读取或重新写入；各 owner 随后幂等 scrub 并验证，不能等待外部 sidecar 或离线设备才清除服务端活动正文。

服务端记录曾成功取得包的设备 possession。官方 CLI 在 exclusive lease 内 crypto-discard 受管对象和 key，验证不存在后使用版本化 challenge 幂等 ack。未 ack、失败或丢失设备保持 pending、failed 或 unknown，因此整体 erasure 可以长期保持 partial，不能错误宣称 verified。

设备 purge ack 只证明官方受管目录和 key backend 的处理结果，不承诺覆盖 OS 快照、用户副本、远程终端日志、取证残留或第三方备份。

## HTTP、认证与运行状态

OpenAPI 是 pack、sync、operation status、offline assessment 和 privacy purge 的公共合同。新 schema 使用 closed object/enums、规范 UUID/时间/大整数、明确大小限制、单一 scope 和现有 closed error envelope；CLI contract tests 防止 DTO 漂移。

所有 endpoint 使用现有 Bearer 认证、设备 ownership、scope、限流和审计。body 中的 device、origin、generation 或签名字段不能覆盖认证上下文。prepare 发布前和每个 sync item 事务内重新校验 credential epoch 与 revoked 状态。

readiness 分别报告 signer、Open evaluation worker 和服务端离线能力。Signer 缺失阻止新 prepare 但不破坏在线教学或已存档 status；模型不可用使 Open evaluation degraded，而不影响 Objective 同步。

## 验证合同

开发按 `docs/development/testing-strategy.md` 和分阶段计划执行。S1 至 S4 只运行具名、受影响 package、必要串行 PostgreSQL 和最小端到端门禁；实现未完整时不得反复运行全仓、完整 fault matrix、Compose、三平台或宽审计。

最终候选使用真实 PostgreSQL 验证事务原子性、幂等、sequence、竞争、重放和 privacy；使用至少两个隔离 CLI profile 验证断网、重启、部分同步、响应丢失、Objective/Open 和多设备场景；使用 Linux 原生环境验证 Secret Service、权限、锁、原子替换、迁移和 purge。macOS Keychain 与 Windows DPAPI 的对应原生验证作为外部 follow-up，未运行或 skip 时必须记录为 not-run 且不得标记为通过，但不阻塞本 change 归档。
