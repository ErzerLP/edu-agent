# Outcome

项目维护一个可从干净源码工作树启动、可恢复并且默认失败关闭的候选验收入口。该入口把模型、PostgreSQL、重建一致性、Inbox/Outbox、隐私 fence、离线同步、Nocturne 和 Fast Note Sync 的既有独立证据绑定到同一候选输入；缺失依赖、空跑、跳过、锁版本变化或证据损坏不会被报告为通过。

# Scope

- 提供统一的 operations candidate runner、机器可读依赖锁索引和 evidence manifest；支持运行完整门禁或一个明确 shard，并为昂贵检查提供严格输入绑定的 resume。
- 收紧现有 PostgreSQL、Fast Note Sync 和 Nocturne runners 的目标测试执行检测、固定版本验证、日志/evidence 完整性、host 资源串行化和确定性清理。
- 用真实 programmable fake model、生产 `llm.Client`/adapter、learning application service 和真实 PostgreSQL 证明 schema 错误、低置信、超时、重试与版本化模型 profile 回归语义。
- 补齐 Knowledge 增量 snapshot 与独立全量重建的完整规范树比较，以及 Learning/Offline 增量投影与从零 replay 的语义 fingerprint 比较。
- 补齐真实 PostgreSQL Identity pairing/token 并发与故障回滚，以及通用 Outbox 在业务副作用提交后、finalize 前故障和 reclaim 的幂等收敛。
- 复用已验收的 Offline、Privacy、Nocturne、Fast Note Sync 和故障矩阵，不复制近似测试；统一入口只验证其当前候选证据仍然有效。

# Non-goals

- 不改变教学、知识维护、离线同步、Memory、Fast Note Sync 或 MCP 的用户可见业务语义。
- 不引入 SQLite 生产方言、消息中间件、第二套业务服务或跨模块通用 Repository。
- 不调用真实外部模型供应商；模型验收使用版本化、可编程且确定性的 fake server。
- 不建设 Obsidian 桌面 UI 自动化；Fast Note Sync service 使用真实固定容器，Obsidian plugin 继续由固定版本/commit和兼容矩阵约束。
- 不把未配置的外部依赖、被 skip 的测试、`[no tests to run]`、未执行平台或历史文字记录推断为通过。
- 不为 operations-hardening 重跑输入未变化且 evidence key 完全匹配的昂贵 shard。

# Acceptance examples

- A1：从干净源码工作树运行统一 candidate 入口时，runner 先验证工具、daemon、平台、磁盘/临时目录和固定依赖锁，再按确定顺序执行所选门禁；任何必需前置条件缺失返回 `blocked/not-run` 或非零失败，不生成 pass evidence。
- A2：每个 evidence manifest 绑定候选源码摘要、runner与lock摘要、测试选择、Go/Docker/Compose/Skopeo版本、OS/arch、开始/结束时间、脱敏日志SHA-256和终态。resume只接受字段完整、日志hash匹配且所有输入相同的 pass evidence；手工、截断、旧输入或未知字段证据被拒绝。
- A3：所有具名 Go 门禁在执行前证明至少一个测试匹配，执行后证明至少一个目标测试实际通过；零匹配、全部skip、`[no tests to run]`、缺失运行记录或空日志不能产生 pass evidence。
- A4：PostgreSQL、Nocturne和Fast Note Sync使用批准的精确image/platform/config digest及版本锁。任一锁、runner、schema、fixture或生产输入变化都使对应旧证据失效；升级必须重跑受影响真实契约，不能根据旧版本通过自动宣称兼容。
- A5：所有重型Docker/PostgreSQL门禁共享一个host级候选锁，使用唯一工作/evidence目录、隔离数据库或schema、动态非冲突资源和有界清理。并发调用不会覆盖日志、复用错误容器或把另一候选结果归因到当前候选。
- A6：真实 programmable fake model经生产client/adapter、learning service和真实PostgreSQL执行固定 assessment corpus。schema错误不产生权威学习事实；低置信保持provisional；瞬态超时或rate limit只按策略重试一次并保存attempt类别；重试耗尽返回degraded且不改变authoritative session/evidence。
- A7：同一固定模型语料分别以明确的baseline与candidate model profile/version运行；两者必须满足相同schema、错误分类、重试、provisional/accepted和持久化不变量。profile或协议版本变化自动失效旧模型证据。
- A8：包含add、edit、move、reorder、delete和unchanged文档的固定Knowledge语料，在任意输入排列下，增量snapshot与独立fresh rebuild的规范化完整树逐字段一致，包括stable identity、parent/order、ranges、canonical slice/hash、manifest和lineage。
- A9：覆盖canonical learning event families、补偿/redaction事件及Offline ingest/evaluation的固定语料，逐事件增量reducer与从零replay得到相同规范化projection与semantic fingerprint；response loss、重试和重启不重复Attempt、Assessment、Decision、Evidence或Outbox事实。
- A10：真实PostgreSQL中同一pairing code并发消费恰好一个device/token成功；任一device/token/code写点故障全部回滚且不消费code，移除故障后相同code可成功使用；吊销或credential epoch变化不能与并发认证/Offline提交形成越权成功。
- A11：真实PostgreSQL Outbox consumer在业务副作用已提交而`applied` finalize前故障时，可在lease后reclaim；业务幂等键保证最终只有一个副作用和一个合法terminal状态。claim/defer/dead/cancel/apply写点失败不消费operation且不留下部分sibling writes。
- A12：完整candidate索引明确列出 PostgreSQL shards、Offline黑盒、fake-model vertical、Fast Note Sync真实容器和Nocturne真实OCI/Compose gate的 `passed/failed/blocked/not-run/reused` 状态及evidence key；只有全部必需项通过或合法复用时总体才为passed，Runtime和Verifier可独立复算该结论。

