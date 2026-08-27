# Offline Sync 技术参考

> 本文保存离线同步的字段级协议、状态矩阵、加密容器和故障边界，供实现阶段使用。正式用户结果、范围和验收合同以当前 change 的 `brief.md` 与 `specs/offline-sync/spec.md` 为准；冲突时正式产物优先。技术细节只在对应交付阶段开始前确认，未进入当前阶段的内容不得提前扩大实现范围或测试矩阵。

## 目标与边界

本 capability 让已经配对的 Go CLI 在在线时准备一个有界、服务端签发的 Activity 包，在断网时阅读并完成其中的 practice/review Activity，再把不可变 client operation/observation 排队。恢复联网后，Go 服务逐项验证和存档操作、生成 canonical Learning Event、运行适用的评估与 Evidence 接纳，并分别返回操作存档状态和 Evidence 状态。

PostgreSQL 继续权威保存 Activity、Attempt、Assessment、Decision、Evidence、Mastery、Misconception、Review、Learning Event、Inbox、设备序号和同步结果。客户端队列不是事件日志副本，不能成为服务端 projection 的重建输入，也不能生成 canonical ID、event sequence、Assessment disposition、Evidence、Mastery、Review due date、RouteRevision 或 tutoring state。

离线 Activity 是独立的 practice/review 工作项。同步可以形成 Evidence 并更新 Mastery、Misconception 和 Review，但不直接推进、切换、完成或恢复在线 tutoring Session；设备重新联网后继续读取当前权威 SessionView，必要时由现有路线或后续规划适应新的 Learner Model。

本 capability 不提供离线模型推理、离线自由问答、离线 attached quiz、离线评分声明、离线路线生成、知识导入、Nocturne 写入、Fast Note Sync、MCP、Web、移动端或多用户同步。离线 Activity 只缓存完成该项所需的冻结 prompt、rubric、帮助规则和 canonical knowledge slices，不缓存完整知识库、完整 SessionView、完整路线历史或模型内部上下文。

## 权威边界与模块所有权

`internal/learning` 定义离线包、离线 Activity、离线 Attempt、同步结果、Evidence 资格和 canonical event 规则。`internal/learning/postgresstore` 独占对应学习表、Inbox、aggregate head、event clock、payload、projection 与 Outbox 的事务写入。`internal/tutoring` 只提供准备包所需的当前权威 Session/Route 上下文，不允许离线同步直接写 tutoring 私有表或推进状态机。

知识引用由应用层通过现有 KnowledgeReferenceResolver 冻结并校验。离线 PostgreSQL store 不直接查询 knowledge 私有表；Activity 写入沿用不可变 knowledge revision、node ID、node revision、document revision、UTF-8 byte range、canonical slice 和 SHA-256 ownership tuple，并依赖现有复合外键。

`clients/cli-go` 新增离线命令、密封存储、设备序号分配、同步编排和稳定渲染。客户端模块仍不得导入 `server/internal`，协议只来自更新后的 OpenAPI 3.1 closed schemas。

## Offline Activity aggregate

每个可完成项有服务端生成的 canonical `offline_activity_id`、revision 1、来源 Session/Goal/Route/RouteStep、knowledge revision、目标 node tuple、完整引用、Activity type、prompt、rubric、difficulty、allowed help、practice/review kind、策略版本、签发时间和双期限。Activity 内容一经签发不可修改；更改内容必须签发新的 ID。

同一learner、generation、来源Session/RouteStep、knowledge/node revision和选择epoch下，服务端在advisory lock内优先复用仍可签发且payload hash完全相同的canonical offline Activity。不同设备获得相同`offline_activity_id`，但各自获得独立的server-owned `submission_id`、预分配`operation_id`、预留`device_seq`和签名authorization；同一设备不能同时持有同一Activity的两个active submission。

客户端提交的aggregate固定为`aggregate_type=offline_attempt`、`aggregate_id=submission_id`、`expected_version=0`。successful completion时`Attempt.ID=submission_id`。该aggregate只承载一次`offline_attempt_completed`或`offline_activity_skipped`终态，不与online Session aggregate version串联，因此一个包中的第二项不会因第一项同步而天然冲突，多设备Attempt也不会因共享Activity version互相拒绝。

离线 Activity 保留来源 Session、GoalRevision 和 RouteRevision 以形成可追踪 Evidence，但其事件 reducer 不修改 tutoring SessionProjection。来源 Session 在同步前被其他设备推进或完成，不会自动使包内其余 Activity 变成在线状态转换；服务端仍独立检查签发期、knowledge revision、privacy generation 和重复 Activity policy。

## 包准备与签发

`edu-agent offline prepare`要求有效设备Bearer token、`learning:write`、开放的learning/knowledge/tutoring privacy gates，以及一个具有冻结Goal和Route的active Session。稳定的RouteActive可以生成新离线practice/review项；AwaitingResponse把当前已物化Activity的同一canonical ID/revision作为第一项，其余项仍从冻结路线和到期Review上下文生成。其他状态返回`offline_prepare_unavailable`，不改变Session。

客户端默认请求 5 项，允许显式请求 1 到 20 项。服务端同时执行最大 20 项、解密后总响应 8 MiB、单项 1 MiB、默认有效期 72 小时和最大有效期 7 天的硬限制。无法生成全部请求数但至少有一项时返回较小包并标记 `truncated` 和稳定 reason；单项超限被跳过并使用`activity_size_limited`，累计总量截断使用`pack_size_limited`。零项时请求失败且不发布空包。

包选择优先包含已到期review，再按冻结RouteStep的稳定ordinal和node revision ID选择practice。prepare事务锁定设备sequence head，为每个submission预留连续递增的`device_seq`、operation ID和submission ID；未完成、过期或本地丢弃的submission自然留下合法缺号。相同请求重放必须返回字节等价的Activity envelope、authorization、ID、签名和双期限，不重新调用模型、不重新分配序号或产生不同包。

prepare采用可恢复两阶段流程。第一阶段在短事务中以device/prepare operation ID和canonical request hash创建或claim带lease的幂等记录；claim owner在事务外调用模型，非owner只等待、重放已完成结果或在lease过期后接管，不能重复发布包。

生成Activity时复用严格tutor-model proposal适配和完整FrozenRequest校验。模型timeout、429、transport unavailable和5xx仍最多两次；畸形/schema/domain错误不重试。模型不可用时已经物化的当前Activity仍可单独进入包；不能满足至少一项时把claim收敛为可重放`model_unavailable`或`offline_prepare_unavailable`，不生成半发布状态。

第二阶段在一个`REPEATABLE READ READ WRITE`事务中按learning、tutoring、knowledge固定锁序重新校验device credential epoch、privacy generation、Session/Route/Knowledge权威快照和模型artifact，随后复用或创建canonical Activity、预留sequence/submission/operation、签名并原子发布pack。响应只从该事务提交后的持久化canonical bytes组装；准备期间Session或generation变化返回可重放stale结果，不能发布旧上下文。

