# 客户端历史 Session 恢复实现设计

> 状态：Build 实施设计，目标 change 为 `client-session-resume`，Shape state version 2。
>
> 正式用户合同见 `docs/comet/changes/client-session-resume/brief.md` 与 `specs/client-session-resume/spec.md`。本文只细化实现，不改变默认自动加密保存、无时间自动删除、模型自动标题、CLI resume + F2、provider/workspace/privacy 门禁等已确认行为。

## 1. 结论

历史 Session 恢复不能实现为 `messages.json` 或在 `command/agent.go` 外层保存一份聊天文本。当前 Agent 的连续性同时依赖 committed model messages、turn 边界、tool history projection、Server/Workspace reference、Context Source/Observation/Reflection ledger、exact-ID recall、durable transcript 和结果未知的副作用回执。

实现采用四个边界：

1. `internal/agentsession`：加密记录、profile/session key、index、lock、revision、dirty marker、quota、delete/clear。
2. `internal/agentloop`：版本化 `StableCheckpoint` 导出/恢复以及副作用前 durability hook。
3. `internal/agentcontroller`：创建/恢复 Session、持久化编排、provider/workspace/privacy gate、标题和安全切换。
4. `internal/agentui`：共享 Session picker、F2 切换、durable transcript 转换和 controller generation fence。

依赖方向固定为：

```text
agentloop ───────────────────────────────┐
workspace / modelclient / config         │
                                        ▼
agentsession ──► securefile / filelock / keybackend
       ▲
       │
agentcontroller ──► agentloop + agentsession + workspace + modelclient
       ▲
       │
agentui（消费 controller interface，不拥有持久状态）
       ▲
       │
command（解析命令并创建 controller）
```

`agentsession` 不依赖 `offline` 业务对象；允许抽取并复用其 AEAD、nonce、lease 和恢复思想。`agentloop` 不导入 `agentsession`，只依赖中立 durability interface。

## 2. 现有实现事实

当前 `agentloop.Session` 直接拥有：

- `messages`、`messageTurnIDs`；
- `turns`、`turnOrder`、`turnSequence`、active/current turn；
- `toolHistory`、`toolReferences`、`workspaceReferences`；
- `ContextRuntime` 与完整 ledger；
- reasoning override、文件授权模式；
- pending question/preference/file continuation；
- preference operation IDs 与文件 effect unknown 状态。

`commitFinalAnswer` 是普通最终回答的内存线性化点；文件完成或 unknown 通过 `fileMutationCompletionFallback` 与 `finishSuccessfulTurnLocked` 形成稳定 turn。`Session.Close` 会清空全部状态，所以 controller 必须在调用原始 `Close` 前保存。

Context raw history 可以被 `trimRawHistory` 删除，因此只保存 `messages` 无法恢复已经压缩的早期语义或 exact-ID recall。必须持久化 Source/Observation/Reflection、coverage、supersession、tombstone、authority/freshness、availability 与 opaque ID 分配状态。

当前 `agentui.Runner` 绑定固定 `Conversation`，异步消息只按 UI turn ID 隔离。Session 切换后旧 context、learning refresh、title、save 或 turn event 可能与新 Session 冲突，必须增加 controller generation。

现有 `keybackend` 固定使用 `edu-agent-offline-v1`，account 绑定 server URL + device ID，并限制 secret 为 32 bytes；Session profile 必须在同 server origin 重新配对后仍可恢复，因此需要泛化 service/account 和 payload。

现有 `securefile.Root.Publish` 已提供 root-confined create/replace、expected hash 和 `completed/unchanged/unknown` outcome，是 Session record publication 基础。`securefile.AtomicWrite` 不暴露 unknown outcome，不用于 checkpoint。

## 3. 包与文件改动

新增：

```text
clients/cli-go/internal/filelock/
  lock.go
  lock_unix.go
  lock_windows.go
  lock_test.go

clients/cli-go/internal/agentsession/
  errors.go
  limits.go
  types.go
  dto.go
  crypto.go
  container.go
  store.go
  index.go
  migration.go
  transcript.go
  naming.go
  store_test.go
  crypto_test.go
  corruption_test.go
  concurrency_test.go

clients/cli-go/internal/agentcontroller/
  controller.go
  factory.go
  persistence.go
  restore.go
  switch.go
  privacy.go
  provider.go
  title.go
  transcript.go
  controller_test.go
```

主要修改：

