# 文件副作用有界日志（缺陷 #17）

## 范围与合同

本设计仅修复同一个稳定 checkpoint 之前，后一个文件 WAL 或 preference marker 覆盖前一个文件副作用的单槽缺陷。适用 [mkdir A8–A10](../comet/specs/client-file-mkdir/spec.md)、[copy](../comet/specs/client-file-copy/spec.md)、[move](../comet/specs/client-file-move/spec.md) 及既有 write/edit/archive 恢复合同。

不增加文件工具、事务、回滚、锁、全工具调用次数上限、配额、文件执行能力或偏好授权。workspace/securefile 的 Prepare/Commit、安全检查和发布结果语义不变；恢复只记录、展示和失效观察，绝不执行文件操作。

## 数据与版本

- **dirty payload v6**：在原有身份、base revision、turn sequence、当前 `file` WAL 与 `preference` 之外，增加 `file_journal`。
- **record payload v6 不变**：继续使用既有 `file_receipts`。
- **dirty/record 加密容器 v1、checkpoint v1 不变**。
- `FileJournalEntry` 保存 `write_ahead`、可选 `result`（既有严格 `FileReceipt`）和 `unchanged` 标记。
- `file` 是当前尚未结算的 WAL；开始下一个文件 WAL 前，将它原样移入日志。已结算的文件项直接进入日志。
- 没有 `result` 且非 `unchanged` 表示只有 WAL，按 unknown 恢复。
- `result` 精确保留 completed/unknown、实际目标 hash 和已知 mkdir 创建前缀。
- `unchanged` 是执行器已确认无效果的终态，不生成 completed 或 unknown 文件回执。该身份保留到稳定边界，防止同一 call 再次执行。
- v6 允许 preference marker 与文件证据共存；更新 preference 不清文件项，更新文件也不清 preference。未重构 preference 自身的单项恢复协议。

dirty v5 通过独立 `dirtyPayloadV5` 冻结；嵌套 effect/endpoint/directory DTO 使用已冻结的字段形状和 C8 闭合操作集合。v1–v5 均拒绝 `file_journal` 等新字段，即使值为 null、空数组、空对象或空字符串；保持旧 preference/file 互斥校验，防止旧 payload 夹带 v6 合同。v5 解码后的版本还必须匹配版本探针，不能通过大小写别名越过旧 DTO。

加载旧 marker 只在内存迁移，保留其确实存在的 singleton，不重写原加密证据，也不构造旧版本已经丢失的早期事实。下一次显式 UpdateDirty 才写 v6。认证后的未来 payload 仍为 `ErrVersionUnsupported`，Load/Save/UpdateDirty 都不可消费或覆盖。

## 持久化顺序

1. `BeginTurn` 创建原有 dirty 身份，绑定稳定 record revision 和 turn sequence。
2. `BeforeFilePublication`：拒绝当前日志/回执中的重复 call ID 和文件/偏好身份冲突；检查未 checkpoint 日志项数；保留此前全部项后，用 `Handle.UpdateDirty` 发布新 WAL。
3. 只有 WAL 持久成功才调用原有 `Workspace.CommitMutation`。
4. 执行器返回后，Loop 同步调用新 `DurabilitySink.AfterFilePublication(ctx, callID, workspace.Result)`。调用在工具结果投影、后续模型请求或下一工具之前发生。
5. 回调使用 `context.WithoutCancel` 加 5 秒超时，保证前台晚取消不会把实际发布结果直接丢掉；超时仍是可见的持久失败，不是成功。
6. Controller 只接受当前 WAL 的 call ID，将执行器真实结果与 WAL 计划匹配后，原子更新为日志结算项；然后 Loop 才继续原有投影、效果标记、事件和后续执行。
7. 稳定保存、正常关闭及中断恢复消费 dirty 时统一合并整个文件日志到 `FileReceipts`；合并发生在 `Handle.Save` 发布与消费 dirty 之前。

不在多工具中途导出假稳定 checkpoint，不推进 base revision，也不引入“旧 checkpoint 配新 dirty revision”的中间状态。已有 root-safe 加密替换、raw-byte CAS、发布 unknown 后重读对账保持原路径。

## 结果证明没有降低

旧单槽实现依赖稳定 turn 的最后一个 `FileEffectCallID`、事件和 checkpoint 引用，无法证明同一 turn 的早期多项结果。新生产路径改为**执行后、继续前的直接可信结算**：

- 结算来源是 Loop 刚调用的执行器 `workspace.Result`，不是模型正文、压缩历史或不完整事件。
- 当前 dirty 身份绑定 turn；当前 WAL 绑定唯一 call 和冻结 plan。不能结算不存在的 call、旧 call 或重用 call 执行新操作。
- 引用的 path/kind 必须与 WAL 完全匹配。
- 有 effect 时必须 `Validate` 且 `SamePlan`；mkdir/copy/move 仍强制完整实际 effect。
- copy completed 仍要求实际目标 hash 与执行器引用一致；move 仍禁止目标 hash 并要求双端失效。
- Store 再按既有严格 receipt 合同验证结果，要求相同 call/plan、mkdir 创建前缀不回退。completed mkdir 必须全部创建，partial 不能伪报 completed。
- write/edit/archive 维持原先允许实际引用补足旧式结果的兼容边界，不把模型文本变成证据。
- 原 `fileReceiptFromCheckpoint` 严格门禁保留，其 copy/move 等负向合同测试不削弱；新多项生产结算不再依赖单槽字段。

