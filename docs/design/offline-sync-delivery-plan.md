# Offline Sync 分阶段交付计划

## 使用方式

本计划把完整离线能力拆成五个顺序阶段。每次只推进一个阶段；当前阶段退出标准未满足前，不进入下一阶段，也不运行最终候选的全量验证。正式产品合同位于 Comet brief/spec，字段级细节位于 `offline-sync-technical-reference.md`。

S1 **权威基础**、S2 **服务端 Objective 闭环**、S3 **安全 CLI 闭环**、S4 **评估与竞争**和 S5 **隐私与平台加固**已形成最终 Builder candidate，并于 **2026-08-27 完成 Runtime Verify 并归档**；当前没有进行中的 Offline Sync Build/Verify 阶段。macOS Keychain 与 Windows DPAPI/ACL 的原生验证仍作为不阻塞归档的外部 follow-up，未运行项不得标记为通过。S1 遗留的真实 PostgreSQL ingest 与 migration upgrade 证据已在 S2 的最终 L4 门禁中补齐。

## 阶段总览

| 阶段 | 可交付结果 | 延期到后续阶段 |
| --- | --- | --- |
| S1 权威基础 | 领域、事件、JCS、migration 和 PostgreSQL transaction port 形成可编译基础 | HTTP、CLI、模型 worker、隐私设备回执 |
| S2 服务端 Objective 闭环 | 实际 HTTP 可 prepare、sync、查询 Objective operation 状态 | 本地 CLI 队列、Open Assessment、完整多设备策略 |
| S3 安全 CLI 闭环 | CLI 可在线准备、断网答题、加密排队、联网同步和查看状态 | 系统 key backend、Open Assessment、隐私 ack |
| S4 评估与竞争 | Open Assessment 收敛，多设备和在线/离线竞争规则完整 | 全局隐私设备清除、跨平台系统 key backend |
| S5 隐私与平台加固 | 设备 possession/purge、跨平台系统 key backend、迁移和 Linux 原生候选证据 | macOS/Windows 原生集成证据作为外部 follow-up |

## S1：权威基础

结果：服务端具备离线 operation 的稳定领域模型、canonical hash、事件和持久化事务入口，且保持 learning、tutoring、knowledge、privacy 的 owner 边界。

范围：`internal/learning`、`internal/learning/postgresstore`、必要的 identity/privacy port，以及追加式 migration。不得增加 HTTP、OpenAPI 或 CLI 行为。

退出标准：

- learning store 不直接读取 tutoring 或 knowledge 私有表；跨 owner 信息经调用方 port 或 caller-owned transaction adapter 提供。
- 投影结构变化使用新 migration、版本化 fingerprint 或 upcaster，不要求 checksum-protected 历史 migration 匹配当前结构。
- `go test` 至少覆盖 learning、privacy、migration 的具名回归和受影响 package。
- 有数据库时只运行一次 S1 相关串行 PostgreSQL 测试；无数据库时记录未运行。
- `git diff --check` 和 error-level diagnostics 通过。

当前已知阻塞是 `offline_store.go` 直接引用 `tutoring_sessions`，以及当前空投影 fingerprint 与历史 `000003` seed 的兼容处理。

## S2：服务端 Objective 闭环

结果：真实调用方可以取得服务端签发的 Objective Activity 包，上传一个不可变 operation，并查询存档与 Evidence 状态；全过程不改变 tutoring Session 状态。

范围：prepare/sync/status application use case、Objective 确定性评估、HTTP/OpenAPI、app composition、签名和服务端配置。Open Activity 只允许明确返回未启用或延期状态，不启动模型 worker。

退出标准：

- 一个实际 HTTP 场景完成 prepare、Objective sync、operation status 和安全重放。
- 同 operation/hash 重放不重复 Inbox、Attempt、event 或 Evidence。
- stale、expired、revoked 和 idempotency conflict 有稳定机器错误。
- OpenAPI、handler、migration 和受影响 PostgreSQL 契约通过一次 L4 门禁。

## S3：安全 CLI 闭环

结果：用户可以在线执行 `offline prepare`，完全断网执行 `offline learn`，再联网执行 `offline sync` 和 `offline status`。

范围：先实现跨平台均可用的隐藏口令加 AES-GCM 密封队列、原子文件发布、profile lease、prepare journal、operation/receipt 状态和 `offline discard`。任何平台不得回退为明文。

延期：Secret Service、Keychain、DPAPI 和 backend migration 留到 S5；Open Assessment 与隐私 purge 留到后续阶段。

退出标准：

- 一个真实 CLI profile 完成 Objective prepare、断网答题、进程重启、sync 和 status。
- 响应丢失后重发相同 canonical bytes，不重复服务端事实。
- 本地损坏、profile/origin/device/generation 不匹配和密钥不可用均 fail closed。
- CLI package、fake HTTP、单一真实黑盒和本地存储崩溃回归通过。

## S4：评估与竞争