```text
clients/cli-go/internal/keybackend/system.go
clients/cli-go/internal/offline/lease*.go
clients/cli-go/internal/offline/store.go
clients/cli-go/internal/securefile/file*.go
clients/cli-go/internal/workspace/workspace.go
clients/cli-go/internal/agentloop/types.go
clients/cli-go/internal/agentloop/session.go
clients/cli-go/internal/agentloop/file_mutation.go
clients/cli-go/internal/agentloop/interaction_resolution.go
clients/cli-go/internal/agentloop/context_ledger.go
clients/cli-go/internal/agentloop/checkpoint.go
clients/cli-go/internal/agentui/agentui.go
clients/cli-go/internal/agentui/transcript.go
clients/cli-go/internal/agentui/session_picker.go
clients/cli-go/internal/command/app.go
clients/cli-go/internal/command/agent.go
clients/cli-go/internal/config/config.go
clients/cli-go/internal/dashboard/dashboard.go
clients/cli-go/scripts/cli-platform-evidence.ps1
.github/workflows/cli-platform.yml
clients/cli-go/README.md
```

## 4. Profile 与密钥层级

### 4.1 Profile identity

Session profile 只绑定规范化 server origin：

```go
type ProfileFingerprint [32]byte

func ProfileFingerprintForServer(normalizedOrigin string) ProfileFingerprint
```

输入只允许 `scheme://host[:port]`，不包含 device ID、token、display name、path、query 或 fragment。同 origin 重新配对仍使用相同 Session profile；不同 origin 不在普通 list/resume 范围。

### 4.2 泛化 keybackend

新增通用 locator API，同时保留 offline 兼容 wrapper：

```go
type Locator struct {
    Service string
    Account string
}

func LoadSecret(Locator, maxBytes int) ([]byte, error)
func StoreSecret(Locator, []byte) error
func DeleteSecret(Locator) error
func AvailableSecret(Locator) bool
```

Session 使用独立 service `edu-agent-agent-sessions-v1`。Linux 使用 Secret Service，macOS 使用 Keychain，Windows 使用 current-user DPAPI。Windows 路径按 service/account 的不可逆 digest 分槽，写入采用同目录临时文件、flush、原子 replace 与 current-user-only ACL；secret 仍通过 stdin/内存传递，不进入 argv。

平台 secret 是固定二进制结构：

```go
type ProfileSecretV1 struct {
    Version           uint16
    PrivacyGeneration uint64
    WrappingKey       [32]byte
    ProfileNonce      [16]byte
}
```

`StoreSecret` 支持有界 payload，而不是仅 32 bytes。offline 现有 `Load/Store/Generate` 继续验证 32 bytes 并委托通用 API。

### 4.3 Per-Session key

每个 Session 生成随机 128-bit storage locator 与 256-bit DEK。DEK envelope 使用 profile wrapping key 认证加密并绑定：

- profile fingerprint；
- local privacy generation；
- Session UUID；
- storage locator；
- envelope schema；
- created time。

Session record、dirty marker 和 transcript/index projection 从 DEK 通过 HKDF 派生不同 record-kind 子密钥，避免 nonce domain 交叉。使用 AES-256-GCM；header 全部作为 AAD。

## 5. 文件布局与安全文件原语

Profile root：

```text
<user-config>/edu-agent/agent-sessions/<profile-hash>/
  profile.lock
  profile.index.enc
  key-<storage-id>.enc
  index-<storage-id>.enc
  record-<storage-id>.enc
  dirty-<storage-id>.enc
  session-<storage-id>.lock
```

文件名只含公开 profile hash 或随机 storage ID，不含 Session UUID、title、workspace、provider 或正文。`profile.index.enc` 是 profile key 加密的 locator catalog，只允许保存 schema、privacy generation、catalog revision 和随机 SessionID/StorageID 定位符；title、workspace、provider、时间、计数、lifecycle、搜索摘要和 record revision 只能进入由各 Session DEK 加密的 `index-<storage-id>.enc` projection 或权威 record。

从 `offline` 抽取 `internal/filelock`：Unix 使用 `flock`，Windows 使用 `LockFileEx`。offline lease 继续委托该包，复用原有行为与测试。

`securefile.Root` 增加只针对 root 内 opaque 文件的有界 `List` 与 handle-relative `Delete`，并保留 publication outcome。Index rebuild 和 cryptographic delete 不使用不受保护的 `filepath.Join(root, modelData)`；所有相对名先经过封闭格式验证。

Session writer lock 在 handle 生命周期内持有；profile mutation lock 只在 create/save/index/delete/clear publication 临界区持有。固定锁顺序：

```text
Session lifetime lock
→ profile mutation lock
→ generation/revision revalidation
→ record/key/index publication
→ release profile lock
```

反向切换或第二进程只得到 `session_in_use`，不得无限等待或 last-writer-wins。

