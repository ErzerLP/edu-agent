# 可恢复导入任务（Issue #7）

## 基线与开发方案

前置基线为 `7ef99f9`。沿用 #6 的扫描、身份审阅、无写入规划、真实提交回执；不另建冲突规则。旧单请求限制已经通过现有容量测试确认，本项保留该限制。

一个任务固定设备、学习区、集合、完整来源清单和摘要。客户端自动分段上传；每文件仍限 4 MiB，每请求仍限 16 MiB，任务最多 1000 文件、128 MiB，服务器最多保留 32 个未清理任务。批次按文件形成，边界在最终确认前完整显示。任务有效期24小时，继续不延长有效期。

服务端 PostgreSQL 保存任务元数据、版本、批次原操作及结果；正文和规划输入通过每任务密钥加密暂存为服务器文件。数据库事务保护 generation、配额和版本；任务级数据库锁串行化上传、预览、确认、继续和取消。完成、取消、过期删除密钥并清理文件；文件删除失败明确呈现且可再次清理，隐私清除删除密钥和敏感元数据，旧任务不能恢复正文。

先按当前基线模拟全部剩余批次，复用既有规划器生成预览与确定性版本链。确认绑定完整清单、批次计划及预览版本。每次继续最多执行一个批次，HTTP连接不承载任务所有权。提交前保存未知状态；恢复先查同一 operation 的持久回执，再重试同一计划。外部版本变化停止剩余批次并要求重新预览确认。取消只停止后续批次，保留成功结果。

CLI 提供创建并自动上传、查询列表/详情、重连校验来源、预览、确认、继续、取消与清理。TUI 使用相同任务接口，复用扫描与差异展示；重启通过服务器列表找到原任务。客户端不落盘正文、凭据或恢复缓存；本地未上传文件不视为服务器备份。

## 验收标准

- 超过旧请求预算的合法资料集自动分批；上传失败以同一任务及摘要重试，不重复创建任务。
- 真实 PostgreSQL 和文件暂存重启后保留任务、逐项上传状态、预览与实际结果。
- 完整计划未经确认不发布；确认后每批原子提交，响应丢失先核对原操作，只有一份提交事实。
- 部分成功、未知、失败、未提交分别计数；取消不回滚成功批次；归档可查询和清理但禁止发布。
- 本地摘要变化必须重新建清单/预览；外部父版本变化使剩余确认失效。
- 配额、过期、暂存缺失、清理失败与隐私清除有明确结果，旧 generation 不得重新写回任务。
- CLI/TUI 查询同一持久状态，目标固定，迟到回调不会污染其他页面。
- 运行定向回归、受影响测试、真实数据库中断场景、build/vet；未运行验证单独记录。

## 使用与恢复

```sh
edu-agent --space 区ID knowledge import jobs new --collection 集合ID --id 任务UUID ./notes
edu-agent --space 区ID knowledge import jobs list --collection 集合ID
edu-agent --space 区ID knowledge import jobs resume --collection 集合ID --id 任务UUID ./notes
edu-agent --space 区ID knowledge import jobs preview --collection 集合ID --id 任务UUID
edu-agent --space 区ID knowledge import jobs batch --collection 集合ID --id 任务UUID --batch 0
edu-agent --space 区ID knowledge import jobs confirm --collection 集合ID --id 任务UUID --plan-version 1
edu-agent --space 区ID knowledge import jobs continue --collection 集合ID --id 任务UUID --all
edu-agent --space 区ID knowledge import jobs show --collection 集合ID --id 任务UUID
edu-agent --space 区ID knowledge import jobs browse --collection 集合ID
```

不提供 `--id` 时 new 会生成并先向 stderr 输出任务ID；JSON结果仅写 stdout。创建响应未知可重试同一ID和完整清单。上传使用base64传输，避免合法4 MiB文件因JSON转义超过16 MiB；暂存文件包含规划输入，独立上限32 MiB。任务来源摘要按转换后的准确导入正文计算，纯文本仍携带 #6 的原始来源摘要。

任务页 `n` 使用共享扫描向导创建，`u` 重新选择来源并校验全部原清单后续传，`p` 预览，左右键选择批次、`v` 查看差异或进入共享身份审阅，Ctrl+S确认完整计划，`r` 逐批推进。每个返回值更新真实进度，未知结果暂停后续批次，`g` 查询原操作。退出页面只停止后续客户端请求，不回滚服务器事务。`x` 取消/清理；当前请求仍占用任务锁时需等待该批结算再取消。单批CLI continue便于调度；`--all` 自动处理已授权剩余批次，结果未知立即停止。归档区允许GET与DELETE，不允许新的上传/确认/发布。

最终清单不可静默替换。缺失或变化的本地文件会在任何续传前被指出，已上传文件也参与来源核对；可恢复原文件继续，或为变更资料创建新任务重新预览。任务存在不代表本地未上传文件已备份。修改身份决定只影响尚未发布批次，每次规划先持久撤销旧授权，再写新的规划输入；服务重启根据服务端保存的审阅依据重新签发内部审阅凭据，用户批准的决定和版本链不变。

## 暂存、配额与清理