服务端使用版本化Ed25519 signer。所有签名输入固定为对象专属ASCII domain separator连接`SHA-256(RFC 8785 JCS(signed_payload))`；closed JSON拒绝duplicate key和float。任何可能超过2^53-1的sequence、generation、revision、aggregate version或计数在signed/hash payload和OpenAPI中使用规范`uint63-decimal`字符串：`0`或无前导零的正十进制，领域校验范围0..9223372036854775807；只有count、TTL等上限远低于2^53的小值使用JSON number。时间使用UTC RFC3339Nano，metadata字符串要求有效UTF-8和NFC，prompt与canonical slice保持原始canonical UTF-8 bytes并只进入Activity payload digest。authorization、pack、prepare response和manifest分别签名，仓库保存2^53边界及MaxInt64跨语言golden vectors。

`SignerManifestPayloadV1`是closed JCS object，字段固定为protocol version、manifest revision decimal string、issuer、normalized server base URL、previous manifest digest、issued_at和有序keys；每个key含key ID、Base64url-no-padding Ed25519 public key、SHA-256 Base64url fingerprint、not_before、not_after、status_effective_at和`active|verify_only|retired`状态。`SignerManifestEnvelopeV1`固定为`payload,signer_key_id,signature`，使用domain separator`edu-agent-signer-manifest-v1\n`。

initial envelope位于pairing AEAD内，revision=1且previous digest为32-byte zero；其trust来自pairing session而非self-signature。后续revision必须精确+1、previous digest等于前一payload SHA-256，并由前一trusted manifest中在新manifest issued_at仍为active且位于not-before/not-after内的key签名；verify-only/retired key不能签新manifest。rotation manifest先由旧active key签名，再把旧key标为verify-only和新key标为active；旧private key随后销毁，public key保留历史验证。

prepare request携带客户端trusted manifest revision/digest，response返回从下一revision到current的有序manifest chain；客户端逐项验证，无缺号、回滚、fork或unknown signer后才原子更新keyring。新pack和authorization必须由pack issued_at时chain末尾manifest中的active key签名；verify-only/retired key只可验证issued_at早于其status_effective_at且当时处于active的历史工件。仓库提供跨两次rotation、过期key、backdated新pack拒绝和旧private-key销毁证据。

`AuthorizationSignedPayloadV1`、`PackSignedPayloadV1`与`PrepareResponseSignedPayloadV1`是独立closed object，分别使用domain separator`edu-agent-offline-authorization-v1\n`、`edu-agent-offline-pack-v1\n`和`edu-agent-offline-prepare-response-v1\n`。authorization精确绑定format、issuer、key ID、pack、device、learner generation、normalized base URL digest、offline Activity、submission/operation、device sequence、expected version、Activity payload digest、eligible/archive期限。完整Activity JSON不直接嵌入signed payload；prepare response的Activity object独立传输并必须与签名digest逐字匹配。PackSignedPayload只绑定pack metadata及有序authorization digest集合。

PrepareResponseSignedPayload绑定prepare operation ID、canonical request hash、replayed、pack digest、current manifest revision/digest和response_at，并始终由response时chain末尾active key签名；即使重放历史old-key pack，当前HTTP response也由current active key认证。public key、signature、fingerprint和digest全部使用unpadded Base64url。

normalized server base URL算法固定为：只允许http/https且无userinfo/query/fragment；scheme小写；DNS经IDNA Lookup ToASCII后小写，IPv4/IPv6使用标准压缩文本；默认80/443端口移除；path要求有效UTF-8，拒绝encoded slash/backslash、control和dot segment，逐segment解码unreserved、NFC后按RFC3986大写hex重编码，根为`/`且其他path移除末尾slash。跨语言golden vectors覆盖DNS、IPv6、default port和base path。私钥只存在服务端secret配置。

首次配对使用不在HTTP上传输pairing secret的PSK challenge-response建立signer trust root。服务端本地命令生成`lookup_id.secret`形式的一次性代码；数据库只保存由secret、lookup ID和info `edu-agent-pairing-verifier-v1`经HKDF-SHA256得到的短期verifier。客户端请求只发送lookup ID、32-byte client nonce、display name和`request_mac=HMAC-SHA256(verifier,RFC8785(request_without_mac))`。

服务端锁定未消费verifier并验证request MAC，生成32-byte server nonce，以`HKDF-SHA256(verifier,salt=client_nonce||server_nonce,info=edu-agent-pairing-response-v1)`得到pairing AEAD key。响应的device ID、Bearer token、normalized server base URL和initial `SignerManifestEnvelopeV1`使用AES-256-GCM加密，AAD绑定lookup ID、双方nonce和request digest；只有ciphertext、nonce和server nonce经过HTTP。配对事务创建device/token并消费verifier，客户端解密成功后才发布credential/config/signer root；响应丢失继续沿现有`pairing_result_unknown`流程使用新代码，不重放已消费secret。仓库保存request/response golden vectors。

loopback HTTP中间人可转发但不能读取token或替换manifest；non-loopback仍同时要求HTTPS。后续SignerManifest必须由当前已信任key签名，任何normalized base URL变化都要求重新配对。

`eligible_until`默认签发后72小时且最长7天；超过该时间仍可在更晚的`archive_until`前保存审计，但自动Evidence eligibility关闭。`archive_until`默认比eligible期限晚30天，超过后服务端拒绝新正文。Signer rotation保留旧verification key至少到所有已签authorization的最大archive期限，CLI也保留旧public key直到本地相关pack全部crypto-discard；缺少active key、key ID冲突、key pair不匹配、manifest signature失败或旧verification key提前移除会使readiness degraded并拒绝新包。

包准备使用独立幂等operation ID和request hash。CLI在首次联网前先密封并fsync状态`preparing`的request ID和canonical body；响应丢失或本地发布前崩溃时必须重放同一bytes，不能生成新request。首次成功返回HTTP 201；相同device/operation/hash重放返回HTTP 200和同一包；相同key不同hash返回`idempotency_conflict`。第二阶段事务原子持久化pack、Activity envelope、authorization、签名、sequence reservation和完成结果；CLI成功原子发布密封pack后才清除preparing journal。

## 本地密封存储

离线根目录位于用户配置目录的独立`offline`子目录。磁盘格式固定为`offline-queue-v1`。每个对象的clear authenticated header按固定字段顺序编码magic、format version、header length、ciphertext length、normalized origin SHA-256、device UUID、learner generation、object kind/ID、stable profile UUID、object schema、compression、96-bit nonce和reserved zeros；整个header作为AAD，AES-256-GCM使用128-bit tag。key backend与KDF只存在独立key header，不进入object header，因此显式key migration不需要重密封既有对象。任何长度超过公开上限、未知reserved bit、generation缺失或header/ciphertext不一致都在分配大缓冲前拒绝。