## 6. 加密容器

固定 header：

```go
type ContainerHeaderV1 struct {
    Magic             [8]byte
    ContainerVersion  uint16
    PayloadSchema     uint16
    RecordKind        uint8
    Flags             uint8
    ProfileHash       [32]byte
    PrivacyGeneration uint64
    SessionID         [16]byte
    RecordRevision    uint64
    Nonce              [12]byte
    CiphertextLength   uint64
}
```

验证顺序：

1. 在分配前检查总文件和 ciphertext 上限；
2. 校验 magic/version/kind/header canonical encoding；
3. 校验 profile、generation、Session、revision 与期望一致；
4. 要求 ciphertext 精确长度且无尾随数据；
5. 以整个 header 为 AAD 打开；
6. 严格解码 payload，拒绝 unknown fields、第二个 JSON value 和引用断裂；
7. payload 内绑定字段必须与 header 相同。

每种 record kind 使用不同派生 key；nonce 使用随机 32-bit prefix + big-endian 64-bit revision。相同 kind/revision 绝不重写不同明文。publication unknown 时通过 revision + 随机 `CommitID` 重读核对，不盲重试。

## 7. 权威 Session Record 与 Index

每个 Session 只有一个权威稳定 record：

```go
type SessionRecordV1 struct {
    SchemaVersion     uint16
    SessionID         string
    StorageID         string
    RecordRevision    uint64
    CoreRevision      uint64
    CommitID          string
    PrivacyGeneration uint64

    CreatedAt    time.Time
    UpdatedAt    time.Time
    LastOpenedAt time.Time

    Profile    ProfileBindingV1
    Workspace  WorkspaceBindingV1
    Provider   ProviderBindingV1
    Privacy    PrivacyStampV1
    Title      TitleStateV1
    Search     SearchProjectionV1
    Statistics SessionStatisticsV1

    Checkpoint json.RawMessage
    Transcript json.RawMessage
    Recovery   RecoveryStateV1

    LastConsumedDirtyID string
}
```

`Checkpoint` 由 `agentloop` codec 严格解码；`Transcript` 由 agentui/controller codec 严格解码。`agentsession` 验证 envelope、metadata、blob 大小和 digest，但不复制 agentloop/UI 语义。

Profile locator catalog 与 per-Session projection 都是可重建缓存，不是权威源。`profile.index.enc` 只定位随机 SessionID/StorageID；`index-<storage-id>.enc` 使用对应 Session DEK 的独立派生 key，header 绑定 profile、generation、SessionID、StorageID 和 record revision，payload 再绑定 record `CommitID` 及 picker/search metadata。权威 title/workspace/provider/search metadata 同时存在于 per-Session record。record publication 成功但 projection/catalog 更新失败时，save 仍是 completed，并标记 index degraded；下次 list 扫描已认证 key envelope 与 record 重建。

List 绝不在 envelope 认证失败后解释 raw header 的 UUID。若 envelope 损坏，只能使用此前已认证 catalog 中的 UUID；catalog 也不可用时只显示有界 `storage:<storage-id>` locator-only corrupt 条目，该条目不能恢复或重命名，但可在二次确认后按 storage locator 做 key-first cryptographic delete。

Record container header 保持 schema v1；当前 SessionRecord payload 为 v2。读取先严格探测整数 `schema_version`，再按冻结 DTO 逐版本解码；v1→v2 只在完整 v1 校验通过后迁移。List 可内存迁移用于显示，OpenSession 在持有 Session lifetime lock 时以旧 ciphertext hash 为 CAS 条件原子发布新 ciphertext。publication unknown 必须重读并区分已提交 v2、仍为原 v1或无法判定；新记录确认前旧 v1始终可恢复。未来认证版本返回 `session_version_unsupported`，损坏返回 `session_corrupt`。

## 8. 版本化 Agent Checkpoint

新增：

```go
type StableCheckpointV1 struct {
    SchemaVersion uint16
    CoreRevision  uint64

    Messages       []CheckpointMessageV1
    Turns          []CheckpointTurnV1
    TurnOrder      []string
    NextTurnNumber uint64

    ReasoningEffort   modelclient.ReasoningEffort
    ContextCompaction string

    ToolHistory         []ToolHistoryEntryV1
    ToolReferences      []ToolReferenceEntryV1
    WorkspaceReferences []WorkspaceReferenceEntryV1
    Context             ContextCheckpointV1
}

func (s *Session) StableCheckpoint() (StableCheckpointV1, error)
func Restore(model Model, server Server, options Options, value StableCheckpointV1) (*Session, error)
```

