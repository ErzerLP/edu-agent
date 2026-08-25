# Outcome

在线 Go CLI 可以预取服务端授权的离线学习材料，在断网时完成受限学习并把不可变 client operation/observation 安全排队；恢复联网后由服务端逐项生成 canonical Learning Event，并分别报告操作存档结果与 Evidence 接纳结果。

# Scope

本 change 覆盖 Go CLI 离线准备、离线答题、本地队列与同步状态，服务端预签发 Activity、设备序号、同步上传与幂等裁决，以及过期知识 revision、多设备重复作答和响应丢失的明确行为。服务端 PostgreSQL、Learning Event Log、Inbox、Activity、Attempt、Assessment、Evidence 和投影继续作为权威来源。

# Non-goals

本 change 不实现离线模型推理、离线评分、离线 Evidence/Mastery/Review 更新、离线路线生成、自由问答、知识导入、Nocturne 写入、Fast Note Sync、MCP、移动端或多用户同步。客户端不生成 canonical event，也不使用设备时钟决定事件顺序、复习日期或证据胜负。

# Acceptance examples

- A1：`offline prepare`在有效在线Session中默认签发5项、最多20项且总计不超过8 MiB的Activity包；相同operation/hash重放返回同一ID、内容、签名和过期时间。
- A2：每项Activity冻结Session/Goal/Route/Knowledge/node引用、prompt、rubric、帮助规则和策略版本；authorization、pack、prepare response与manifest使用独立Ed25519域，manifest链无缺号/回滚/分叉，篡改或跨origin/device/generation复制失败。
- A3：离线项使用独立`offline_attempt` aggregate和`expected_version=0`；同步可以更新Evidence、Mastery、Misconception和Review，但不推进、切换或完成在线tutoring Session。
- A4：Activity、knowledge slice、答案、operation和receipt全部AEAD加密落盘；Windows DPAPI、macOS Keychain或Linux Secret Service优先，密钥服务不可用时要求隐藏口令，任何平台都不降级为明文。
- A5：密封文件使用固定根句柄、原子替换、fsync、跨进程锁和symlink/reparse拒绝；崩溃恢复不会复用nonce、operation ID或device sequence。
- A6：每设备`device_seq`单调且不可复用，但允许乱序到达和暂时缺号；同sequence不同operation返回`device_sequence_conflict`，canonical顺序仍只由`event_seq`决定。
- A7：`offline learn`只展示完整性验证通过且未过期的签发项，只记录白名单Attempt/observation，不提供离线模型、评分、自由问答、路线推进或Evidence声明。
- A8：`offline sync`每批最多50项并按item独立事务处理；确定性拒绝后继续，瞬态失败停止并把后续标记`not_processed`，已提交项不回滚。
- A9：同一operation在post-commit响应丢失后以相同字节重放，返回首次终态且数据库只有一个Inbox、sequence claim、Attempt、event batch、Assessment和有效Evidence。
- A10：相同`(device_id, operation_id)`不同body或sequence永久返回`idempotency_conflict`，不能用新device sequence覆盖已存档operation。
- A11：Objective Activity由服务端冻结规则同步评估；Open Activity先存档Attempt并返回`pending_evaluation`，再由事务Outbox和两阶段proposal/apply worker收敛，模型故障不丢失Attempt。
- A12：知识head推进、Activity过期或策略变旧时Attempt可以审计存档，但默认provisional且不改变Mastery/Review；privacy redaction或generation失配则拒绝旧正文复活并要求本地purge。
- A13：同一Activity的多设备Attempt全部保留审计；按数据库成功提交和event sequence确定的首份进入正常接纳，后续默认provisional且数据库约束保证不产生第二份有效Evidence。
- A14：未来或过去的`occurred_at`只作为untrusted展示值；estimated active time、retained间隔、Review推进和多设备winner不受设备时钟影响。
- A15：每项同步结果使用archived/retryable/blocked/conflict/not-processed closed oneOf并分别返回archive和Evidence语义；无receipt的结果不能伪造event range，CLI不会把pending/provisional/not-eligible显示为accepted。
- A16：存在非terminal离线数据时，`logout`和`device forget-local`在远端吊销前返回`offline_queue_pending`；用户必须先同步或明确`offline discard --all`。
- A17：全局privacy barrier立即关闭旧generation读写并使服务端离线正文不可返回；各owner随后幂等scrub并经VerifyRedacted确认，同时建立`offline_device_cache`summary和逐设备child receipt，任一设备未ack时总ErasureStatus保持`partial`。
- A18：设备恢复联网后通过专用端点取得可重算的版本化purge challenge；官方CLI crypto-discard后幂等ack，失败后GET原子生成新revision供重试，丢失或吊销设备保持unknown且不被宣称verified。
- A19：Learning Event增量投影和从零重放对offline Activity/Attempt/Assessment/Evidence产生相同fingerprint、Mastery和Review，但SessionProjection状态保持不变。
- A20：OpenAPI为PSK pairing、pack、sync、operation status、offline assessment query/decision和privacy purge提供closed schema、单一scope、大小限制、稳定枚举及真实HTTP错误；CLI contract test拒绝漂移。
- A21：真实PostgreSQL逐写点故障注入证明sequence、Inbox、typed records、event clock、Outbox、projection和privacy receipt原子回滚；多设备并发只有一个normal evidence slot winner。
- A22：真实双CLI黑盒覆盖在线准备、完全断网答题、乱序/缺号同步、双设备同Activity、过期revision、Objective/Open评估、answer-revealed、response loss和服务重启。
- A23：Linux原生证据同时验证真实Secret Service和Argon2id口令后备，macOS验证CGO禁用的Keychain，Windows验证DPAPI；三平台还验证权限/ACL、文件锁、原子替换、隐藏输入和crypto-discard，交叉编译不能替代。
- A24：Nocturne、模型或无关后续集成故障不破坏已存档learning authority；模型瞬态不可用使Open评估保持pending-retry/degraded，永久schema错误收敛为provisional，Objective同步保持明确可用。
- A25：prepare使用带lease的幂等claim、事务外模型调用和最终repeatable-read读写发布事务；模型响应、HTTP响应或本地pack发布任一点丢失都不会重复模型artifact、Activity、sequence reservation或submission。
- A26：AwaitingResponse缓存保留同一canonical Activity ID；只有online submit与offline ingest在Attempt事务中竞争claim。offline先赢后，online落败Attempt仍走Feedback/acknowledge但永久`evidence_eligibility=false`；worker只沿用winner，历史多Evidence迁移fail closed。
- A27：`offline_activity_skipped`只生成审计event且不创建Attempt/Evidence；同设备同Activity最多一个active submission，queued后重答必须联网重新签发。
- A28：首次配对不在HTTP发送pairing secret，而以lookup/client nonce/request MAC和AEAD响应建立trust root；prepare返回从trusted revision到current的完整manifest chain，新工件只由current active key签名，跨两次rotation仍可验证。
- A29：Open评估从pending收敛后，Inbox的首次ingest JSON/hash保持字节不变，而operation status与sync replay显示新的Assessment/Evidence状态；全量重放结果一致。
- A30：offline event携带`parent_session_id`并进入来源Session timeline和estimated-time输入，但SessionProjection state/focus/route不变；Evidence reducer按`accepted_event_seq`确定顺序。
- A31：prepare最终发布和每个sync item在事务内重新校验device credential epoch；与设备撤销并发时，已提交item保留，未处理item稳定返回`device_revoked`。
- A32：`offline key migrate`采用durable two-phase journal并保持stable profile UUID；object header不绑定backend/KDF，因此无需重密封既有对象，在新包装创建、read-back、切换和旧key删除各边界崩溃后至少一个backend仍可解密。
- A33：device possession ledger不因TTL、terminal或token轮换消失；只有同设备认证purge ack可收敛，故长期离线设备会真实阻止`verified`。
- A34：逐写点PostgreSQL矩阵明确覆盖prepare claim/publish、reservation/claim、Inbox、Activity/reference、Attempt/payload、event payload/clock、evidence claim/eligibility、Assessment/Decision/Evidence、Outbox/job、projection/checkpoint和privacy child receipt。
- A35：fresh离线Assessment为provisional时可通过独立online query和`offline assessment show|confirm|override|void`解决；Decision追加到offline_attempt且不改变Session，ineligible Attempt不会出现在待处理列表。
- A36：所有进入JCS签名/hash且可能超过2^53的sequence、generation和revision使用规范`uint63-decimal`字符串；2^53边界与MaxInt64的跨语言golden vectors一致。
- A37：签名可信的deterministic rejection写入不含正文的`OfflineOperationRejected` event并返回真实aggregate version/event range；blocked、retryable和untrusted authorization不产生伪receipt。