每个DEK创建时生成随机32-bit nonce prefix，并维护64-bit unsigned seal counter。对象nonce固定为prefix连接big-endian counter；每次seal前在profile exclusive lock内先通过sealed journal原子预留counter range并持久化新high-water，再发布密文，因此崩溃只留缺号而不会复用nonce。Activity、knowledge slices、答案、operation payload、receipt和错误详情全部独立密封；counter达到实现硬上限前必须通过journaled migration轮换DEK。确定性测试覆盖counter回退、重复nonce、溢出和崩溃缺号。

object container固定160-byte header，所有整数big-endian。offset 0..7为ASCII `EDUOFF1\0`；8..9为u16 version=1；10..11为u16 header_len=160；12..15为u32 flags且v1必须0；16..23为u64 sealed_len且包含16-byte GCM tag；24..55为normalized origin SHA-256；56..71为RFC4122 raw device UUID；72..79为u64 learner generation；80为object kind，1=profile、2=pack、3=operation、4=receipt、5=journal；81为object schema且v1为1；82为compression且v1只允许0=none；83保留0；84..99为logical UUID；100..115为stable profile UUID；116..127为96-bit nonce；128..159保留0。header后恰有sealed_len bytes，尾随或截短均拒绝。canonical pack明文最大8 MiB；为完整PackRecord、authenticated publication journal和固定元数据提供有界余量，单object sealed_len绝对上限为12 MiB。超过对应明文或密文上限均在写入与读取两侧fail closed。

DEK bootstrap使用固定128-byte key header：0..7为`EDUKEY1\0`，8..9 version=1，10..11 header_len=128，12 backend，13 KDF profile，14..15 flags=0，16..47 origin hash，48..63 device UUID，64..71 generation，72..87 profile UUID，88..103 Argon2 salt，104..115 wrap nonce，116..119 wrapped_len，120..127保留0。system backend的salt/nonce/wrapped_len为0且DEK存于由profile UUID定位的系统entry；passphrase backend的wrapped_len固定48，header全部作为AAD，后随32-byte DEK的AES-GCM sealed bytes。

所有container明文使用RFC8785 JCS closed object。journal schema固定字段`journal_version=1,journal_id,kind,state,source_id,target_id,revision,created_at`；kind闭集为`prepare_publish|object_replace|counter_reservation|key_migration|crypto_discard`，每种kind另有closed detail schema。未知magic/version/enum、非零reserved、超限长度或journal非法转换一律fail closed。仓库保存key header、object seal/open、每种journal和崩溃恢复的共享golden fixtures，Linux/macOS/Windows读取相同bytes。

每个profile使用随机256-bit DEK。Windows使用当前用户绑定的DPAPI，macOS使用Keychain，Linux使用Secret Service；实现不得通过外部shell命令访问密钥库，也不得引入发布时CGO要求。系统backend保存或保护DEK，并把normalized origin、device ID、learner generation和profile ID绑定到key locator或authenticated description。

系统密钥服务不可用或用户在首次初始化明确选择后备时，CLI通过隐藏TTY或非TTY stdin读取两次一致、非空口令，使用Argon2id v1参数`memory=64MiB,time=3,parallelism=1,salt=16 bytes,output=32 bytes`派生KEK，再用独立96-bit随机nonce和AES-256-GCM密封DEK；wrapped-key header和profile binding作为AAD。Argon2id不直接代替key wrapping。口令、KEK、DEK和明文缓冲不得进入argv、环境变量、日志或崩溃信息。

profile创建后key backend不可静默切换。Keychain/Secret Service临时锁定、访问被拒绝或backend错误只能返回`offline_key_unavailable`。`offline key migrate --to system|passphrase`使用durable two-phase journal：先成功解密旧DEK、创建并read-back验证新的128-byte key header/包装，再原子切换stable profile UUID指向的新key metadata，最后删除旧key entry；object header只绑定不变profile UUID和同一DEK，所以既有pack/operation无需重密封。每个边界崩溃后至少一个backend仍可解密。wrapped key永久丢失时不能重建或猜测，CLI只允许用户确认后crypto-discard整份不可读队列。

任何平台都不得自动降级为明文。密钥库不可用且用户没有提供后备口令时，`offline prepare/learn/sync/status` 返回 `offline_key_unavailable`，不写入 Activity 或答案。Bearer token及其 hash不得作为 data key、KEK 或口令材料，token轮换不得使已排队数据不可读。

每个密封对象的 associated data 固定绑定 format version、normalized origin、device ID、learner generation、object kind 和 logical ID。密文被复制到另一 server profile、另一 device、另一 generation 或另一文件名时必须验证失败，且错误不得回显答案、Activity正文、AEAD tag 或 key material。

存储新增根句柄相对的create/read/replace/delete API，所有路径组件在已打开root下逐级解析并拒绝symlink、junction、reparse point、非普通文件和根目录逃逸。发布使用同目录临时文件、file flush/fsync、原子rename或ReplaceFile、目录durability原语；Windows不能以no-op目录sync虚假宣称durable，需使用已验证的write-through/handle flush组合。AEAD不替代Unix 0700/0600和Windows用户ACL。

本地结构包含密封pack、不可变operation、独立receipt/state和key/profile journals。用户可以在进入`queued`前修改仅存在内存或密封draft中的答案；operation一旦queued就不得修改。需要重答时只能crypto-discard该submission，并在恢复联网后重新prepare获得新的server-issued operation/submission/sequence，离线客户端不能自造替代ID。

每个profile使用跨进程lease。`offline learn/status`持有shared lease直到明文清零和输出缓冲完成；prepare发布、sync状态转换、discard、key migrate、privacy purge、logout和forget-local持有exclusive lease。purge或logout不能在另一个进程仍持有shared lease时删除key并ack；超时返回`offline_profile_busy`，不强行声称清除。

CLI不得自造或重新分配device sequence；它只把prepare response中已签名预留的operation ID、submission ID和sequence与用户payload原子组合并发布。崩溃可以使预留项未使用而留下缺号，但不能复用、交换或递增改写authorization。临时文件、未完成rename、损坏ciphertext、未知format version和authorization重复均fail closed。

本地持久化采用后述完整状态矩阵。进程在`uploading`中断时，下次启动校验immutable request bytes后恢复为`queued`；取得durable ingest receipt后立即crypto-delete答案和完整operation，archived-pending与terminal只保留所需的最小加密status/receipt。

## CLI 行为

命令树增加`offline prepare`、`offline learn`、`offline status`、`offline sync`、`offline discard`和`offline key migrate --to system|passphrase`。默认渲染继续低颜色、无logo/banner/emoji/spinner、提示符中性；非TTY输出稳定纯文本，不打印答案、pack signature、密钥backend secret或完整上游响应。

`offline learn`只展示签名、hash、device、origin、generation和本地完整性验证都通过的未完成Activity。开始前本地时间超过eligible_until时阻止新答题并提示重新准备；由于设备时钟不可信，服务端使用事务内数据库时间作最终裁决。用户从冻结allowed help中选择，完成后把预签authorization与answer组合为immutable`offline_attempt_completed` operation，并显示“已保存在此设备，尚未上传”。