Checkpoint 排除 system messages。已完成 assistant tool call 只保留 call ID/name，arguments 固定持久化为 `{}`；tool result 仍保留有界 live/history projection。内存中的 completed historical tool arguments也在稳定完成后归一化为 `{}`，保证 restart 前后 context 一致并避免落盘 raw args。

只导出 completed/protected/outcome-unknown 的稳定 turn。普通 active/pending turn 由 dirty marker 表示；pending sibling calls、resolver、PreparedMutation、preview、BaseVersion、旧授权与 YOLO 不进入 checkpoint。

Context checkpoint 使用有序数组，不同时信任 map 与 order：

```go
type ContextCheckpointV1 struct {
    Sources                []ContextSourceV1
    Observations           []ContextObservationV1
    Reflections            []ContextReflectionV1
    Supersessions          []ContextSupersessionV1
    Tombstones             []ContextTombstoneV1
    CoverageWatermark      string
    CoverageIndex          int
    SuccessfulObserverRuns int
    LastReflectedRun       int
    AllocatedIDs           []string
}
```

Token estimate、SourceIndex、hotTurns、cache usage、backoff、channel 和 worker 不持久化，恢复时重算。`AllocatedIDs` 包含已 prune/discard 的历史 ID，避免重用。

Restore 完整验证：

- role/enum/ID/计数/UTF-8/control/总字节；
- turn-message-source 引用；
- assistant tool calls 与 tool results 一一对应；
- persisted arguments 必须为 `{}`；
- tool history/reference call ID 一致；
- coverage watermark/index、Observation source、Reflection support；
- supersession、tombstone 与 active 状态；
- RecallText hash、source availability、authority/freshness；
- 无 system message、active turn、pending resolver 或 running state。

恢复调用当前 `agentloop.New` 重建 system/workspace prompt 和工具面，随后导入历史状态；reasoning override 可恢复，文件授权强制为 `confirm`。

## 9. Durability Hook 与线性化点

只在 controller 外层包装 `Send` 不足以保护 preference/file 副作用。`agentloop.Options` 增加中立接口：

```go
type DurabilitySink interface {
    BeginTurn(context.Context, DirtyIntent) error
    BeforePreferenceWrite(context.Context, PreferenceWriteAhead) error
    BeforeFilePublication(context.Context, FileWriteAhead) error
    StableCommit(context.Context, StableCommitNotice) error
}
```

规则：

- persistent Session 的 dirty marker 写失败时不发送模型请求；
- preference operation IDs 生成后、首次 server mutation 前写 WAL；create 成功后、admit/reject 前更新阶段；
- file `CommitMutation` 前写 path/operation WAL，不写 candidate bytes、preview、expected hash 或批准；
- ordinary final answer、cancelled/failed transcript-only turn、file completed/unknown 和 preference resolved/unknown 触发 stable save；
- unsaved Session 使用 no-op sink，但 UI 明确显示无持久化。

## 10. Dirty Marker 与恢复状态机

Dirty marker：

```go
type DirtyMarkerV1 struct {
    SchemaVersion      uint16
    DirtyID            string
    SessionID          string
    BaseRecordRevision uint64
    TurnSequence       uint64
    StartedAt          time.Time
    OperationClass     string
    MayHaveSideEffect  bool
    WriteAhead         *WriteAheadReceiptV1
}
```

它不包含用户正文、tool args、preview、授权或 credential。

状态机：

```text
Clean(record N)
  └─ publish dirty ─► Dirty(turn T, base N)
       ├─ ordinary/cancel completed ─► record N+1 consumes dirty ─► remove dirty
       ├─ effect completed/unknown ───► record N+1 + receipt ─────► remove dirty
       └─ crash ──────────────────────► restore record N + interrupted notice
```

Record 包含 `LastConsumedDirtyID`。如果 record 已发布但 marker 删除前崩溃，恢复发现 ID 已 consumed，只清理 marker，不重复显示 interruption。dirty base revision 高于 record 或绑定不一致视为 `session_corrupt`。

Preference recovery receipt 保存严格语义 intent、create/admit/reject operation IDs、candidate/revision、阶段和 stable code。恢复后只能显式 retry-only 核对同 ID/同 payload；不恢复旧 pending calls，不允许切换成“仅本次/不采用”。

File receipt 只保存 relative path、operation、outcome、stable code 和 WorkspaceReference。恢复后同路径 evidence 标记 stale/unknown，任何后续 mutation 重新 read/prepare/preview/authorize。

## 11. Workspace Binding

扩展 workspace：