`UpdateDirty` 另验证文件证据单调演进：旧 WAL 必须仍在；只能给未结算 WAL 加终态，不能删除、更改计划、把 completed 降为 unknown、抹掉已知前缀或用陈旧候选覆盖结算。Controller 用值拷贝/新切片构造候选，失败不会修改内存中的旧证据。

## 容量、失败与停止

配额仍是 **dirty 16 KiB、ReceiptCount 32、NoticeCount 32**，其他 Session/Transcript/Checkpoint 配额不变。

- 当前 WAL + 日志项占用现有 receipt 数量配额；字节数仍由 UpdateDirty 在发布前检查。实际可容纳数量取决于路径和记录大小，并非新增固定全工具调用次数限制。
- 不对未 checkpoint 项使用截尾追加。不允许为了下一个变更删除旧 WAL、旧 settlement 或 unchanged 身份。
- 一次待消费批次最多 ReceiptCount 项。因此合并到 bounded FileReceipts 后，整个当前批次仍能保留；只有已经 checkpoint 的历史 receipt 可按原有保留策略老化。
- 结算本身可能比 WAL 大；若它超过 16 KiB，已发生的当前效果仍由原 WAL 保守记录为 unknown，并立即停止。不会因为结算容量不足而继续下一副作用。
- WAL/结算失败锁存 `fileJournalErr` 与现有 save failure。后续文件/偏好门禁失败；普通 finish/shutdown 不得消费该 dirty。即使普通关闭被调用，也保留磁盘上的旧证据。
- 含未消费文件证据的持久 session 在 checkpoint 保存失败时，不可降级成继续写文件的非持久 session；已存在的原生非持久 session 可用性不变。
- 本实现不尝试中途刷新 checkpoint 来释放容量。恢复到稳定边界后才释放 dirty 日志。

## 崩溃与恢复矩阵

| 停止位置 | 恢复事实 | 文件操作 |
| --- | --- | --- |
| WAL 之前 | 无本次文件效果 | 不执行 |
| WAL 成功、Commit 之前 | unknown，计划路径可能发生 | 不重放 |
| Commit 返回、settlement 未持久 | unknown；只有 WAL 时 Created=0 不等于未发生 | 不重放、不回滚 |
| completed settlement 持久、后续 WAL/偏好/模型/保存失败 | 保留对应 completed 与实际结果 | 不重放 |
| partial mkdir unknown settlement 持久 | 保留准确已知创建前缀，剩余计划仍可能发生 | 不删除目录、不 retry |
| unchanged settlement 持久 | 不生成文件效果回执 | 不重放 |
| dirty 消费前 Save 失败 | 旧 checkpoint 与完整旧 dirty 仍可恢复 | 不继续副作用 |
| 稳定 Save 成功 | record 中保留本批多个回执，dirty 按原消费协议清除 | 不重放 |

恢复逐项失效相关观察，并逐项保留回执。状态提示区沿用 NoticeCount 有界展示；文件证据本身不能因提示区老化而被删。正常持久 transcript 的文件 notice 保持原有短文案，避免重复展开长路径造成新的单条展示预算回归；完整路径仍在 effect/receipt 中。

## 验证边界

专用测试：

- `agentcontroller/consecutive_file_wal_test.go`：原父代理失败回归，alpha 已落盘、beta 仅 WAL，恢复两项 unknown，beta 不重放。
- `agentcontroller/file_journal_test.go`：真实 Loop/Controller 多 mkdir、mkdir+copy、模型失败、晚取消、实际 partial mkdir；下一 WAL 提交前/后崩溃；文件与 preference marker 共存及偏好仍需显式授权；数量/字节/结算 I/O 失败阻止下一副作用；重复 call、冲突 plan/引用、缺完整结果和 unchanged。
- `agentsession/file_journal_test.go`：旧证据不被删除/改写，容量失败字节不变，发布 unknown 对账，v5 迁移不重写，v1–v5 拒绝新字段空值夹带，未来认证版本不可 Save/Update/消费。
- `agentsession/archive_test.go` 的旧迁移 fixture 原先直接丢 singleton；改为先断言这种覆盖被拒绝，再保留旧 WAL 和旧 unknown receipt 后增加 archive，未放松迁移或结果断言。
- `agentsession/move_test.go` 更新 live dirty 版本与 future=当前+1，record v6 和容器 v1 断言不变。

完整受影响三个package已串行通过一次；最后局部收尾只复核相关具名集合，复用未失效整包证据。精确命令、结果和输入摘要见`/tmp/edu-agent-file-journal-handoff.md`。父代理随后补验受影响包vet、Consecutive/Journal/Durability定向race、agentui及command/dashboard整包，均通过；最终Linux CLI构建和Darwin/Windows amd64交叉构建通过。未运行AIX、非Linux原生、数据库、Compose或真实provider。