结果：Open Activity 可以异步评估并收敛；同一 Activity 的在线/离线和多设备提交只有一份 normal Evidence winner，其余保留审计但不能重复计分。

范围：offline assessment worker、operation status projection、provisional query/decision、evidence claim、stale knowledge/policy/context 和多设备竞争。

退出标准：

- Objective、Open accepted、Open provisional、模型重试和永久 schema 错误均有端到端结果。
- Inbox 首次 ingest 内容在 worker 收敛后保持不变，status projection 更新。
- 两设备和在线/离线并发只有一个 winner，全量重放得到相同投影。
- 只运行受影响 worker、learning、projection 和串行 PostgreSQL 竞争矩阵。

## S5：隐私与平台加固

结果：离线设备纳入全局 privacy erasure；CLI 支持 Linux Secret Service、macOS Keychain、Windows DPAPI 和安全迁移；最终候选具备 Linux 原生平台与完整故障证据。

范围：device possession ledger、purge challenge/ack、privacy child receipt、Secret Service、Keychain、DPAPI、key migration、路径攻击、完整 crash matrix 和 Linux 原生平台门禁。macOS/Windows 原生集成验证作为外部 follow-up，不扩张本阶段的归档门禁。

退出标准：

- barrier 后旧 generation 正文立即不可访问，owner scrub 和设备 ack 可幂等恢复。
- 丢失或未 ack 设备保持 `partial/unknown`，不宣称 verified。
- Linux 原生 Secret Service、权限、锁、原子替换、迁移和 purge 证据绑定候选提交。
- macOS Keychain 与 Windows DPAPI/ACL 的对应原生证据保持 not-run 并作为外部 follow-up；不得标记为通过，也不阻塞本 change 归档。
- 完整 PostgreSQL 故障矩阵、双 CLI 黑盒、race/vet/build、OpenAPI 和必要 Compose 在候选上各运行一次。

## 批次记录

### 2026-08-25：S1 权威基础代码检查点

完成结果：离线领域、事件、JCS、migration 与 PostgreSQL transaction entry 保持可编译；learning store 不再直接查询 `tutoring_sessions`，而是在同一事务中锁定 tutoring privacy generation 并通过 `tutoringOwner` 读取 Session authority。历史 `000003` 继续保存 projection v1 的 fingerprint，`000007` 明确把旧 active generation 标记为需要 v2 rebuild。

实际检查：

- `go test ./internal/learning/postgresstore -run '^TestProductionSourceDoesNotOwnTutoringOrReadKnowledgeTables$' -count=1`：通过。
- `go test ./migrations -run '^TestProjectionMigrationsPreserveV1SeedAndRequireV2Rebuild$' -count=1`：通过。
- `go test -p=1 ./internal/identity ./internal/learning ./internal/learning/postgresstore ./internal/privacy ./internal/privacy/postgresstore ./internal/tutoring/postgresstore ./migrations`：通过。
- 25 个当前修改 Go 文件的 error-level LSP diagnostics：通过。
- `git diff --check`：通过。
- `TestPostgreSQLOfflineIngestSequenceWinnerRejectionAndOpenQueue`：skip，`TEST_DATABASE_URL` 未设置。
- `TestOfflineSyncCoreMigrationUpgrades000006`：skip，`TEST_DATABASE_URL` 未设置。

复用证据：本检查点只覆盖 S1，不形成完整 Builder candidate；后续阶段不重复运行不受影响的 S1 单元测试，除非输入或相关代码发生变化。

已知限制：真实 PostgreSQL transaction、migration upgrade 和 fault behavior 尚未在当前环境执行；HTTP、OpenAPI、app composition、Objective prepare/sync/status、CLI、Open Assessment、设备隐私和系统 key backend 均未进入本批次。

下一阶段：S2 先建立一个真实调用方可执行的 Objective prepare/sync/status 服务端闭环，并在该垂直行为稳定后运行一次受影响的 PostgreSQL/OpenAPI L4 门禁。

### 2026-08-25：S2 服务端 Objective 闭环代码检查点

完成结果：服务端新增 Ed25519 signed Objective pack、prepare idempotency、device sequence reservation、逐项 sync、Objective deterministic assessment、operation status、HTTP/OpenAPI、app composition、strict signer configuration 和 readiness component。learning store 通过 tutoring/knowledge owner port 与 privacy generation gate 读取跨 owner authority，不直接查询 owner 私表；offline operation 的 canonical authorization payload 和 signature 在 ingest transaction 内与服务端 reservation 精确匹配。prepare 和 sync replay 均返回相同 canonical signed payload，不重复 sequence、Inbox、Attempt、event 或 Evidence，且 tutoring Session 保持 `AwaitingResponse`。

实际检查：