离线模式不提供 `:ask`、`:quiz`、assessment decision、acknowledge feedback、goal switch、session completion或模型生成。Objective题也不得在本地宣称 accepted；本地可以显示不具权威性的“答案已记录”，不能复制服务端评分器。

`offline status` 显示包数量、可用/过期项、queued/uploading/archived-pending/terminal/conflict/blocked计数、最早过期时间和最后一次成功同步时间，不显示 Activity prompt或答案。队列损坏、profile不匹配、key不可用和privacy purge required使用稳定错误码。

`offline sync` 按 device sequence升序选择最多50项、总请求不超过8 MiB。它必须验证当前 credential的 normalized origin和device ID与profile完全一致；账户、server或device不匹配时fail closed，不尝试迁移或上传。

同步结果逐项显示“存档状态”和“Evidence状态”。`archived`、`replayed`、`accepted`、`provisional`、`pending_evaluation`、`not_eligible`和`not_applicable`是不同词义；CLI不能把存档成功描述为计分成功。混合结果保留未完成项，并为每个冲突给出一个稳定下一动作。

`offline discard`默认只crypto-discard尚未提交的选定pack/operation，显示影响并要求确认；`--all`在exclusive lease内删除全部密文、temp/journal、receipts、profile和wrapped DEK/key entry，并验证受管路径不存在。它不声称覆盖OS快照、用户副本或取证残留。

`logout`和`device forget-local`先在exclusive lease内确认queue可解密、零nonterminal、零pending privacy purge和零未完成journal；只有复查通过才联系服务端吊销或删除本地binding。用户选择`offline discard --all`后仍需复查受管对象与key entry已不存在，绝不能沿用现有“先远端revoke再删本地”的顺序而遗留孤儿密文。

## 闭合状态与终态规则

服务端submission head状态固定为`reserved|claimed_succeeded|claimed_rejected|expired|revoked`。唯一合法转换为reserved到四个终态；claimed/expired/revoked不可重新打开或换绑operation、device或Activity。successful/rejected sync原子claim；数据库时间超过archive期限的sweeper转expired；device revocation只把尚未claim的submission转revoked，已存档Attempt不回滚。

本地对象状态固定为`draft|queued|uploading|archived_pending_evidence|terminal|conflict|blocked|discarded`。合法转换为draft到queued或discarded，queued到uploading/discarded，uploading在进程恢复时到queued、收到retryable/not_processed到queued、收到archive终态到archived-pending或terminal、收到idempotency/sequence conflict到conflict、收到auth/profile/purge错误到blocked；archived-pending只通过status到terminal或blocked。terminal、conflict、blocked只能显式crypto-discard，不能改写回queued；privacy purge可从任意状态原子转discarded。

archive、assessment和Evidence合法组合固定如下。successful fresh Open ingest为archived-succeeded+queued+pending-evaluation并进入archived-pending，后续processing/pending-retry仍保持；successful Objective为archived-succeeded+completed+accepted|provisional|not-eligible并terminal；successful skip为archived-succeeded+not-requested+not-applicable并terminal；successful但evidence-eligibility=false的Attempt为archived-succeeded+not-requested+provisional并terminal且不创建worker；answer-revealed为archived-succeeded+not-requested+not-eligible。worker internal failed只能对应unchanged+internal-error并blocked/degraded；archived-rejected只能对应not-requested+unchanged并terminal；retryable/not-processed保持queued；blocked进入blocked或purge后discarded；idempotency/device-sequence conflict进入conflict。其他组合是protocol error并fail closed。

Open worker瞬态重试最多持续到`min(Attempt archived_at+7 days, authorization archive_until)`；到达期限仍只有依赖瞬态错误时，用internal deterministic operation把status收敛为completed+provisional reason`model_unavailable`，而不是永久pending或failed。只有完整性/ownership损坏使用failed并要求operator修复。

一旦immutable ingest receipt已durable，本地即crypto-delete answer和完整operation bytes；archived-pending只保留密封的operation ID、ticket、receipt和最小状态。terminal receipt按配置保留最多30天后删除。conflict/blocked因可能需要诊断保留密封request，直到用户显式discard；日志永不保存正文。

`sync_request_id`只用于request关联和脱敏审计，不是业务幂等键；每项operation ID、reservation和canonical hash才决定重放。相同sync request ID可携带同一批重试，携带不同item集合不会覆盖或合并已有item结果。

本地discard未提交submission时，在线情况下先使用同一签名authorization提交`offline_activity_skipped`以终结server reservation，再crypto-discard；离线`--all`可以直接crypto-discard，但server submission保持reserved直到archive期限或device revoke，因此同设备在此期间不会重新取得同一Activity。queued Attempt不能静默改成skip；用户需先同步/显式放弃并重新prepare。

## Operation envelope与设备序号

每个上传项包含closed schema字段：签发时预分配的`operation_id`、body中的`device_id`、预留`device_seq`、`submission_id`、`payload_schema_version`、`aggregate_type=offline_attempt`、`aggregate_id=submission_id`、`expected_version=0`、`offline_activity_id`、`activity_revision`、authorization和signature、可空RFC3339 UTC `occurred_at`、`operation_type`及严格payload。服务端actor始终取Bearer credential，body device ID必须与认证设备完全相等，所有预分配字段必须逐字匹配签名authorization。

`OperationHashPayloadV1`是closed JCS object，required字段固定为protocol version、operation ID、device ID、device sequence、submission ID、payload schema version、aggregate type/ID、expected version、offline Activity ID/revision、authorization SHA-256、nullable occurred_at、operation type和payload。UUID使用canonical lowercase文本；device sequence、expected version和Activity revision使用`uint63-decimal`字符串；occurred_at必须显式存在并为null或UTC RFC3339Nano；metadata字符串NFC。answer保持用户原始有效UTF-8 bytes且不做Unicode normalization，服务端重算其SHA-256并与payload字段常量时间比较。

operation request hash固定为`SHA-256(RFC8785 JCS(OperationHashPayloadV1))`，内部保存32-byte值，wire诊断需要时只用unpadded Base64url。它不依赖HTTP framing或服务端动态时间。CLI持久化canonical JCS bytes并在transport uncertainty时原样重发；语义字段改变必须获得新的server-issued submission。仓库保存Attempt、skip、null occurred_at、CJK answer及array-order的Go/跨语言golden vectors。

允许的operation type只有`offline_attempt_completed`和`offline_activity_skipped`。Attempt payload包含answer、answer SHA-256、help level和客户端展示observations；skip只包含封闭reason code。自由文本observation、模型prompt、完整Session快照、客户端评分或自造Evidence字段一律拒绝。

`device_seq`是每设备64-bit正整数，由prepare事务从数据库sequence head单调预留且不可复用。服务端允许不同HTTP请求按任意预留顺序到达和暂时缺号，不等待未使用submission，也不使用device sequence决定event sequence。相同device/sequence/operation/authorization/canonical payload是重放；相同sequence配不同operation、submission、authorization或payload返回`device_sequence_conflict`；相同operation配不同sequence或body返回`idempotency_conflict`。