# Constraints and invariants

离线客户端只持有服务端预签发且内容冻结的 Activity 与白名单 operation/observation。每项 operation 固定携带 `operation_id`、认证设备、`device_seq`、`aggregate_id`、`expected_version`、`activity_revision`、不可信 `occurred_at` 和 payload schema version。服务端只使用可信 `received_at` 与 `event_seq` 生成权威顺序和 reducer 输入。

# Decisions

- 复用现有 `(device_id, operation_id)` Inbox、事务性 Learning Event Log、typed records、projection 和 Outbox，不建立第二套业务权威。
- 同步批次不承诺整体原子；每个 operation 独立事务提交，已提交项可安全重放，未处理项可稍后继续。
- 离线模式不复制教学状态机、评分器或 Evidence acceptance policy；需要当前权威状态或模型的动作必须联网。
- 同一预签发 Activity 的多设备 Attempt 全部保留审计；按服务端成功提交顺序，首份进入正常 Evidence 接纳，后续默认 provisional 且不得创建第二份有效 Evidence，不使用 `occurred_at` 决定胜负。
- `device_seq` 在每个设备内单调且不可复用，但允许乱序到达和暂时缺号；prepare 事务为每个 submission 预留 operation ID、submission ID 和 sequence，客户端不得自造或改写，未使用项自然留下缺号而不阻塞后续同步。
- `prepare-offline` 生成有界 Activity 包，默认 5 项，并同时受数量、总字节和有效期上限约束；同步仍逐项裁决，不承诺整个包成功。
- canonical offline Activity 可在多个设备间共享，但每台设备得到独立签名 submission authorization。签名使用版本化 Ed25519，并分别冻结可正常接纳的 `eligible_until` 与只允许审计存档的 `archive_until`。
- 离线 Activity 和答案正文始终加密落盘。随机数据密钥优先由 Windows DPAPI、macOS Keychain 或 Linux Secret Service 保护；系统密钥服务不可用时使用用户口令后备，绝不自动降级为明文。
- 全局隐私清除的barrier会立即关闭旧generation读写并使正文不可返回，各owner随后幂等scrub并验证；只要任一相关设备尚未完成本地purge ack，总ErasureStatus保持`partial`，丢失设备不被宣称`verified`。
- `offline_activity`是可跨设备共享的签发内容实体；每台设备的提交使用独立`offline_attempt` aggregate。同步可以形成Evidence并更新Mastery、Misconception和Review，但不直接推进、切换或完成在线tutoring Session。

# Open questions

无。

# Verification expectations

使用真实 PostgreSQL 验证 Inbox 重放、设备序号、expected-version 竞争、过期 revision、多设备同 Activity、逐项事务回滚和 canonical event/Evidence 不重复；使用真实 CLI 进程验证断网、崩溃恢复、部分同步、响应丢失和稳定结果展示。Linux、macOS、Windows 原生测试验证本地队列权限、链接攻击、终端行为和选定的正文保护方案。
