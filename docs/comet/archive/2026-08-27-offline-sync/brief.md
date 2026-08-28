# Outcome

在线 Go CLI 可以在线准备服务端签发的离线 Activity，在断网时安全完成并排队不可变 operation，恢复联网后由服务端生成 canonical Learning Event，并分别展示操作存档结果和 Evidence 状态。

# Scope

本 change 保留完整离线能力，但按五个顺序阶段交付：S1 权威基础、S2 服务端 Objective 闭环、S3 安全 CLI 闭环、S4 Open Assessment 与多设备竞争、S5 隐私和平台加固。每个阶段都有独立可运行结果和退出标准，最终候选只在五个阶段全部完成后形成。

服务端 PostgreSQL、Learning Event Log、Inbox、Activity、Attempt、Assessment、Evidence 和投影继续是权威来源。客户端只保存服务端签发的 Activity、不可变 operation 和必要 receipt，不生成 canonical event、评分、Evidence、Mastery、Review 或 tutoring state。

具体阶段、当前进度和测试边界见 `docs/design/offline-sync-delivery-plan.md`；字段级协议和安全格式见 `docs/design/offline-sync-technical-reference.md`。

# Non-goals

本 change 不提供离线模型推理、离线自由问答、离线路线生成、离线知识导入、Nocturne 写入、Fast Note Sync、MCP、Web、移动端或多用户同步。设备时间不决定 canonical 顺序、复习日期、Evidence 胜负或学习时长。

本 change 不要求在早期阶段提前完成后续阶段的系统 key backend、Open Assessment worker 或设备隐私回执。S5 保留 Linux Secret Service、macOS Keychain 和 Windows DPAPI 的生产实现，但本 change 的原生候选门禁只要求 Linux；macOS/Windows 原生权限、锁、迁移和 purge 验证作为明确的外部 follow-up，未运行时保持 not-run 且不标记为通过。

# Acceptance examples

- A1：S1 完成后，离线领域、事件、JCS、migration 和 transaction port 可编译并通过受影响 package；learning store 不直接查询 tutoring 或 knowledge 私有表。
- A2：投影或事件 schema 演进使用追加 migration、版本化 fingerprint 或 upcaster；checksum-protected 历史 migration 不因当前结构变化被改写。
- A3：`offline prepare`为有效 Session 签发有界 Activity 包；相同 operation/hash 重放返回相同持久化结果，不重复模型工件、Activity、submission 或设备序号。
- A4：`offline learn`只显示完整性验证通过且未过期的签发项，不执行模型、评分、自由问答、路线推进或 Evidence 声明。
- A5：CLI 使用加密队列保存 Activity、答案、operation 和 receipt；密钥不可用时 fail closed，任何平台都不自动降级为明文。
- A6：`offline sync`按 item 独立事务处理并分别返回存档与 Evidence 语义；瞬态失败不回滚已提交项，未处理项可以用相同 canonical bytes 重试。
- A7：相同 operation 重放不重复 Inbox、Attempt、event、Assessment 或 Evidence；相同 operation 或 device sequence 携带不同内容返回稳定冲突。
- A8：过期、stale knowledge/policy/context 或 answer-revealed 的 Attempt 可以按规则审计，但不能静默产生正常 Evidence；privacy generation 或引用完整性失败不复活旧正文。
- A9：同一 Activity 的多设备和在线/离线 Attempt 均保留审计，但数据库约束保证只有一个 normal Evidence winner，设备时钟不能改变胜负。
- A10：Objective Activity 由服务端确定性评估；Open Activity 由持久化 worker 收敛为 accepted、provisional、not-eligible 或明确降级，模型故障不丢失 Attempt。
- A11：离线事件可以更新 Timeline、Evidence、Mastery、Misconception 和 Review，但增量与全量重放结果一致，且不推进或改写 tutoring SessionProjection。
- A12：`offline prepare/learn/status/sync/discard`形成完整 CLI 闭环；非 TTY 输出稳定，不打印答案、密钥材料、签名原文或完整上游响应。
- A13：全局 privacy barrier 立即关闭旧 generation 正文；设备 possession、crypto-discard 和幂等 ack 如实收敛，未 ack 或丢失设备保持 partial/unknown。
- A14：最终阶段验证 passphrase 后备、Linux Secret Service 和 key migration；macOS Keychain 与 Windows DPAPI 保留生产适配，其原生权限、锁、迁移和 purge 证据作为外部 follow-up，不阻塞本 change 归档；token 轮换不使既有队列不可读。
- A15：OpenAPI、CLI DTO 和服务端 handler 对 pack、sync、status、assessment 和 privacy 使用 closed schema、稳定枚举、大小限制、scope 和真实错误响应。
- A16：真实 PostgreSQL 证明 sequence、Inbox、typed records、event clock、Outbox、projection 和 privacy receipt 在事务失败时不留下部分事实。
- A17：真实 CLI 黑盒覆盖在线准备、完全断网答题、重启、部分同步、响应丢失、Objective/Open 结果和双设备竞争。
- A18：开发阶段遵循分层测试和证据复用；全仓、完整 PostgreSQL 故障矩阵、race、Compose、Linux 原生证据和独立 Verifier 只在对应稳定阶段或最终候选运行；macOS/Windows 原生证据未运行时记录为外部 follow-up，不标记为通过。

# Constraints and invariants

离线 operation 固定绑定服务端签发的 device、learner generation、Activity revision、submission、operation ID、device sequence 和 payload schema。客户端不能自造或重新分配这些权威标识。

所有 canonical 顺序、期限裁决、Evidence winner 和 reducer 输入使用服务端数据库时间与 event sequence。`occurred_at` 只作为不可信审计或展示信息。

离线实现必须遵守模块所有权：learning 不直接读取 tutoring/knowledge 私有表，跨 owner 信息通过调用方定义的 port、公开能力或 caller-owned transaction adapter 提供。

# Decisions

- 保留最终生产级离线目标，但按 S1 至 S5 垂直阶段推进，避免一次实现和验证全部子系统。
- S3 先以跨平台隐藏口令加 AEAD 提供无明文的可运行闭环；系统 key backend 和 backend migration 在 S5 加固，不阻塞早期端到端验证。
- 字段级签名、容器和状态矩阵保留在技术参考，不再把每个内部字段单独提升为正式验收项。
- 当前未提交的服务端领域、store 和 migration 工作归入 S1，不回滚；先修复 owner 越界和投影版本兼容，再进入 HTTP 或 CLI。
- 一个阶段只运行适用的 L0 至 L4 检查；完整 L5 门禁和 Builder handoff 在 S5 后运行一次。
- Linux 是本 change 的原生平台候选门禁；macOS Keychain 与 Windows DPAPI 的原生权限、锁、迁移和 purge 验证明确延期为外部 follow-up。延期项必须保持 not-run，不得伪装为 passed，也不阻塞当前 Linux 候选归档。

# Open questions

无。

# Verification expectations

每个阶段必须先通过具名回归和受影响 package，再运行一次适用的垂直契约。PostgreSQL 使用真实隔离数据库并串行运行；未配置数据库时明确记录未运行。最终候选运行完整 PostgreSQL、重放、并发、黑盒、race/vet/build、OpenAPI 和 Linux 原生证据，由新的只读 Verifier 逐项验收。macOS/Windows 原生集成证据作为外部 follow-up 保持 not-run，不得标记为通过，但不阻塞本 change 归档。