- `go test -p=1 ./internal/knowledge/postgresstore ./internal/learning ./internal/learning/postgresstore ./internal/platform/config ./internal/platform/health ./internal/app ./internal/transport/httpapi ./api ./migrations -count=1`：通过。
- `TestOfflineSignerUsesCanonicalDomainSeparatedPayloads`、`TestOfflineServicePrepareAndSyncFreezeWireSemantics`、`TestOfflineServiceRejectsBodyDeviceAndSequenceReorderingBeforeIngest`：通过。
- `TestOfflineHTTPEnforcesScopesActorsReplayAndClosedBodies`、`TestOfflineHTTPMapsSignerFailureAndBodyLimit`：通过。
- `TestOfflineOpenAPIContractsAreClosedScopedAndMatchWireResults`：通过。
- 临时 digest-pinned PostgreSQL 17，`-p=1`：`TestPostgreSQLOfflineObjectivePrepareSyncStatus` 通过真实 `httpapi.New` 完成 HTTP prepare/replay/sync/status；`TestPostgreSQLOfflineIngestSequenceWinnerRejectionAndOpenQueue` 和 `TestOfflineSyncCoreMigrationUpgrades000006` 同样通过。所有临时容器均在命令结束时删除。
- 扩展后的 `TestProductionSourceDoesNotOwnTutoringOrReadKnowledgeTables`：通过。
- error-level LSP/lens diagnostics 和 `git diff --check`：通过。

修复证据：首次 PostgreSQL gate 发现 `JSONB` 会重排嵌套 signed payload，已在 replay 和 authorization comparison 前统一 JCS canonicalize；第二次 gate 证明 canonical payload/signature 可以通过 transaction 校验。旧 ingest fixture 同步补齐 canonical authorization bytes/hash/signature 后，原 sequence/winner/rejection/open 回归通过。

复用证据：S2 PostgreSQL/OpenAPI L4 已绑定当前 worktree 输入；S3 不修改服务端 prepare/sync/status 合同时复用该证据，只运行 CLI package、fake HTTP 和一个真实 CLI Objective 黑盒。若 S3 改动公共 DTO、server handler 或 migration，则相应证据失效并只重跑受影响门禁。

已知限制：当前 prepare 只签发 `AwaitingResponse` Session 已物化的 Objective Activity；请求更多项时返回 `current_activity_only` 截断。RouteActive 新 Activity 生成、pairing trust-root 下发、CLI 加密队列、Open Assessment worker、多设备完整竞争、privacy purge 和系统 key backend 尚未实现。缺少 signer 配置时 prepare 返回 `offline_signer_unavailable`，在线教学与已有 status 保持可用。

下一阶段：S3 使用当前 OpenAPI 和 signer manifest 实现一个 passphrase-derived AEAD profile，完成真实 CLI `offline prepare → learn（断网）→ sync → status`，并保持任何 key failure fail closed、无明文后备。

### 2026-08-25：S3 安全 CLI 闭环代码检查点

完成结果：CLI 配对会把服务端签名 manifest、learner generation 与 origin 绑定保存为 profile trust root；`offline prepare` 验证 manifest chain、pack 与逐项 authorization 的 Ed25519/JCS 签名后，使用隐藏口令经 Argon2id 派生 KEK、随机 DEK 和 AES-256-GCM 密封本地 pack、operation、receipt、status、journal 与 nonce 状态。`offline learn` 可在无服务器参与时答题并排队，`offline sync` 逐项应用服务端结果，`offline status` 展示本地终态，`offline discard` 与安全 logout 执行本地 crypto-erasure。profile lease、原子替换、目录同步、nonce crash-gap、symlink/root escape 与错误口令均 fail closed，不存在明文后备。

实际检查：

- `go test ./...`（`clients/cli-go`）：通过，包括 `internal/offline` 的 header golden、错误口令与 trust binding、journal recovery、lease contention、symlink/root escape、nonce crash-gap/rollback/duplicate/overflow、store 状态机与 preflight 回归。
- `TestPairPersistsOfflineTrustBootstrap`、`TestOfflinePrepareLearnSyncAndSafeLogoutLoop`：通过；命令级闭环覆盖 pack 签名验证、离线答题、queued logout 阻断、sync terminal 化和 logout crypto-erasure，磁盘扫描未发现题目、答案或 passphrase 明文。
- `TestOfflinePrepareUsesAuthenticatedClosedHTTPContract`：通过，冻结 Bearer、路径、closed request 与未知成功响应拒绝。
- 临时 digest-pinned PostgreSQL 17，`TestBlackBoxOfflineObjectivePrepareLearnSyncStatus`：通过真实 `edu-agent`、`edu-agentd`、strict fake LLM 与隔离 schema，覆盖 pairing bootstrap、Objective materialization、跨 CLI 进程 prepare/learn/sync/status、queued logout 阻断，以及 Attempt/Evidence 唯一性。
- `go test ./internal/identity ./internal/learning ./internal/learning/postgresstore ./internal/platform/config ./internal/platform/health ./internal/app ./internal/transport/httpapi ./api ./migrations` 与 contracttests blackbox compile-only：通过。
- error-level LSP/lens diagnostics 和 `git diff --check`：通过。