```go
type Binding struct {
    Identity string
    RootPath string
    Label    string
}

func OpenBound(path string) (*Workspace, Binding, error)
func Reopen(binding Binding) (*Workspace, Binding, error)
```

`Identity` 由平台规范化绝对路径和 root directory stable file identity 哈希生成；`RootPath` 只进入加密 record，错误/UI 只使用 `Label`。

跨 workspace 恢复顺序：显示 label 并确认，随后重开原 root；重开的 identity 必须与保存值一致。不存在、移动、ACL/权限变化或不安全时仍恢复对话，但 Workspace=nil、五个文件工具不注册并显示 `session_workspace_unavailable`。不得改用当前 cwd 或相似目录。

## 12. Provider Gate

Endpoint identity：

```text
lower(provider) + NUL + normalized base URL
```

模型名不属于 endpoint identity。Record 保存原 endpoint/model label 与已确认 endpoint。

Endpoint 改变后可本地查看 transcript、rename/delete，但 `Send`、title 和任何带历史正文的 provider 请求返回 `session_provider_confirmation_required`。用户确认后持久化当前 endpoint identity；再次改变重新 gate。仅模型名变化显示 notice，不强制旧模型。

## 13. Privacy Revalidation

不新增服务端 API。恢复存在 server-derived evidence 的 Session 时，controller 使用现有认证 `Server.ExportMemory(ctx, "", 1)` 获取 `MemoryExportPage.ReadGeneration`，只使用 learner/memory generation stamp，丢弃返回正文，不把它传给 provider 或 title。

Record 保存最近已确认的：

```go
type PrivacyStampV1 struct {
    LearnerGeneration int64
    MemoryGeneration  int64
    VerifiedAt        time.Time
    Verified          bool
}
```

新 Session 在首次保存 server-derived evidence 前尝试建立 stamp。恢复时：

- generation pair 相同：server body 仍只能作为 historical evidence；
- learner generation 不同：失效全部 server-derived tool/source/assistant/Observation/Reflection；
- 仅 memory generation 不同：至少失效长期偏好 identity 与派生内容；
- `content_redacted` 或 `privacy_clear_in_progress`：沿用现有 invalidation；
- API/Nocturne 不可用：受保护正文进入内存 quarantine，模型和 title 只看到协议完整的 `session_privacy_revalidation_pending` placeholder。

Quarantine 不能被新的安全 placeholder 保存覆盖而永久丢失；record 保留加密原 evidence 与 pending 标记。后续成功核对后再决定恢复 historical body 或持久化 invalidation。

## 14. Durable Transcript

不直接序列化私有 `transcriptEntry`。定义 presentation DTO：

```go
type TranscriptEntryV1 struct {
    Sequence       uint64
    PresentationTurn uint64
    Kind           string
    CreatedAt      time.Time
    Text           string
    AssistantState string
    ModelCommitted bool
    Tools          []TerminalToolActivityV1
    Error          *StableErrorV1
    Context        *StableContextEventV1
    Notice         *TypedNoticeV1
}
```

允许 user、final/stopped/failed assistant、terminal tool summary、stable error/context/file/preference/session notice。禁止 running Activity、spinner、selector、composer draft、raw tool args/result、preview/BaseVersion、absolute path、provider raw body、hidden reasoning、Observer/Reflector/title 内容。

达到 transcript 上限时只折叠最老 presentation-only entries并插入“较早的 N 条展示记录已收起”；不删除 checkpoint、unknown receipt 或必要 Context evidence。

## 15. Agent Controller

核心接口由 `agentsession` DTO 与现有 agentloop 类型组成，避免 controller 导入 agentui：

```go
type Controller interface {
    Send(context.Context, string) (agentloop.Result, error)
    // 现有 Conversation resolver/status 方法

    Current() agentsession.ActiveView
    Generation() uint64
    Events() <-chan agentsession.ControllerEvent
    List(context.Context, agentsession.ListRequest) ([]agentsession.Summary, error)
    PreflightSwitch(context.Context, string, agentsession.SwitchOptions) (agentsession.SwitchPlan, error)
    CommitSwitch(context.Context, agentsession.SwitchPlan) error
    NewSession(context.Context) error
    Rename(context.Context, string, string, uint64) error
    Delete(context.Context, string, uint64) error
    ConfirmProvider(context.Context) error
    RetryPreferenceReceipt(context.Context, string) error
    Close(context.Context) error
}
```

切换严格顺序：

