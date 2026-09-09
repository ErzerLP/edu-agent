# 有界资料导入向导（Issue #6）

## 基线与范围

已确认旧入口只有路径表单、Markdown 全批扫描和直接提交。此次在 `0fd3bcf` 的学习区、集合和目标前置上交付一个完整垂直批次，不建设持久任务、跨进程续传或后台扫描。

## 开发方案

1. 客户端扫描明确选择的目录或文件，保留逐项状态、相对路径、大小及原因。支持 UTF-8 Markdown、UTF-8 纯文本与粘贴文本；纯文本用代码块保留原文，来源包含类型、相对名称及原始内容摘要。沿用安全文件读取及单次预算。
2. 服务端通过无写入适配器复用 Import 身份、canonical 与 lineage 规则，输出新增、更新、未变化、正文/章节差异与关联影响。预览不发布版本；维护提案的自动应用策略不用于此入口。
3. 预览回执签名绑定设备、学习区、集合、完整输入、身份决定、父版本、隐私 generation、策略及有效期。同一 operation 的规划使用确定性 ID；提交重新核对签名并在原事务内核对父版本和 generation。原子提交回执保留真实计数，支持按原 operation 核对，网络未知不得换 operation 自动重传。
4. 独立 Bubble Tea 全屏页面依次提供目标、来源、扫描清单、冲突、预览确认和结果。异步消息绑定当前草稿代次，请求可取消；返回保留输入，过期确认作废。目录可浏览、多选与排除，长正文/候选可滚动与搜索。
5. 保留旧脚本入口，新增显式 JSON 预览、提交和 operation 查询。非 TTY 不启动向导、不等待身份输入；脚本通过结构化身份决定重新预览。结果链接资料页及已有目标导航，导入本身不创建目标或证据。

## 验收标准

- 扫描混合有效、排除、不支持、读取/编码/路径/预算错误，每项可定位；最终请求只包含选择项。
- Markdown、纯文本、粘贴完成导入；原文可追溯，不发送客户端绝对路径。
- 预览不改变 head；新增、更新、未变化与冲突可区分，身份不由文件名继承。
- 修改内容、目标、身份决定、父版本或隐私 generation 后拒绝旧回执。
- 有界批次真实 PostgreSQL 原子提交/回滚；重试与查询保持相同 operation、版本及计数。
- TUI 返回/错误保留草稿，取消和迟到消息不污染后续页面；长列表与差异可实际查看。
- CLI 非 TTY 无全屏序列和交互等待；结果可在集合资料页读取，目标导航不自动创建目标。
- 通过定向回归、受影响包、build/vet，记录真实 PostgreSQL 与 Linux/macOS 原生交互的实际覆盖。

## 实现约束与回执

预览不保存正文任务，不复用会自动应用的 maintenance 提案。无写入适配器捕获 canonical 计划；由 operation 派生的确定性 UUID 使预览和确认使用相同身份分配。确认回执通过 HMAC 绑定完整请求、设备、目标、策略、有效期和知识隐私 generation；提交重新走相同规划，并在真实写入事务中核对父版本和 generation。预览关联影响只展示，不自动改写既有学习证据。

迁移16仅为既有 `knowledge_import_operations` 添加 summary，保存原 operation、设备、学习区、集合、文档稳定 ID 和计数，不保存文件路径、正文或任务。它与正文、head 在同一事务写入；查询继续受集合引用与知识隐私门控制。新 HTTP 确认/查询响应只包含版本元数据和 summary，不随集合增长返回全部正文。无效或过期确认不会重新分配 operation 自动重传；已提交的准确请求先查持久回执，服务重启后仍可核对。

TUI 每次运行拥有独立程序与 context；每个异步请求附带递增代次。退出取消 context，取消请求同时淘汰旧代次；迟到结果不能写入下一页面。目标副本随草稿冻结，目标导航使用本次成功文档的明确版本范围，不把整个集合或排除项自动加入目标。

## 验收记录（2026-09-09）

环境为 Linux / Go 1.26.6，更新前置后基线 `0fd3bcf`。隔离 PostgreSQL 17 使用本机已有镜像 `postgres@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`，测试在独立容器与 schema 中运行，不操作现有服务数据。

已通过：

- server 与 cli-go 分别执行 `go test ./...`、`go vet ./...`、`go build ./...`；后续局部调整仅重跑相关 command 检查。
- 真实数据库串行执行 `go test -p=1 ./internal/knowledge/postgresstore ./internal/transport/httpapi ./migrations -count=1`，全部通过。新增导入场景另通过定向 `-race`。
- `TestPostgreSQLImportPreviewConfirmAndReplay`：非默认学习区/集合中预览零版本写入，修改正文/设备旧确认失效，原子提交、未变化、明确章节改写、父版本冲突、重启服务对象后的同操作回执与查询。
- `TestPostgreSQLImportConfirmRollsBackWholeBatch`：事务内 generation 不符被拒绝；回执写入注入失败时，head 和全部正文回滚，原请求重试成功。
- `TestImportPreviewReceiptBindsInputTargetGenerationAndExpiry`：输入、身份决定、设备、学习区、集合、generation 和过期时间均不能复用旧确认。
- 收尾回归补充：保留正文版本但移动路径计为更新；`TestImportConfirmationReturnsConcurrentCompletedOperation` 精确模拟首次查回执后另一请求提交相同操作，返回已完成回执而非迟到的父版本冲突。后者与签名边界测试通过定向 race。
- `TestImportPreviewHTTPScopesClosedInputAndLimits`：预览无需审批权限，正式提交要求审批；认证注入设备，拒绝伪造字段、重复字段、缺少父版本及超限请求。
- `TestScanReportsEveryFileAndKeepsOnlySelection`、`TestScanCancellationAndPathCollision`：混合文件报告、排除、编码错误、文本原文与来源、取消及转换路径冲突。
- `TestImportDraftSelectionAndLateResults`、`TestImportUnknownOutcomeKeepsOriginalOperation`、`TestImportFailedReviewKeepsFilesAndSearchSelectsVisibleItem`、`TestImportNonTTYNeverReadsInteractiveInput`：选择、迟到结果、未知提交、错误返回、搜索选择及非 TTY；这些测试和 PTY 测试均通过定向 race。
- `TestImportWizardPTYScanPreviewConfirm`：Linux 原生 PTY 中分别完成 Markdown 扫描与粘贴原文、预览、Ctrl+S 确认、结果页、Esc 退出及全屏控制序列检查。HTTP 使用固定夹具，真实持久链路由下一项单独覆盖。
- `TestBlackBoxImportPreviewConfirmedBatchAndOperation`：生产 CLI、生产服务与真实 PostgreSQL 完成建区建集合、Markdown/纯文本混合扫描、预览零写入、确认、服务重启、原操作查询/重放、资料页正文与来源一致；没有创建目标。宿主机未装 psql，首次检查因此未执行；随后使用已有 PostgreSQL 镜像提供临时 psql 客户端完成验证，没有安装软件。
- macOS arm64 客户端交叉构建与 `git diff --check`。

未运行原生 macOS 交互、真实外部模型/NoteSync 服务、全仓 race。现有 CI 的 Linux/macOS 全 CLI 测试会包含新增 PTY 场景，但此次未推送，未声称远端 CI 已通过。交叉构建不作为原生 macOS 验收证据。

单次选择使用扫描时的内容快照；重新扫描、改路径或换目标会作废旧确认。服务重启后未提交预览需要重做，跨进程任务恢复不属于此次交付。正文或差异达到既有展示预算时明确提示，并提供资料详情入口；不放宽批次或服务端资源上限。