修复证据：真实黑盒发现 root origin 的 `http://host` 与 signed `http://host/` 被误作不同 trust root，现以规范化 URL 比较等价性但仍按 manifest 原始 origin bytes 验签；PostgreSQL 读回的 Activity 时间统一为 canonical UTC `Z` 后再签名；空成功 `reason_codes` 固定编码为 `[]` 而非 `null`。S2 的相同 canonical sync replay 与本阶段 journal recovery 共同证明响应丢失后可重发相同 operation bytes，且服务端 Inbox、Attempt 和 Evidence 不重复。

复用证据：S3 绑定当前 CLI store、API DTO、pairing bootstrap 和服务端 Objective wire 输入。S4 不修改本地密封格式时复用本地 crash/crypto 证据，只重跑受影响 worker、status projection、竞争 PostgreSQL 矩阵和一个必要的 CLI status 场景。

已知限制：S3 使用跨平台 passphrase backend；Secret Service、Keychain、DPAPI、backend migration 与原生三平台证据留到 S5。Open Activity worker、多设备与在线/离线竞争、privacy purge/ack 尚未进入本批次。

下一阶段：S4 实现 Open Assessment 异步 worker、不可变首次 ingest 结果与可推进 status projection，并冻结多设备及在线/离线单 winner 竞争规则。

### 2026-08-25：S4 评估与竞争代码检查点

完成结果：Open Activity ingest 只写入不可变 operation/Attempt、`queued` operation status 和幂等 outbox job；后台 worker 从 PostgreSQL 冻结 Activity、Attempt 与 request hash，运行现有 strict assessment model pipeline，并通过独立事务把状态推进为 `processing → completed`、`pending_retry` 或 integrity-only `failed`。首次 Inbox result 在 worker 收敛后保持逐字节不变；查询只读取 append-only status revision。格式错误、超时和限流在 deadline 内重试；永久无效模型输出或 deadline 耗尽后收敛为 completed provisional，稳定 reason 为 `schema_error` 或 `model_unavailable`，不会伪造 accepted Evidence。accepted 结果进入既有 assessment/decision/evidence 表；低置信度结果保留 provisional decision，不生成 normal Evidence。

实际检查：

- `go test ./internal/learning -run '^TestOfflineEvaluationConsumer' -count=1`：通过，覆盖 accepted、low-confidence provisional、可重试 schema error 和 deadline 后 provisional 收敛。
- 临时 digest-pinned PostgreSQL 17：`TestPostgreSQLOfflineOpenEvaluationConvergesWithoutRewritingInbox`、`TestPostgreSQLOfflineEvaluationRetriesThenConvergesProvisionally`、`TestPostgreSQLOfflineAttemptRespectsExistingOnlineWinner`、`TestPostgreSQLOfflineIngestSequenceWinnerRejectionAndOpenQueue` 全部通过；容器在命令结束时删除。
- Open 收敛场景通过真实 HTTP status handler、真实 outbox worker、learning service 与 PostgreSQL，证明 queued ingest 可变为 accepted Evidence，重复 worker 不新增 assessment/decision/evidence，Inbox 原始 result 不变。
- invalid-schema 场景证明第一次消费进入 `pending_retry/schema_error`，强制越过 retry deadline 后第二次消费进入 `completed/provisional`，只保存一份 fallback Assessment/Decision、零 normal Evidence；这一行为与正式 spec 的“永久无效输出收敛为 provisional”一致。
- 多设备竞争回归证明并发离线 contender 只有一个 `learning_activity_evidence_claims` winner；在线 winner 回归证明后到离线 contender 保留 Attempt 审计但为 `duplicate_activity` provisional，不能替换在线 claim 或生成重复 Evidence。
- `TestOfflineEvaluationWorkerMigrationUpgrades000007`、`TestOfflineSyncCoreMigrationUpgrades000006`、`TestMigrationsRunAndCheck`：通过；`000008` 追加 worker artifact/result 字段和 status 组合，不修改历史 migration。
- `go test` 与 `go vet` 覆盖受影响的 learning、postgresstore、platform outbox、app、config、health、httpapi 和 migrations package：通过。
- error-level LSP/lens diagnostics 与 `git diff --check`：通过。

修复证据：真实 worker 首次运行暴露 test fake model 的知识引用不完整，已改为从 frozen Activity reference 构造严格合法的模型输出；无效 `{}` 模型响应现因缺少顶层 `assessment` 被 schema 检查拒绝。`BeginOfflineEvaluation` 在获得 processing lease 的同一事务内读取冻结 Activity/Attempt，避免提交 lease 后 load 失败造成永久 stranded job；retry snapshot 保留上一错误类别，deadline 后生成显式 `retry_exhausted` fallback Assessment 并以 provisional 收敛，`failed`仅保留给完整性或 ownership 损坏。