1. resolve target/scope；
2. 获取 target writer lock；
3. decrypt、schema、migration、record validation；
4. local generation 与 dirty reconciliation；
5. workspace confirmation/reopen；
6. privacy revalidation/quarantine；
7. 构造新 agentloop Session 与 transcript，但不暴露；
8. 保存当前 stable checkpoint；
9. 当前保存失败则释放 target，当前保持不变；
10. controller generation +1；
11. cancel/close old worker/title/workspace；
12. 原子替换 active Session；
13. 清空 composer/selector/running UI；
14. late old events 因 generation mismatch 丢弃。

目标等于当前 Session 是 no-op。

## 16. CLI 与 Picker

命令解析有效组合：

```text
agent [--workspace PATH] [--no-save]
agent resume
agent resume --all
agent resume SESSION
agent resume SESSION --all
agent resume --last
agent sessions delete SESSION --confirmed
agent sessions clear --confirmed
```

拒绝 `SESSION + --last`、`--last + --all`、多个 SESSION 和未知 flag。UUID 解析优先；title 使用 NFC + Unicode case-fold 精确匹配，重名返回 `session_name_ambiguous`。

`agent resume` 无 target 时运行共享 `sessionPickerModel`，选择 ID 后再创建 controller；F2 在主 Agent 内复用同一 picker 组件，不复制 list/search/rename/delete 规则。Dashboard 只返回统一 dispatcher args。

Picker：current workspace scope 默认，`Tab` 切换 all，输入最多 128 runes 的 bounded fuzzy search，字段为 title、UUID、first/recent user summary、workspace label；不搜索全文、不支持 regex/threaded。按键为 ↑/↓、PgUp/PgDn、Enter、Esc、Ctrl+R rename、Ctrl+D delete confirm，并提供“新建 Session”伪条目。provider endpoint identity 改变时，确认面板使用 `Enter` 确认向新 provider 发送，`L` 仅本地打开并拒绝发送，`Esc` 取消切换；`L` 不绕过独立的 workspace 确认。仅本地打开后 transcript、rename/delete 可用，但 `Send`、自动标题和任何历史正文请求保持 blocked，直到后续明确确认 provider。

F2 只有在非 busy、非 stopping、无 pending question/preference/file/unknown receipt、非 saving/switching/closing 时可打开。拒绝只更新 status，不改变当前 reducer。

## 17. Controller Generation

以下 Bubble Tea async message 全部增加 generation：

- turn delta/result；
- context update；
- learning refresh；
- save status；
- title result；
- picker/list result；
- switch/preflight result。

`agentui.model` 只接受当前 generation。成功切换时 generation 单调增加，UI turn sequence 可从零开始但不再与旧 Session 混淆。

## 18. 模型自动标题

调度默认：

- 首个稳定 committed user turn 后；
- 后续至少增加 3 个 committed user turns；
- 距离上次标题尝试至少 10 分钟；
- 每 Session 同时最多 1 个 job；
- provider request timeout 15 秒，record save timeout 5 秒；
- manual rename、switch、close 或 provider gate 变化使旧结果失效。

失败或 timeout 也持久化有界 attempt 边界，避免每轮无限重试；后续成功会清除旧的 `session_title_failed` 状态。

输入总计最多 6 KiB：当前 auto title、首条用户文本、最近 3 组 bounded user/final-assistant 文本。含 server/workspace tool provenance 的 assistant turn 不进入 title，避免间接发送 tool evidence。标题请求 `Tools=nil`、`MaxTokens=96`，要求严格 JSON：

```json
{"title":"..."}
```

客户端拒绝 unknown field、第二个 value、多行、空值、control/bidi、credential/opaque-ID 样式和越界。限制：256 bytes、80 runes、display width 60。失败保留旧 title 或 first-user fallback，只发布 `session_title_failed`，不修改 Agent history。

结果提交必须同时匹配 Session ID、controller generation、base record revision、title revision、`title_source=auto` 和当前 provider gate。人工 rename 增加 title revision并停止自动覆盖；空人工名恢复 auto。

## 19. 默认 Limits

集中在可注入 `agentsession.Limits`；零值或负值回退到以下生产默认：