服务端永久保存reservation和claim。低于high-water的sequence只有在它对应的签名reservation尚未claim时才可补入；未预留sequence、跨device reservation、已占用sequence和过期authorization均不能通过新operation ID绕过。package过期、本地删除或崩溃留下的reservation保持缺号而不阻塞后续同步。

`occurred_at`只作为不可信审计/展示信息保存。canonical event排序、received time、estimated active time、retained的24小时间隔、review推进、Evidence胜负和多设备first-winner全部使用服务端数据库时间和event sequence。

## 同步HTTP契约

现有`POST /v1/pairings/exchange`升级为PSK协议。closed request固定为`lookup_id`、Base64url 32-byte`client_nonce`、`display_name`和Base64url 32-byte`request_mac`，不再接受或传输原始`code`。201 closed response固定为`server_nonce`、`response_nonce`、`ciphertext`和`protocol_version=1`；ciphertext解密后closed payload固定为device、token、normalized server base URL、initial `SignerManifestEnvelopeV1`和response revision。它保留400、401`pairing_failed`、429、500；plaintext token只存在解密后的客户端内存，HTTP层不再直接返回。

`POST /v1/learning/offline/packs`要求单一`x-required-scope: learning:write`。closed request字段固定为`operation_id`、`payload_schema_version=1`、`expected_session_version`、`trusted_manifest_revision`、`trusted_manifest_digest`、可空`requested_count`和可空`requested_ttl_seconds`；count范围1..20、默认5，TTL范围900..604800秒、默认259200。closed response固定为`operation_id`、`replayed`、`pack`、`manifest_chain`和`response_signature`；pack包含pack ID/revision、device ID、learner generation、normalized origin、source Session/Goal/Route、issued/eligible/archive time、truncated、closed reason codes、有序Activity payload、有序authorization及pack signature。response signature封装前述PrepareResponseSignedPayload。Prepare reason code只允许`requested_count_limited|activity_size_limited|pack_size_limited|model_partial|route_exhausted|review_exhausted`；非truncated返回空数组。Activity payload不位于PackSignedPayload内部，但每项必须匹配签名authorization中的digest；任何额外字段拒绝。

prepare首次成功返回201、同hash重放返回200，并明确列出400`invalid_request`、401`authentication_failed`、403`forbidden`、409`version_conflict|idempotency_conflict|offline_prepare_unavailable`、413`request_too_large`、429`rate_limited`、503`content_redacted|privacy_clear_in_progress|model_unavailable|offline_signer_unavailable`及500`internal_error`。错误继续使用现有closed error envelope，不在错误体夹带pack、公钥或正文。

`POST /v1/learning/offline/sync`要求`learning:write`。closed顶层request固定为`sync_request_id`、`payload_schema_version=1`和`operations`；operations数量1..50、总body最多8MiB，并按request内device sequence严格递增。每个operation使用前述closed envelope。`offline_attempt_completed.payload`固定为`answer`、`answer_sha256`、`help`和`observations`；observation kind只允许`activity_presented`与`answer_recorded`，包含可空untrusted occurred_at且不进入reducer。`offline_activity_skipped.payload`只包含`reason`，枚举为`user_skipped|expired_locally|unreadable_local_item`。

sync批次不原子。服务端按请求数组顺序为每项开启独立事务；确定性rejection或replay后继续下一项，数据库/依赖瞬态错误时停止，当前项返回retryable且后续项返回not_processed。此前成功项保持提交，客户端可用完全相同canonical operation bytes重发整个批次。

sync成功HTTP固定为200，表示request级schema/auth通过而不是每项成功。request级畸形JSON、duplicate/unknown field、超限、未排序、重复sequence或签名结构错误在处理任何item前整体400/413拒绝。端点还列出401、403、429、503`content_redacted|privacy_clear_in_progress`和500；item级业务冲突只进入200结果数组。

`OfflineSyncItemResult`使用closed discriminator `result_kind=archived|retryable|blocked|conflict|not_processed`。所有variant都要求operation ID、device sequence、submission ID、archive status和reason codes；只有archived variant要求replayed、aggregate version、immutable ingest receipt、assessment/evidence status和可空entity IDs，且archived-succeeded返回status ticket、archived-rejected不返回ticket。retryable、blocked、conflict和not-processed严禁携带ingest receipt、aggregate version、ticket或entity ID。

`ingest_receipt`固定为receipt ID、archived_at、aggregate version、first/last event sequence、projection as-of和archive status；`status_ticket`固定为ticket ID、operation ID、revision和updated_at。响应不得包含原答案、request hash、其他设备ID、模型raw body或knowledge slice。

archive status枚举固定为`archived_succeeded|archived_rejected|not_archived_retryable|not_archived_blocked|idempotency_conflict|device_sequence_conflict|not_processed`。assessment status固定为`not_requested|queued|processing|pending_retry|completed|failed`。Evidence status固定为`accepted|provisional|pending_evaluation|not_eligible|not_applicable|unchanged`。reason code闭集为`duplicate_activity_submission|stale_knowledge_head|expired_activity|stale_context|stale_policy|answer_revealed|model_unavailable|evaluation_invalid|offline_activity_invalid|content_redacted|privacy_clear_in_progress|device_revoked|authorization_expired|authorization_invalid|version_conflict|idempotency_conflict|device_sequence_conflict|not_processed|internal_error`；无原因时返回空数组，不使用自由文本。

expired submission在签名和reservation可验证时返回archived-rejected、assessment not-requested、Evidence unchanged及`authorization_expired`，并永久终结reservation且不保存答案。批次中途device revoke、content-redacted、privacy-clear或无法信任authorization返回not-archived-blocked且无receipt；前两类使客户端进入blocked，privacy/content类触发本地purge后discarded。request开始前已撤销仍返回HTTP 401。

`GET /v1/learning/offline/operations/{operation_id}`要求`learning:read`，只允许canonical UUID path，返回同一认证设备的immutable ingest receipt和current status projection。它列出200、401、403、404`operation_not_found`、429、503`content_redacted|privacy_clear_in_progress|projection_unavailable`和500。Inbox只保存首次ingest/archive终态，异步评估不得反写它；GET与sync replay从独立operation-status projection合并当前状态。

`GET /v1/privacy/offline-purge`要求新增`privacy:device` scope，并故意绕过已关闭的learning content gate，只读取不含正文的device receipt。没有任务返回204；有任务返回200 closed challenge：erasure ID、device ID、old/current generation、challenge revision、challenge、issued_at和status。challenge以版本化服务端privacy challenge key对`erasure_id|device_id|old_generation|current_generation|revision`执行HMAC-SHA256确定性派生并用Base64url返回；数据库保存key version、revision和challenge hash，同一revision在重启/响应丢失后稳定重算。轮换先递增revision并保留旧verify key，已成功ack的revision不能复活。