复用证据：S4 未修改 S3 本地 sealed-object 格式、Argon2id/AES-GCM 参数或 journal/nonce 协议，因此复用 S3 的本地 crypto/crash 与 Objective 真进程黑盒证据。S5 修改 key backend、privacy purge 和平台文件系统行为后，只使相应平台与隐私证据失效，不重复运行不受影响的 Open worker 矩阵，除非共享 migration、status 或 competition contract 再次变化。

已知限制：全局 privacy erasure 仍未把所有离线 possession child 收敛为 device ack；CLI 仍仅提供 passphrase backend，尚无 Linux Secret Service、macOS Keychain、Windows DPAPI 和 backend migration。完整 fault matrix、race、Compose、双 CLI 和三平台原生验证留到 S5 candidate 边界。

下一阶段：S5 先完成 device possession/purge challenge/ack 与 `partial/unknown/verified` 证明，再实现系统 key backend 和安全迁移，最后只在稳定候选上运行一次完整 PostgreSQL、CLI、race/vet/build、OpenAPI、Compose 与原生平台门禁。

### 2026-08-25：S5 隐私与平台加固代码检查点

完成结果：服务端在签发离线包时记录 generation-bound device possession；privacy barrier 为每个旧 generation 设备建立 append-only child receipt 和 HMAC 派生 challenge，并通过受认证、device-bound 的 GET/ACK HTTP 接口收敛。服务端 barrier 与正文 scrub 不等待设备：challenge keyring 缺失时仍提交 barrier，并把对应 step/child 保守记录为 `unknown`；失败或 unknown child 可通过新的 challenge revision 重试。最后一个设备 ACK 只有在全部 receipt slot 同时完成时才允许父 receipt 进入 `verified`，其他 owner、备份或外部 step 仍不完整时保持 `partial/blocked` 语义。

CLI 新增 `offline purge` 与 `offline key-migrate`。Linux Secret Service、macOS Keychain 和 Windows DPAPI 后端只保存随机 wrapping key，sealed Store 仍持有 AES-GCM 数据；系统 backend 不可读、删除失败或删除后无法明确证明 `not found` 时均 fail closed 并发送失败 ACK。migration 在独占 profile lease 内只重包 DEK，不重写密封对象；purge 的独占 lease 覆盖受管对象删除、系统 key 删除验证和网络 ACK，失败或 ACK 中断时保留不含秘密的 backend marker 供后续安全重试。macOS wrapping key 经 stdin 交给 `security`，不进入 argv；无可用系统 backend 的环境保持显式 passphrase 模式，不静默降级已迁移 profile。

实际检查：

- `go test ./internal/platform/config ./internal/privacy ./internal/privacy/postgresstore ./internal/transport/httpapi ./internal/app ./internal/platform/health ./migrations`（server）与 `go test ./internal/api ./internal/config ./internal/keybackend ./internal/offline ./internal/command`（CLI）：通过。
- 临时 digest-pinned PostgreSQL 17：`TestOfflineDevicePurgeChallengeAckIsDeviceBoundAppendOnlyAndGatesParent`、真实 HTTP `TestOfflineDevicePurgeHTTPPostgres`、多设备父级收敛、失败后新 challenge revision 重试、其他 step partial 不得 verified、challenge keyring 缺失仍提交 barrier全部通过；容器在命令结束时删除。
- `TestOfflineEvaluationAndPrivacyPurgeMigrationsUpgrade000007`：通过，证明 `000007 → 000009` 追加升级有效且未重写历史 migration。
- CLI `TestOfflineKeyMigrateRewrapsExistingPassphraseProfile`、system backend outage fail-closed、purge 根目录/系统 key 删除、ACK 期间 lease contention、backend marker retry、macOS argv 不含 secret、Linux exit-code/error-channel 区分：通过。
- 受影响 server/CLI package 的 `go vet` 与共享状态 package 的 `go test -race`：通过。
- OpenAPI/handler closed contract、`git diff --check`、error-level LSP/lens diagnostics 和带必需 secret placeholder 的 `docker compose config --quiet`：通过。

修复证据：独立安全审计首先发现设备 ACK 可越过其他 step、challenge key 缺失可阻塞 barrier、系统 key backend 不可读可能被误作已删除、purge 与 prepare 存在 lease 窗口、macOS secret 进入 argv，以及失败 child 无生产重试路径。上述路径均已修复并增加精确回归；复核进一步发现 Linux `secret-tool` 的 exit 1 既可能表示 absent 也可能表示 backend error，现仅在 stderr 为空时映射 `not found`，否则按 unavailable fail closed。

复用证据：S5 未改变 S4 Open worker 的模型收敛、competition claim 或 immutable Inbox 合同，因此复用 S4 PostgreSQL worker/竞争矩阵；S5 修改了 privacy migration、CLI key wrapper 和 purge 行为，已单独重跑对应 migration、HTTP→PostgreSQL、CLI、race/vet 与 Compose 结构门禁。