```go
type Limits struct {
    Sessions               int   // 256
    ProfileCiphertextBytes int64 // 1 GiB
    SessionPlaintextBytes  int64 // 48 MiB
    SessionCiphertextBytes int64 // 64 MiB
    DirtyMarkerBytes       int64 // 16 KiB
    DirectoryEntries       int   // 2048

    TranscriptEntries     int   // 2048
    TranscriptBytes       int64 // 4 MiB
    TranscriptEntryBytes  int   // 64 KiB
    TranscriptEventBytes  int   // 16 KiB
    TranscriptEntryLines  int   // 1024
    TranscriptLineColumns int   // 4096
    TranscriptTools       int   // 64

    PickerQueryRunes   int // 128
    PickerResults      int // 256
    SearchSummaryRunes int // 160
    SearchSummaryBytes int // 512

    ManualTitleBytes   int // 256
    ManualTitleRunes   int // 80
    ManualTitleColumns int // 60

    AutoTitleInputBytes     int           // 6000
    AutoTitlePartBytes      int           // 1600
    AutoTitleResponseBytes  int           // 256
    AutoTitleMaxTokens      int           // 96
    AutoTitleRequestTimeout time.Duration // 15s
    AutoTitleSaveTimeout    time.Duration // 5s
    AutoTitleTurnInterval   int           // 3
    AutoTitleMinInterval    time.Duration // 10m

    NoticeCount  int // 32
    ReceiptCount int // 32
}
```

Agent checkpoint 自身的 turn/message/context 上限继续由 `agentloop` 的稳定 codec 常量控制；Store、picker、搜索摘要、标题、notice 和 receipt 不得由模型扩大。到达 ciphertext/count quota 返回 `session_store_full`，不 TTL/LRU delete。Existing Session 可安全折叠 transcript 的 presentation-only entries 后重试一次；仍超限则明确标记最新内容未保存。

## 20. Delete 与 Clear

Individual delete 使用结构化 `{SessionID, StorageID, ExpectedRecordRevision}` target。健康 record 必须匹配权威 revision；record 损坏但 projection 有效时匹配 projection revision；envelope/record/projection 都不可读时，只允许用户明确选择 locator-only corrupt 条目并以 `expected=0` 删除。已认证未来版本不会被当前客户端删除。锁序为 profile 定位 → Session lock → profile generation/target 重验；先删除 wrapped DEK 并通过重读区分 completed、unchanged、unknown，再清理 record/projection/dirty/catalog。只要 key 已确认不可达，残留 ciphertext 就不能被 rebuild 注册；后续 cleanup 失败诚实返回 `session_delete_failed`。

Full clear：profile lock 下生成 `{generation+1,new wrapping key}` 并原子替换平台 secret；成功是 cryptographic clear 线性化点。随后删除旧 envelope/record/dirty/index/temp并建立空 index。若替换前失败，旧 store 保持有效且不能宣称 clear；替换后崩溃，旧 ciphertext 因 key/generation 不匹配只能清扫，不能恢复。

Windows Session root 使用 protected current-user-only DACL，目录 ACE 向 record/projection/dirty/key/lock 子项继承，private read 和 publication 还会按 handle 复核 owner、protected 标志与精确 ACE；reparse root、parent或target fail closed。Windows keybackend 通过 current-user DPAPI，secret目录和原子替换后的`.dpapi`文件同样使用原生 current-user-only ACL。Unix继续要求目录 `0700`、文件/lock `0600`并拒绝 symlink。

## 21. Stable Errors

实现 brief/spec 中全部稳定 code。内部错误保留 cause 供测试/日志，但 UI/CLI 只显示 code、安全 Session/workspace label 和下一动作。不得显示 key account、ciphertext、absolute root、raw JSON/provider/OS body 或 tool args。

## 22. 串行实施流程

### 批次 1：安全 store 与 checkpoint

改动：泛化 keybackend、抽 filelock、补 securefile list/delete、实现 agentsession crypto/store/index/lock/dirty/delete/clear、实现 agentloop checkpoint/restore 与 transcript codec 的基础 round-trip。

退出标准：

- 20+ turn、raw trim、Context consolidation 后 round-trip；
- exact recall、authority/freshness、coverage、tombstone、supersession 保持；
- active/pending 不恢复可执行 continuation；
- wrong key、swap、bit flip、truncate、trailing、unknown schema fail closed；
- two writer 返回 `session_in_use`；
- quota 不淘汰，index 可重建；
- Unix focused native test 与 Windows cross-compile通过。

对应主要 acceptance：A1、A6-A7、A12-A17、A20-A23、A25-A71、A79-A85、A98-A115、A128-A135。

### 批次 2：命令、隐私、unknown receipt 与标题

改动：controller create/resume/save、config/`--no-save`、resume UUID/title/last/all、workspace binding、provider gate、`ExportMemory(...,1)` privacy revalidation、preference/file WAL、title scheduler、delete/clear command、Dashboard mapping。

退出标准：

- 退出/恢复后的下一模型请求等价；
- provider 改变确认前 fake provider 0 请求；
- privacy pending body 不进入 model/title；
- preference retry只用原 IDs，file unknown 不恢复批准；
- title `Tools=nil` 且不含 tool/workspace/server evidence；
- title failure不影响主对话；
- command help/non-TTY 与 focused tests通过。