`POST /v1/privacy/offline-purge/{erasure_id}/ack`要求`privacy:device`。closed request使用oneOf：成功形态含challenge revision、challenge、`outcome=succeeded`和`managed_objects_absent=true`；失败形态含challenge revision、challenge、`outcome=failed`和closed`failure_code=profile_busy|key_delete_failed|path_delete_failed|verification_failed`。服务端以认证device、erasure、generation和constant-time challenge hash校验，同一outcome ack重放返回同一200 child receipt，不同outcome使用同revision返回409。端点列出200、400、401、403、404、409`purge_challenge_invalid|purge_not_current|purge_ack_conflict`、429和500。普通新建及迁移设备默认增加`privacy:device`，但仍不含`privacy:erase`。

现有`PrivacyErasureReceipt`的`offline_device_cache`step增加不含正文的`device_counts{pending,succeeded,unknown,failed}`、`children_truncated`和可空next cursor。`GET /v1/privacy/erasures/{erasure_id}/offline-devices?limit&cursor`要求`privacy:read`，limit默认50、最大200，按device ID稳定分页返回display-safe device ID、source generation、child status、challenge revision、updated_at和closed reason；cursor绑定erasure ID和receipt revision，stale cursor返回409。它列出200、401、403、404、409、429和500。

## 服务端裁决与事务

服务端先验证认证、privacy generation、envelope schema、sequence reservation、Inbox和签名Activity，再按固定顺序锁定offline attempt aggregate和Activity evidence slot。所有TTL、winner和received-time裁决都使用取得这些锁后的数据库`clock_timestamp()`线性化点，不使用请求进入应用层的时钟。相同`(device_id, operation_id)`同canonical operation hash返回首次archive终态；不同hash永久冲突。确定性领域拒绝在一个事务中claim reservation、写入Inbox archived_rejected并终结submission，因此可永久重放；取消、数据库错误或依赖瞬态错误整体回滚且不claim reservation、不消费Inbox或event clock。

实现新增调用方定义的`OfflineIngestStore`事务入口。store在事务内取得device credential epoch/row lock、generation gate、reservation、aggregate、evidence claim和数据库时间，再调用纯领域planner构造CommandBatch；禁止应用层在事务外用`time.Now()`预计算完整batch后复用现有Commit，从而避免expiry、winner或revocation的TOCTOU。

一个successful operation事务原子写入sequence claim、Inbox、必要的Activity/Attempt typed records、canonical payload/events、offline aggregate head、Activity evidence slot、objective Assessment/Decision/Evidence、projection/checkpoint和必要Outbox。任何写点失败不得留下sibling row、sequence claim、Inbox、event clock推进或部分projection。

签发Activity尚未物化为现有`learning_activities`时，首次Attempt事务用签发时冻结的服务端ID、Session/Goal/Route/Knowledge ownership tuple物化Activity和references，再写Attempt。AwaitingResponse缓存的current Activity始终复用原canonical ID/revision，不复制。任何offline materialization都不更新`tutoring_sessions.activity_id/attempt_id/state`。

`offline_activity_skipped`只生成审计event并使对应offline attempt aggregate终结，不创建Attempt、Assessment或Evidence。`answer_revealed`帮助级别保留Attempt但Evidence status为`not_eligible`，并按现有exposure语义更新可重放投影。

## 多设备与重复Activity policy

同一canonical Activity ID和revision具有一个数据库约束保护的normal-evidence slot。只有online submit和offline ingest能在Attempt事务中取得claim；第一个通过revision和业务校验并成功提交Attempt的事务成为winner。Offline worker不得取得、替换或释放claim，只能在锁内验证`winning_attempt_id`等于自身Attempt后沿用资格。migration为已有Activity按现有有效Evidence回填winner；若历史上同一Activity发现多份active Evidence则fail closed并要求修复，不能任意选择。

后续设备或operation针对同一Activity提交的不同offline Attempt仍保存不可变审计记录，但固定为duplicate contender，Evidence status默认`provisional`且不能自动创建第二份有效Evidence。调整`occurred_at`、device sequence、HTTP到达顺序重试或本地时钟不能覆盖已经提交的winner。

若offline ingest先赢得一个仍处于在线Session AwaitingResponse的canonical Activity，Session本身保持不变。随后online submit落败时仍沿现有online tutoring状态机保存online Attempt、生成用于用户反馈的Assessment/Decision并进入Feedback，但持久化`evidence_eligibility=false`和`duplicate_activity_submission`，confirm/override不得创建Evidence；用户解决provisional并acknowledge feedback后可正常推进Session。该在线反馈路径不改变offline winner，也不把Session进展解释为第二份计分证据。

相同Attempt operation重放不创建第二份Attempt、Assessment、event batch或Evidence。duplicate contender不自动失效winner，也不提供本change内的跨Attempt替换命令；用户需要新的服务端Activity形成新证据，避免把历史答案静默迁移到已计分Activity。

数据库使用`activity_id+revision`唯一claim、`winning_attempt_id`复合归属和Evidence复合外键/约束保证每个Activity最多一份当前有效Evidence。Attempt和Assessment持久化不可变`evidence_eligibility`及closed reason；`duplicate_activity_submission`、`stale_context`、`expired_activity`、`answer_revealed`和privacy mismatch不能通过后续confirm/override升级为Evidence。并发两设备提交时只能一个事务取得normal slot，另一个在重读锁定状态后走permanent duplicate-provisional路径，不依赖应用层先查后写。

## Knowledge revision、过期与评估

Activity引用的immutable knowledge revision仍存在且等于签发时revision时，服务端始终按签发内容评估，不使用当前Markdown重新生成quote、range、hash或node identity。knowledge head在签发后推进形成`stale_knowledge_head`、数据库received time超过eligible期限形成`expired_activity`、Goal切换或RouteRevision被明确替代形成`stale_context`，或签发策略版本不再active形成`stale_policy`；普通Session状态推进本身不使独立离线practice失效。这些不合格情况在archive期限内仍可存档Attempt，但持久化evidence eligibility为false、Evidence status固定provisional并带closed reason，Mastery和Review不变化。

知识revision因privacy erasure被redacted、learner generation已推进、ownership tuple失效或引用内容/hash无法验证时，privacy/完整性规则优先：服务端不得重新写入旧正文，返回`content_redacted`或`offline_activity_invalid`，客户端进入本地purge而不是保存Attempt审计。

只有取得winner claim且`evidence_eligibility=true`的fresh Objective Activity才在successful Attempt事务内使用冻结ObjectiveRule和现有确定性assessment acceptance policy生成Assessment、Decision及适用Evidence，因此同步响应可以立即返回accepted、provisional或not-eligible。duplicate、stale、expired或stale-policy Objective Attempt与Open相同，直接以assessment not-requested、Evidence provisional终结。服务端不得使用客户端声称的正确答案或客户端评分。

只有取得winner claim且`evidence_eligibility=true`的fresh Open Activity才原子存档Attempt和`pending_evaluation`状态，并通过transactional Outbox排入独立offline assessment worker。duplicate、stale、expired或stale-policy Open Attempt直接以assessment not-requested、Evidence provisional和closed reason终结，不调用模型且永远不能升级资格。

