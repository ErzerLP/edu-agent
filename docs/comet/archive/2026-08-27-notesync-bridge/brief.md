# Outcome

交付一个可实际连接 Fast Note Sync 与 Obsidian 的双向知识桥。服务端接受 canonical KnowledgeRevision 后，在同一 PostgreSQL 事务中写入发布意图并异步投递；Fast Note Sync 故障不能回滚知识提交或阻塞教学。Obsidian 修改只通过显式预览、三方差异和审阅操作进入新的 canonical revision。

# Scope

本 child 固定 Fast Note Sync Service `3.6.1`（commit `7a6c78792c631f999c8a5f725bba5dd7235d6688`）和 Obsidian plugin `2.4.0`（commit `f2b15c09d34e621d2d97ad526fdee03460bac151`）的真实接口。生产 bridge 使用实际 REST 路由、`Authorization: Bearer <token>`、`X-Client: CLI` 和 `p:rest c:CLI f:note_r,note_w` 业务权限，不用虚构端点或只通过 fake 的空壳替代。PostgreSQL 继续拥有 canonical Markdown、stable identity、publication revision、review 和 Outbox；Fast Note Sync 只是外部 Markdown 副本。

Fast Note Sync `3.6.1` 没有原子 expected-version/CAS，WebSocket `baseHash` 检查也不在写锁内。按用户确认的 fail-closed 合同，本地旧 revision 永不发送；已观察到的 remote 漂移、删除或占用只生成 review，不自动覆盖；版本或 capability 不兼容时依赖降级并保留 Outbox。上游 GET/POST 之间无法消除的并发窗口必须如实记录，不能描述为强 CAS。

服务端提供 capability/readiness、异步发布、显式 preview/review/resolution 和 closed OpenAPI；在线 Go CLI 只调用这些应用用例。发布复用现有 Obsidian export，保留 document/node stable ID 与 `edu-agent-source-revision-id`。详细路由、状态、数据模型和分批设计见 `docs/design/notesync-bridge.md`。

# Non-goals

本 child 不实现或 fork Fast Note Sync，不直接访问其数据库，不自动合并冲突，不执行实时无条件双向覆盖，不自动删除或重命名用户的 Obsidian 文件，也不把 remote path、mtime、`version` 或 `contentHash` 当作 canonical 顺序或身份。

Fast Note Sync 中已经存在的外部副本不纳入服务端“已物理清除”声明。privacy barrier 必须取消旧 generation 待投递并阻止其复活，但外部 vault、副本历史和备份仍需要按外部系统边界处理，不能伪造成已由 edu-agent 擦除。

# Acceptance examples

- A1：兼容矩阵固定 Fast Note Sync Service `3.6.1` 与 Obsidian plugin `2.4.0` 的 tag/commit及实际 `GET /api/version`、`GET /api/health`、`GET /api/vault`、`GET /api/note`、`GET /api/notes` 和 `POST /api/note` 合同；升级、未知版本或未知业务 envelope 不自动视为兼容，候选必须通过真实上游服务而非仅 fake。
- A2：启用 bridge 必须提供安全校验后的base URL、server-only API token、vault和受管path prefix。token使用`Authorization: Bearer <token>`，要求`X-Client: CLI`、`p:rest c:CLI f:note_r,note_w`及configured vault限制，不返回HTTP/CLI、不进入日志、错误或导出；redirect和非授权明文连接fail closed。
- A3：canonical KnowledgeRevision 在同一 PostgreSQL 事务中为受影响文档写入带generation、单调revision和确定性幂等键的Outbox意图；外部调用只在提交后发生，Fast Note Sync故障不回滚知识revision或阻塞核心学习。
- A4：consumer发送前重新读取当前knowledge head、stable document、publication mapping和generation。旧revision或旧generation作为superseded no-op且不发网；首次发布只create-only，后续仅在remote exact content等于持久base或目标时收敛，漂移/删除/占用只生成review。
- A5：写入成功必须exact GET readback后才推进publication base；超时、EOF或response loss按unknown outcome读回对账。remote等于target才applied，等于base才可重试，其他内容转review，不允许blind retry或把HTTP 200当作业务成功。
- A6：publication mapping以stable document ID为主键，保存固定remote vault/path、已验证source/document revision、exact base Markdown和SHA-256。canonical path/title变化不改变身份或静默rename/delete；发布Markdown复用现有Obsidian export并保留stable ID及`edu-agent-source-revision-id`。
- A7：显式preview按指定path或受管prefix有界分页读取真实remote Markdown，生成不可变`base/local/remote` snapshot、line-level三方diff、source revision、identity和basis hash；扫描本身不创建KnowledgeRevision、不自动回写。
- A8：review只允许accept-remote、keep-canonical或用户提供merged Markdown。应用前重新核对generation、local head和remote hash；陈旧basis拒绝。accept/merge复用现有knowledge import、expected parent和identity review形成新immutable revision，不能走bridge专用后门。
- A9：notesync HTTP/CLI只调用应用用例并复用现有设备认证、knowledge scope、限流、审计、read permit和稳定错误；CLI不读取remote token、不直连Fast Note Sync、不在argv或本地状态保存Markdown。OpenAPI、CLI DTO、handler和migration保持closed contract。
- A10：bridge state和review正文服从knowledge privacy owner，Outbox服从generation fence；barrier后旧consumer、readback或review commit不能恢复正文。readiness独立报告notesync降级且不使PostgreSQL、知识、教学或查询整体not-ready，并明确外部vault副本不属于服务端清除证明。
- A11：固定上游候选场景验证Bearer认证、`p:rest c:CLI f:note_r,note_w`和vault限制、业务envelope、create-only、publish/readback、旧消息、remote drift、outage/response-loss和显式导入；fake/httptest只覆盖故障矩阵，不能替代真实Fast Note Sync `3.6.1`证据。
- A12：实现严格按S1真实client/contract、S2 outbound publication、S3 explicit import/review、S4 candidate integration推进。每批只运行能证伪当前行为的检查；真实Fast Note Sync、完整PostgreSQL、race和独立Verifier只在对应稳定边界运行一次并复用未失效证据。

# Constraints and invariants

PostgreSQL 是唯一 canonical 知识与同步状态源。remote Markdown 只有通过现有 knowledge import、expected-parent、identity-review 和 privacy generation 边界才能进入 canonical revision；outbound side effect 只能来自已提交 Outbox。

Fast Note Sync Service `3.6.1` 不提供原子条件写。bridge 只承诺旧本地 revision 不发送、已观察 remote 漂移不覆盖、unknown outcome 必须读回对账；获得真正 CAS 需要未来上游合同和新的 Shape 决策。

# Decisions

采用官方 REST，不采用未完整公开且同样非原子 CAS 的 WebSocket 插件内部协议；采用fail-closed remote preflight；首次remote path成为stable mapping；入站只通过显式preview/resolution；真实上游合同是候选门禁。

# Open questions

无。

# Verification expectations

S1只验证strict client、auth、URL和capability；S2验证transaction+Outbox+consumer及一个具名PostgreSQL纵向场景；S3验证preview/review/resolution、OpenAPI和CLI；S4才运行一次固定上游real-process合同、受影响race/vet/build和独立Verifier。环境或harness故障不扩展产品范围。