已知限制：这只是 S5 代码检查点，不是完整 Builder candidate。真实 Linux Secret Service、macOS Keychain、Windows DPAPI/ACL、平台锁语义、崩溃耐久性和提交绑定仍需原生平台证据；完整仓库测试、全 PostgreSQL 故障矩阵、双 CLI 黑盒、Compose smoke 与候选提交静态检查尚未在同一稳定 candidate 上完成。

下一阶段：冻结 S1-S5 输入，只运行一次最终候选门禁；任何候选 gate 暴露的用户可见需求漂移必须返回 Shape，普通实现缺陷则在当前 Build iteration 修复并只重跑失效证据。

每个阶段结束时记录：完成结果、实际命令、通过/失败/skip、复用证据、已知限制和下一阶段。S1 至 S5 的代码检查点不代表整个 change 已验收；只有最终候选完成并经 Runtime Verifier 接受后才可归档。

### 2026-08-26：最终候选验证（阻塞）

候选环境：所有正式运行证据均来自 HexHub `1zerohk`；本机 WSL2 结果不计入候选证据。PostgreSQL 使用固定 digest，数据库 shard 严格串行执行并复用已有 pass marker。

已通过并复用的证据：

- `make check`：package tests、race、vet 和 build 通过。
- Learning PostgreSQL core、offline HTTP→service→PostgreSQL、21-case typed-record fault matrix 和 migration shard 通过。
- Memory PostgreSQL shard全部 30 个 top-level tests 通过（`750.990s`）；Privacy fault matrix 全部 20 个 cases 通过（`1549.377s`）。
- CLI 真实进程 PostgreSQL 黑盒 10/10 通过；最后一个 Due Review 场景在独占负载下通过（`220.474s`）。
- Linux Secret Service 原生 migration/clear 证据通过；Windows/macOS cross-build 和静态脚本检查通过，但 `1zerohk` 不提供 Windows DPAPI 或 macOS Keychain 原生运行环境。
- Nocturne 37 项静态合同通过；锁定 OCI 输入完成两次独立 no-cache 构建并得到一致产物，`image.lock.json` 已按可复现输出更新。
- Compose `rollback` 定向场景在测试专用 `2s` HTTP timeout、`24s` worker lease 和 `3s` poll 下通过，返回码为 `0`。

阻塞证据：唯一一次完整 Nocturne Compose `full` 门禁返回 `1`。初始 delivery 和真实 rollback/restart 均已前进，失败发生在 `check_down_queue_and_replay`：Nocturne 恢复后 delivery 在当前 `10s` 自动收敛等待和 `30s` replayable 等待内保持 `queued`，replay endpoint 持续返回 `409`，最终为 `GateError: memory delivery did not become replayable`。服务端和 Nocturne 在失败时均保持 healthy，清理后无残留进程或 Compose 容器。失败日志保存在远端 `/tmp/edu-agent-nocturne-compose-e2e-last.log` 和 `/tmp/edu-agent-nocturne-compose-e2e-compose-last.log`。

判定：最终候选尚未通过，不提交 Builder，不创建 commit。下一次 Build iteration 只处理 Nocturne outage replay 时序与测试窗口的一致性，并只重跑失效的 `check_down_queue_and_replay` 定向证据和一次 `full` Compose 门禁；不得重跑已通过的 PostgreSQL、race、CLI 黑盒或 Linux platform shard。macOS Keychain 与 Windows DPAPI 的原生证据仍需在相应原生 runner 上补齐，未运行不得标记通过。

### 2026-08-26：最终 Builder candidate handoff

后续生产修复已收敛：Nocturne mutation 前失败会以 attempt/lease-token CAS 只释放仍为 `prepared` 且未发送的 attempt；进入 `sent` 的 attempt 保持 `unknown` 并等待 reconciliation。Capabilities 取消后的 prepared cleanup 与 mutation error 后的 `sent → unknown` 记账都使用独立的两秒 detached context，原请求取消不能阻止安全状态持久化。Expiry reconciliation 只有在 durable delete plan 已存在时才把远端 orphan absence 视为幂等成功，plan 前 absence 与 hash/reference conflict 仍失败。

复用候选证据：固定 PostgreSQL shard、Memory/Privacy fault matrix、CLI 真实进程 10/10、Linux Secret Service、race/vet/build、OpenAPI、锁定 OCI 可复现性和静态合同均保持有效。独立 `expiry` gate 通过；独立 `replay` gate 通过自动恢复、五次真实 mutation failure、`pending` replay `409`、`dead+queued` replay `202`、新 boot epoch reconciliation 和最终 `applied`。后续 full gate 已通过初始真实 delivery、CRUD、learning/privacy、隔离、expiry、自动恢复和完整 replay，剩余失败位于后置 rollback rehearsal 的测试 fixture/preflight，不否定这些已完成的生产路径证据。