# Constraints and invariants

- PostgreSQL 17 candidate shard使用仓库批准的digest且严格串行；Nocturne/Fast Note Sync保持各自独立数据库、账号、migration生命周期和固定上游版本。
- runner不得记录Bearer token、pairing code、Nocturne/Fast Note Sync credential、模型secret、知识正文、答题正文或被清除payload；日志hash基于脱敏后的持久日志。
- evidence key至少包含会改变行为或测试选择的生产代码、migration、fixture、runner、lock、工具链和平台输入。
- fault injection只证明当前写组原子性与重试语义，不通过放宽生产约束、缩短安全fence或伪造成功回执实现绿色。
- 一个完整 candidate gate可以包含多个重型matrix，因为它们共同产出同一不可分割的release qualification索引；Build仍按model、determinism/concurrency、runner/evidence三个串行批次推进，稳定前不运行完整矩阵。

# Decisions

- 保持单一child：主要结果是一个统一、可复算的候选资格结论；runner、lock索引、evidence schema和host锁是共享可变边界，继续拆分会制造不一致的全局证据协议。
- fake model验收必须穿过生产adapter和application service并落到真实PostgreSQL；分层单测保留但不能替代该vertical。
- 外部依赖兼容采用“精确锁变化即证据失效并重跑真实契约”，不把一次旧版本成功升级成未来兼容承诺。
- 综合runner默认fail closed；环境不可用是blocked/not-run，不是产品测试失败，也不是pass。
- 既有已充分覆盖的typed-record、Offline 17写组、Privacy scrub、grant和purge矩阵直接复用，不新增近似重复场景。

# Open questions

- None.

# Verification expectations

- L0/L1先验证新增manifest/lock parser、目标测试枚举器、fake model场景和规范化oracle，不启动完整Docker矩阵。
- 每个Build批次只运行受影响package、精确真实PostgreSQL测试、定向race、shell/schema检查和一个最小可运行vertical。
- stable candidate只运行一次完整server/CLI tests、vet、关键race、cross-platform builds、全部必需PostgreSQL shards、Offline黑盒、fake-model vertical、Fast Note Sync真实固定容器和Nocturne真实OCI/Compose gate。
- Runtime检查无外部依赖的确定性入口；独立Verifier审正式A1-A12、branch delta、生产调用链、evidence manifests和外部gate完整日志，确认无skip、空跑、错误复用或敏感信息泄漏。