`IMPORT_JOB_STAGING_DIR` 可指定绝对目录，必须为真实目录、权限0700；文件0600。直接运行默认是服务器用户配置目录下的 `edu-agent/import-jobs`。仓库Docker镜像使用 `/var/lib/edu-agent/import-jobs`，Compose将其保存在已有服务器持久卷中；独立容器部署需要自行挂载该目录。进程重启可以复用，卷丢失返回missing并要求从原来源续传。禁止将它指向用户原资料目录。

每任务独立AES-GCM密钥保存在PostgreSQL，密文绑定任务及批次文件名，写入先fsync再原子rename并同步目录。没有客户端明文恢复文件。原始正文最多128 MiB、1000篇；32个持有暂存密钥的任务为全服务上限。JSON转义、规划和密文开销独立于原文字节配额，单暂存文件最多32 MiB。磁盘写入失败返回storage_unavailable，不计入已上传；同一摘要可重试。各文件解析继续使用既有canonical/parser预算。

既有应用worker每分钟回收24小时过期任务和未完成清理，启动时立即运行一次；它只清理，不扫描来源或自动发布。完成、取消、过期先持久关闭，再销毁密钥和删除文件；失败保留cleanup_pending以重试。残留临时密文超过一小时清理。到期七天后删除来源清单等敏感元数据，只留任务ID墓碑防止旧ID重建。正式知识版本与操作事实不因任务过期或取消而删除。

隐私knowledge owner清除任务state和密钥，残留验证纳入同一事务流程；旧generation写回失败，残留密文由清理worker删除。备份留存仍遵循项目既有隐私备份截止时间，清除在线密钥不宣称已删除外部备份。日志只记录错误分类，任务错误与列表不返回正文；单批预览沿用 #6 的有界正文/差异接口。

## 验收记录（2026-09-09）

环境：Linux、Go 1.26.6、独立PostgreSQL 17测试容器，镜像固定为项目既有digest `18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`。所有数据库包串行执行，未操作运行中的应用数据库。

已通过：

- server与cli-go分别运行 `go test ./...`、`go vet ./...`、`go build ./...`。其中未配置数据库的skip不作为持久化证据。
- 配置真实 `TEST_DATABASE_URL` 后运行 `go test -p=1 ./internal/knowledge/postgresstore ./internal/transport/httpapi ./migrations -count=1`。覆盖新增任务以及既有导入、范围、迁移与HTTP回归。
- `TestPostgreSQLImportJobRestartLostResponseAndLargeManifest`：五个合法文件累计超过16 MiB，预览不发布，加密文件跨服务实例恢复，提交后响应丢失先对账，最终恰好五条操作事实，完成删除暂存，跨设备查询拒绝。
- `TestPostgreSQLImportJobExternalWriteMissingStagingAndQuota`：外部版本推进撤销授权，旧plan_version拒绝，暂存丢失可从未变化来源重传，摘要变化拒绝，后台真实到期清理和任务配额拒绝。
- `TestPostgreSQLImportJobCancelStaleAndExpiry`、`TestPostgreSQLImportJobCleanupFailureAndConcurrentRequest`：取消保留既有结果，批次锁拒绝并发取消，文件删除失败仍销毁密钥并保留可恢复清理状态。上述小型边界场景及外部更新场景通过数据库定向race。
- `TestBarrierPersistsAcrossStepFailureAndLocalScrubResumes`：在真实隐私barrier及owner scrub中加入已上传任务，元数据和密钥被清除，旧generation写回失败，原密文无法解密且清理worker删除残留文件。
- `TestBlackBoxImportJobsProcessRestartAndLostResponse`：生产CLI/服务在非默认区、指定集合中创建、上传、重启、校验来源、预览、确认；代理丢弃已经提交的真实HTTP响应后查询原任务，后续进程重启继续恰好剩余批次。再次导入修改资料完成文档和章节两阶段身份审阅，重启真实进程后保留批准的决定，归档后查询和取消仍可用。全过程非TTY输出无全屏控制序列。
- `TestImportJobsPTYQueriesAndContinuesPersistentState`：Linux原生PTY中操作任务列表、详情、完整计划确认和继续，显示实际新增计数；HTTP为固定夹具，真实持久链路由上一项独立覆盖。
- CLI/API/扫描相关定向race、HTTP认证/审批/重复字段/预算测试、全清单来源核对与迟到回调测试。race曾发现测试计数器未同步，改用原子计数后精确回归通过。
- OpenAPI随server API测试加载验证；`docker compose --env-file deploy/env.example -f deploy/compose.yaml config --quiet` 和 `git diff --check`。

联调期间发现新任务路由未列入集合范围适配、空列表/审阅摘要与客户端严格JSON解码不匹配，均在生产黑盒中修复验证，未放宽范围保护或客户端通用解码规则。

未运行：全仓race、原生macOS/Windows交互、完整Compose启动/镜像重建、真实磁盘耗尽注入、真实外部模型/NoteSync服务。文件写入失败统一返回可重试的storage_unavailable，未将未运行的磁盘耗尽场景标为通过。容器持久路径已完成配置校验，实际进程重启和文件恢复由上述生产黑盒验证。