对应主要 acceptance：A2-A3、A8-A21、A24、A27-A37、A64-A82、A93-A127、A132、A136、A138。

### 批次 3：F2 picker、切换与平台闭环

改动：共享 picker、F2 gate、controller generation、safe switch、rename/delete/new、footer/settings/help/README、CI native evidence。

退出标准：

- busy/pending/unknown 时不能切换；
- target corrupt/in-use/provider/workspace failure保留当前 Session；
- old turn/context/learning/title/save event不污染新 Session；
- 46×18 可操作；
- Linux/macOS/Windows required key/ACL/lock/atomic/tamper/clear cases零 skip；
- 完整 CLI gate通过。

对应主要 acceptance：A4-A6、A18-A24、A29-A33、A72-A78、A83-A92、A127、A133、A137、A139。

## 23. 测试与证据策略

实现期间只运行能够证伪当前批次的最小检查。每个批次稳定后运行其 affected package tests 和定向 race；完整 `go test -race ./...`、native platform matrix 和 Comet Verifier 保留到候选边界。

批次 1 最小矩阵：

```text
go test ./internal/agentsession ./internal/agentloop ./internal/securefile ./internal/filelock ./internal/keybackend ./internal/offline
go test -race ./internal/agentsession ./internal/agentloop
GOOS=windows GOARCH=amd64 go test -c ./internal/agentsession
```

批次 2 最小矩阵：

```text
go test ./internal/agentcontroller ./internal/agentloop ./internal/command ./internal/config ./internal/workspace ./internal/modelclient
go test -race ./internal/agentcontroller ./internal/agentloop
```

批次 3 最小矩阵：

```text
go test ./internal/agentui ./internal/dashboard ./internal/command ./internal/agentcontroller
go test -race ./internal/agentui ./internal/agentcontroller
```

候选 gate：

```text
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./cmd/edu-agent
GOOS=windows GOARCH=amd64 go build ./cmd/edu-agent
git diff --check
```

`.github/workflows/cli-platform.yml` 在 candidate SHA 上运行 native Linux/macOS/Windows Session matrix。证据脚本核对 `git rev-parse HEAD`、runner/GOOS/GOARCH，使用 fresh `go test -json -count=1` output逐个确认 manifest 中每个精确 expected test 恰好出现 run/pass、没有 failure或嵌套 skip，并验证跨进程 helper success marker；artifact包含 expected/executed/skipped清单和原始JSONL/log。Required symlink/reparse、ACL/mode、key backend round-trip/delete、two-writer lock、atomic recovery、tamper 和 privacy clear 不允许 skip；cross-build只算编译证据。实现完成不代表原生证据已通过，只有绑定最终候选SHA的三平台artifact可关闭A133/A139。

## 24. 候选与 Git 流程

1. 每批次实现与 focused test 完成后保留证据，不提前提交 Builder handoff。
2. 三批次和完整候选 gate通过后，准备包含 A1-A139 的 `builder-handoff`，明确 native platform证据尚未运行或已通过。
3. Comet Runtime required checks通过后派发全新只读 Verifier；任何 fail/blocked返回 Build，只修复实际未满足项。
4. Verifier全通过并确认 skill-coordinated pass 后 archive change。
5. 只暂存任务源代码、测试、正式设计/README、CI 和归档工件；排除 `.pi/`、`.comet/runtime/`、本地计划和任何无关改动。
6. 提交前检查：

```text
git status --short
git diff --cached --name-only
git diff --cached
git diff --cached --check
```

1. 创建本地 commit 后，按用户明确授权仅将完成候选 fast-forward 推送到 `origin/main`；不使用 `git add .`、`git add -A`、`git commit -a` 或 force push。

## 25. 已知风险

- 泛化 keybackend 会影响现有 offline key 路径，必须保留兼容 wrappers 和回归证据。
- Windows profile-secret 原子替换、ACL 与 DPAPI 是 native acceptance；Linux cross-compile不能证明。
- `ExportMemory(...,1)` privacy fence依赖 Memory read可用性；不可用时会安全地 quarantine server evidence，用户仍可恢复本地对话但上下文降级。
- Session record 最大 48 MiB 明文，每轮完整 rewrite 有写放大；首版接受，后续若有实际性能证据再引入 append journal。
- 文件 publication 与 Session checkpoint 无跨资源事务；dirty/receipt只能诚实避免重放并标记 unknown，不能承诺 exactly-once。
- 139 项 acceptance 使独立验证成本较高；实现与测试必须按批次建立可复用证据，避免重复运行无关全量门禁。