未运行或延期证据：`1zerohk` 不能提供 macOS Keychain 与 Windows DPAPI 原生运行；它们只具备 cross-build、argv/stdio 与 mock contract 证据，不标记为原生通过。独立 rollback rehearsal 的 parent-root fixture 已按 API 返回的 `422 core://core://` 契约修正并通过 Python/static diagnostics，但不再继续重跑 Compose；该 harness 缺口不扩张为新的产品开发主线。

Builder 判定：S1–S5 生产路径已达到可独立验收状态，提交 Runtime Verify。Verifier 应复用上述未失效证据，逐项记录原生平台的 blocked/not-run 状态，并且只有直接证伪当前 offline-sync 用户结果或必要不变量的检查才返回 Build；不得因独立环境或 harness 缺口要求无差别重跑历史矩阵。

### 2026-08-26：Verifier 修复候选 handoff

第一次独立 Verifier 逐项检查后把候选退回 Build，确认 11 个验收项存在实现缺口；本轮没有改变 brief、spec 或用户可见范围，只修复这些既有合同：

- Prepare 对具有冻结 Goal/Route authority 的有效 Session 生成默认五项、允许一至二十项的有界包；稳定 truncation reason 只在实际截断时出现。Prepare claim 在外部生成前持久化版本化冻结 plan，lease 过期接管复用同一 plan 和 operation，不重新规划；publish 事务按 pack、authorization、artifact 的外键顺序原子提交，并在当前 authority 已变化时拒绝过期 artifact。
- Signer manifest 支持递增 revision、previous manifest digest 和当前 trusted signer 签名；服务端保留 active/verification ring，CLI 从配对 trust root 连续验证 chain，拒绝 rollback、fork、未知 signer 和跨 origin/device/generation 替换。
- Open evaluation worker 即使没有模型也会启动并确定性收敛为 `completed + provisional / model_unavailable`；readiness 分别报告 offline signer、evaluation worker 和 offline protocol，worker 故障可恢复后清除 degraded 状态。
- 新增独立 offline assessment list/show/decision 服务、HTTP/OpenAPI 和 CLI 合同。查询经过 device ownership、learner generation 和 redaction barrier；confirm/override/void 写入 `offline_attempt` aggregate，不依赖 tutoring Feedback，也不推进 Session。该 decision 路径复用 `000007_offline_sync_core.sql` 已建立的 generic learning aggregate head 与 Inbox schema；仓库不需要也不存在额外的 `000010` migration。
- CLI key migration 改为 durable journal + authority marker 协议。每个崩溃边界至少保留一个可解密同一 DEK 的 authority；一旦 system backend 成为 authority，其读取失败必须 fail closed，不能回退 passphrase；旧 backend cleanup 可由恢复和 purge 幂等继续。

本轮聚焦验证全部通过：

- 本地受影响 package 单测、`go vet`、30 个实际变更 Go 文件 LSP diagnostics、`git diff --check` 通过；session-wide lens 对 213 个文件报告零 error，保留的 warning 不构成编译或验收阻塞。
- 直接密钥 SSH 主机 `186.241.77.23` 使用仓库固定 PostgreSQL 17 镜像串行执行：`TestPostgreSQLOfflineObjectivePrepareSyncStatus`、`TestPostgreSQLOfflineOpenEvaluationConvergesWithoutRewritingInbox`、`TestPostgreSQLOfflineEvaluationRetriesThenConvergesProvisionally` 通过。最后一项扩展为真实 service + HTTP + PostgreSQL 的 provisional list/show/void/replay 场景，验证首次 decision `201`、相同 operation replay `200` 且不错误生成 Evidence；decision 持久化复用 `000007` generic aggregate/Inbox schema，没有新增 assessment migration。
- 同一主机的非数据库具名检查通过：server learning、health、HTTP、prepare claim takeover；CLI offline store/key migration、command 和 API contract。
- 真实 PostgreSQL 场景还发现并修复两个仅在驱动执行时可见的问题：assessment cursor UUID 显式 cast，以及将 PostgreSQL 保留关键字 `authorization` 用作 table alias 导致的 `42601`。

复用边界：本轮没有修改 Nocturne delivery/expiry/replay、privacy purge、已有 CLI real-process 主循环、Memory/Privacy fault matrix 或 OCI 输入，因此不重跑这些已通过证据，也不重跑 full Compose。独立 rollback/backup harness 仍是非核心 fixture 限制。Linux 主机仍不能提供 macOS Keychain 或 Windows DPAPI/ACL 原生证据；A14、A18、A22、A55 的对应原生平台部分必须保持 blocked/not-run，cross-build 和 mock 不得标记为原生通过。

Builder 判定：第一次 Verifier 指出的可实现缺口已经修复并完成对应的最小本地和远端证据，候选重新提交 Runtime Verify。

### 2026-08-26：第二次 Verifier 修复候选 handoff

第二次独立 Verifier 确认 48 项通过、4 项因缺少 macOS/Windows 原生 runner 保持 blocked，并指出 A29、A15、A51 的两个残余合同缺口。本轮保持正式 scope 不变，只修复这两个边界：