Offline worker不调用要求Session处于Evaluating/Feedback的tutoring proposal或decision入口；它使用冻结Activity、Attempt、rubric和knowledge references建立版本化FrozenRequest，经严格model schema后用确定性internal operation ID在offline attempt aggregate上apply assessment policy。

worker状态固定为`queued`、`processing`、`pending_retry`、`completed`和`failed`。每次模型执行仍最多两次attempt；timeout、429、transport unavailable和5xx在archive期限内保持`pending_retry/pending_evaluation`并按持久化backoff继续，不因一次budget耗尽变成failed。永久schema/domain输出形成completed+provisional reason`evaluation_invalid`；只有数据库损坏、无法解码冻结artifact或违反ownership等内部不可恢复错误进入failed、Evidence unchanged并使projection/readiness明确degraded。privacy barrier会取消或fence旧generation job，stale worker不能在scrub后写回正文。

fresh winner评估产生provisional Assessment/Decision时，它进入独立offline assessment query，不依赖current Session work item。`GET /v1/learning/offline/assessments?status=provisional&limit&cursor`和`GET /v1/learning/offline/assessments/{assessment_id}`要求`learning:read`，返回closed冻结Activity/Attempt/Assessment、current Decision、ConfirmableAssessment结果、projection metadata和allowed decisions；cursor绑定projection generation/as-of。列表列出200、400、401、403、409`stale_cursor`、429、503`content_redacted|privacy_clear_in_progress|projection_unavailable`和500；按ID另列404。

`POST /v1/learning/offline/assessments/{assessment_id}/decisions`要求`learning:write`。closed request复用confirm/override/void oneOf discriminator，但aggregate固定为offline_attempt，携带operation ID、expected offline-attempt version和expected disposition version。confirm只在服务端对冻结Activity/Attempt/Artifact执行ConfirmableAssessment为真时可用；override逐项复制immutable quote/range/hash并只允许改变conclusion/reason/candidate；void要求reason。首次成功201、同operation/hash重放200，并列出400、401、403、404、409`assessment_disposition_conflict|version_conflict|idempotency_conflict|invalid_decision`、429、503和500。Decision和Evidence事件追加到offline_attempt aggregate，验证winning claim和evidence eligibility，不改变SessionProjection。CLI提供`offline assessments`与`offline assessment show|confirm|override|void`，冲突刷新同一offline Assessment。

ineligible duplicate/stale/expired Attempt不创建Assessment，不出现在provisional query，也不能通过该API升级。fresh provisional被confirm/override/void后从pending query移除，Node/Mastery/Review按新Decision重放；相同decision operation重放不重复Evidence。

`GET /offline/operations/{operation_id}`和后续sync replay同时返回immutable ingest receipt与当前status projection。Open评估从pending收敛到accepted/provisional/not-eligible时追加canonical events并更新projection，不改写首次Attempt、Inbox request hash或ingest result。worker重试、响应丢失和进程重启不得重复Assessment、Decision或Evidence。

## Privacy generation与设备清除

包、Activity、operation和本地密封profile都绑定签发时learner generation。prepare第二阶段在同一个repeatable-read read-write事务中按固定顺序持有learning、tutoring和knowledge owner gate/authority快照，写入并读取最终canonical response；sync、status和任何正文读取同样在对应owner permit/generation快照内完成。barrier关闭后不得返回旧generation内容或接受旧operation。

服务端维护append-only device possession ledger：任何device/generation只要成功签发过pack，就视为可能持有本地密文，pack TTL、operation terminal、token轮换或server-side discard都不能自动清除该责任；只有该设备在官方CLI完成crypto-discard后的认证ack可收敛。

全局privacy barrier第一事务立即关闭旧generation读写、阻止旧正文返回、取消/冻结未发送评估Outbox，并创建owner scrub steps、一个固定`offline_device_cache`summary step及每个ledger device的child receipt；它不在同一事务中直接宣称物理scrub完成。learning/knowledge/tutoring等owner随后在独立、幂等、可恢复事务中RedactTx，再由VerifyRedacted证明pack、Attempt、Assessment、event、projection和job正文已清除。服务端owner scrub不等待Nocturne可用，也不因设备离线延迟。

相关设备下次认证时先从专用purge endpoint取得不含正文的稳定ack challenge。CLI取得profile exclusive lease，等待所有shared reader退出，crypto-discard pack、operation、receipt、temp/journal、profile和wrapped DEK/key entry，验证官方受管路径无对象且backend无法read-back key后提交ack。ack幂等绑定erasure ID、device ID、old generation和challenge hash，不能由其他设备代答。

每个device child由append-only identity row和versioned head组成，identity绑定erasure/device/source generation。child status固定为`pending|succeeded|unknown|failed`：创建时为pending；官方CLI有效ack转succeeded且terminal；device在ack前被吊销或用户标记lost转unknown；官方CLI明确报告受管crypto-discard失败转failed。pending没有自动超时，长期离线保持pending。

认证device对failed或unknown head调用GET时，服务端在锁定head的单一事务中确认credential当前有效、递增challenge revision、追加新head并转pending；并发GET只生成一个revision，其余返回同一challenge。旧revision从新head提交起永久失效。succeeded返回204且不重开；当前仍revoked的unknown device无法认证，因此继续unknown。测试覆盖failed后修复成功、unknown device恢复、并发GET/ack、旧revision ack和每个响应丢失点。

summary step完全派生：全部child succeeded时succeeded；存在failed时failed；否则存在unknown时unknown；否则存在pending时pending。只要summary不是succeeded，整体ErasureStatus固定为现有`partial`，不能标记verified。吊销token只阻止同步，不能作为磁盘清除证据；该强保证可能使verified无限期等待。

purge ack是认证设备对官方CLI受管目录crypto-discard结果的声明，不是不可伪造的物理擦除证明。它不承诺覆盖OS快照、磁盘取证、用户复制、远程终端日志或第三方备份；服务端30天managed-backup承诺保持不变，外部副本不纳入虚假清除声明。

## 安全、限流与运行状态

offline pack和sync端点使用现有Bearer认证、device ownership、learning scopes、请求体上限和设备/IP限流。body中的device、origin、generation或签名字段不能覆盖认证上下文。prepare第二阶段发布前和每个sync item事务内都锁定device row或验证单调credential epoch及revoked状态；批次中途撤销使尚未处理item返回device_revoked，已提交item保持权威。server-side evaluation job在Attempt已存档后使用内部身份继续，除非privacy generation fence取消；设备撤销本身不回滚已存档Attempt。

服务默认loopback；non-loopback仍要求HTTPS反向代理和现有安全边界。HTTP client禁止redirect；signer trust只由pairing transcript root和已信任key签名的manifest推进，不能从普通exact-origin response替换。TLS错误、origin变化或manifest chain失败不能通过临时确认绕过。

新增readiness组件分别报告offline signer、evaluation worker和local client key backend能力。Signer缺失阻止prepare但不阻止在线教学和已存档status；模型不可用使open evaluation queued并使readiness degraded；本地key backend不可用只影响该CLI，不能被服务端描述为健康。