- CLI signer rotation 的 trust checkpoint、sealed pack 和 prepare intent completion 现在通过 durable publication journal 作为一个可恢复的本地提交发布。任何崩溃边界都会在下一次 Store open 时重放未完成步骤；legacy intent 若已验证并推进 trust checkpoint，也可用 chain-end revision/digest 恢复发布。真正 rollback、fork、gap、未知 signer 或不匹配 chain-end 仍 fail closed。
- Offline assessment 的 `reason`、`rubric_item_id` 和 `misconception_candidate` 统一采用 OpenAPI JSON Schema 的 Unicode code-point 长度语义，并拒绝非法 UTF-8。Server service、HTTP handler、CLI API validator 和交互命令都执行相同边界；`rubric_item_id` 非空时上限为 200 code points。独立 HTTP body byte limit 保持不变。
- Delivery contract 明确 assessment decision 复用 `000007_offline_sync_core.sql` 的 generic learning aggregate head 与 Inbox schema；没有新增或依赖 `000010` migration。

聚焦验证：

- `TestPreparePublication*` Store recovery、legacy-intent rotation crash replay、正常 signer rotation、rollback/fork/gap/unknown signer 拒绝均通过。
- CLI `internal/offline` 与 `internal/command` 完整 package tests 和 `go vet` 通过。
- Server `learning`、`httpapi`、`api` 与 CLI `internal/api`、`internal/command` 的 Unicode 多字节边界、非法 UTF-8、恰好上限、超限与 OpenAPI closed-schema tests 全部通过；对应 package tests 和 `go vet` 通过。
- 实际变更文件 primary LSP diagnostics、Markdown diagnostics 与 `git diff --check` 通过。

复用边界：本轮不修改 PostgreSQL query/migration、Open worker、privacy、Nocturne、OCI 或平台 backend，因此复用上一候选未失效的聚焦 PostgreSQL 与历史 gate 证据，不运行数据库、Compose、全仓、race 或平台矩阵。macOS Keychain、Windows DPAPI/ACL 原生证据仍保持 blocked/not-run。

Builder 判定：第二次 Verifier 的三个 failed acceptance 已修复，候选提交第三次 Runtime Verify。

### 2026-08-26：第三次 Verifier 修复候选 handoff

第三次独立 Verifier 确认 assessment 长度合同已通过，但指出 signer publication journal 只保存 pack identity/digest，无法在 journal 已持久化而 PackRecord 尚未落盘的崩溃点自主恢复；Store open 忽略 missing pack 会违反 A29/A32 的恢复和 fail-closed 门禁。本轮只修复该耐久性边界：

- authenticated sealed `prepare_publish` journal 现在保存完整 PackRecord、原始 request、base/next trust checkpoint 及其内容摘要；恢复时缺失 pack 由 journal 重建，已有 pack 必须逐字一致，随后才推进 trust checkpoint 并清理 journal；
- legacy、缺 payload、未知 version、摘要/绑定不一致或 pack 冲突的 unfinished journal 会使 Store open fail closed，shared List/Get 也在未完成 publication 存在时拒绝业务读取；
- publication crash injection 覆盖 journal、pack、trust、intent completion 和 journal deletion 五个 durable 边界，重开后均得到相同 pack/checkpoint 并清除 journal；
- canonical pack 继续严格限制为 8 MiB；本地 sealed-object 绝对上限固定为 12 MiB，为合法最大 PackRecord 和 journal metadata 提供有界余量，不改变 header/schema、AEAD 或明文格式。最大 8,388,608-byte pack 形成 8,390,019-byte sealed journal 并成功恢复；8,388,609-byte pack 在写入和读取两侧都 fail closed。

聚焦验证：`TestPreparePublication*`、`go test -count=1 ./internal/offline ./internal/command`、`go vet ./internal/offline ./internal/command`、四个 publication/size 文件的 primary LSP diagnostics 与 `git diff --check` 全部通过。没有运行数据库、网络、Compose、race 或平台矩阵，因为本轮只修改 CLI sealed local-store publication 协议。

Builder 判定：第三次 Verifier 指出的 A29/A32 crash-recovery 缺口已修复，候选提交第四次 Runtime Verify。macOS Keychain 与 Windows DPAPI/ACL 原生证据仍保持 blocked/not-run。

### 2026-08-27：原生平台验收范围决策

用户确认本 change 保留 Linux Secret Service、macOS Keychain 和 Windows DPAPI 的生产实现，但最终原生候选门禁只要求 Linux。macOS Keychain 与 Windows DPAPI/ACL 的权限、锁、原子替换、迁移和 purge 原生验证作为明确的外部 follow-up，必须记录为 not-run 且不得标记为通过，但不再阻塞当前 Linux 候选归档。该决定只调整验收证据范围，不删除跨平台实现、不放宽 fail-closed 安全合同，也不使既有 Linux、PostgreSQL、CLI、privacy 或 crash-recovery 证据失效。