服务端配置严格验证active signer key ID、Ed25519 key长度、verification ring、TTL范围、pack limits、sync batch limits、worker lease与HTTP timeout比例。secret校验错误不得回显私钥、口令、wrapped key或签名原文。

## 迁移、重放与查询

新增migration必须在现有000006之后增加，不修改checksum-protected历史migration。它为prepare claim、canonical offline Activity、per-device submission authorization、sequence reservation/claim、offline attempt aggregate、Activity evidence claim、immutable evidence eligibility、operation status、evaluation job、device possession ledger和privacy child receipt建立复合ownership、唯一性、generation gate及append-only约束，并把新aggregate/event type纳入明确schema version/upcaster。

prepare本身不写Learning Event；签发记录是服务端授权事实。首次successful completion的offline_attempt event batch按固定ordinal包含`OfflineActivityMaterialized`（仅Activity尚未物化时）、`OfflineAttemptSubmitted`、可选`OfflineAssessmentQueued|AssessmentRecorded`、可选`AssessmentAccepted|EvidenceAccepted`及投影事实；skip只写`OfflineActivitySkipped`。

签名和reservation可信的deterministic archived rejection写一个不含answer/prompt/slice的`OfflineOperationRejected` canonical event，使offline_attempt aggregate从version 0到1，并在payload中只保存operation ID、submission、device sequence、closed reason和received_at；因此archived-rejected ingest receipt具有真实first/last event sequence。authorization无法信任、privacy/content blocked或内部retryable不写rejection event、不推进aggregate且无receipt。worker和offline decision API后续在同一offline_attempt aggregate追加Assessment/Decision/Evidence事件。所有event携带immutable`parent_session_id`、Goal/Route/Knowledge、Activity ownership和schema version。

TimelineItem新增closed字段`parent_session_id`、`source=online|offline`、archive/evidence disposition和actor device ID。Session timeline匹配自身aggregate ID或parent session ID；estimated active-time可使用offline event的parent和可信received time，但SessionProjection reducer绝不改变state/focus/route。Replay把offline Activity/Attempt/Assessment/Evidence纳入Timeline、Node、Evidence、Review、Mastery和Misconception。

Evidence projection持久化`accepted_event_seq`。Mastery、Review和Misconception的确定性排序先按accepted event sequence，再用稳定ID作不可达的tie-break；received_at只用于24小时间隔和due-date计算。增量投影与从零replay必须得到相同semantic fingerprint、evidence claim、session-filtered timeline和review due date。

公共查询显示离线Attempt的received time、可选untrusted occurred time、archive/evidence disposition和来源device，不显示本地sequence gap、request hash或签名。estimated active time只能使用现有可信received-time policy，不从离线duration或设备时钟推导。

OpenAPI为所有新schema使用`additionalProperties:false`、closed enums、canonical UUID、RFC3339、int64边界、数组/字节限制和真实错误响应。CLI contract test从OpenAPI读取path、scope、request discriminator、response status、archive/evidence enums和privacy错误，防止手写DTO漂移。

## 验证要求

领域单元测试覆盖包选择与truncated reason、RFC8785/Ed25519 golden vectors、Activity/Attempt/skipped状态、same-device active submission唯一、online/offline duplicate winner、sequence reservation/gap、archive/evidence正交结果、双期限、knowledge stale、objective/open worker终态和Session不推进。Activity、submission、operation、worker、device child receipt和local queue的每个合法/非法状态转换都需要闭合矩阵。

真实PostgreSQL测试使用随机隔离schema并串行运行。故障矩阵逐点覆盖prepare claim/lease/final publish、canonical Activity/reference、submission authorization、sequence reservation与claim、Inbox rejection/success、Activity/Attempt/payload、event payload/event clock、evidence claim/eligibility、Assessment/item/Decision/Evidence、Outbox/evaluation job、aggregate head、projection/checkpoint、device possession ledger和privacy child receipt；每点证明失败不留下sibling row并允许合法恢复。

并发数据库场景覆盖prepare response loss与lease takeover不重复模型artifact或sequence、同sequence conflict、缺号乱序、online submit对offline ingest、两offline device winner、worker对privacy barrier、pack publish对device revoke、50-item batch中途revoke、旧key signer rotation、expired/archive deadline和历史Evidence claim migration fail-closed。

Inbox/status回归在Open ingest后保存Inbox JSON/hash，worker从pending收敛后证明Inbox字节不变，而status endpoint与sync replay返回新Assessment/Evidence状态；服务重启及全量replay后结果相同。Session-filtered timeline验证parent_session_id和offline disposition可查，accepted_event_seq排序确定，SessionProjection state/focus/route不变。

CLI测试覆盖AES-GCM header/tag/长度、AAD跨profile复制、nonce碰撞与seal上限、system key backend、Argon2 wrapping与错误口令、prepare durable intent、shared/exclusive lease、temp/flush/rename/delete/key journal各崩溃点、中间symlink/junction/reparse交换、logout/forget-local复查、部分sync、response loss、mixed result、key migration和crypto-discard正文泄漏扫描。

候选黑盒使用真实服务、真实PostgreSQL和至少两个隔离CLI profile。场景覆盖在线准备默认5项和partial pack、完全断网答题/skip、乱序/缺号同步、当前online Activity与offline Device竞争、双offline device同Activity、objective accepted、open pending/retry/provisional收敛、knowledge head推进、answer-revealed、prepare与sync post-commit response loss及服务/worker重启。

隐私黑盒先给多台设备签发pack，即使pack过期或operation terminal仍保留possession责任；设备离线时发起全局erasure，分别证明barrier提交后旧generation正文立即不可访问、后续每个owner RedactTx/VerifyRedacted最终无残留，而ErasureStatus因device child保持partial。设备恢复后取得稳定challenge，官方CLI在exclusive lease内crypto-discard并幂等ack，全部child和owner steps succeeded后才允许verified；丢失或吊销设备保持unknown。

本 change 的原生候选门禁在 Linux 验证真实 session D-Bus Secret Service、Argon2id fallback、权限、lock、atomic replace、hidden input、key migration 和 purge，artifact 绑定 candidate SHA、OS/arch、Go version、CGO值、backend和结果。macOS Keychain与Windows DPAPI/ACL保留生产适配；对应的原生权限、锁、原子替换、迁移和purge验证作为外部follow-up，未运行时保持not-run且不得标记为通过。执行这些follow-up时仍需在对应原生平台覆盖fallback、篡改和删除，并绑定同等artifact metadata；交叉编译和mock不能替代原生证据。

按照项目分层测试策略，开发期默认运行受影响package，持久化批次只运行一次串行PostgreSQL矩阵；候选里程碑再运行CLI race/vet/build、server受影响测试、真实黑盒、Linux原生证据和必要Compose smoke。macOS/Windows原生验证按外部follow-up独立排期，未运行时保持not-run且不作为当前候选门禁。Nocturne OCI、Fast Note Sync和MCP未改变时不重复无关重型验证。
