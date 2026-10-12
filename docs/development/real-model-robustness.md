# 真实推理模型：开学、提示与评估可靠性

验收日期：2026-10-12。基线为 `main@493aaa5`，改动保留在 `fix-real-model-robustness` 工作树。

本地使用 `deepseek-v4-flash`，教学连接为 Responses，导师复用教学连接；输出预算 8192、空闲超时 60 秒、上下文设置 32768，模型并发为 1。验收服务使用端口 33500，数据库为独立验收容器内的 `robust_local`。未改默认输出预算、CLI、`packages/agentcore`、迁移或公共 API/OpenAPI，也未操作演示部署。

## 根因与修复

### 1. 开学综合耗尽输出预算

基线调用位置为 `server/internal/mentorrun/research.go:181`，准备活动为同版本 `server/internal/mentorrun/start.go:132`。两者直接使用旧导师 `callModel.Complete`，没有结构化输出格式，实际发到 Chat Completions；复用教学连接并没有让这两个调用使用教学的 Responses 格式。第一次失败发生在研究综合，还没有执行准备活动。

基线的两次综合调用均设置 `max_tokens=8192`，提供商报告输入 3502、输出 8192、其中推理 8192、总量 11694；耗时分别为 42.438 秒、41.709 秒，最终分类 `response_truncated`。没有发现单步拿到比设置值更小的输出上限，因而没有修改预算分配或提高默认值。

改用 Responses strict schema 后，宽泛综合仍出现过输出 8191、推理 7694、`output_budget_exceeded`，耗时 43.499 秒。这说明单独切换协议不能解决推理耗尽预算。

修复在 `server/internal/mentorrun/structured.go:65`：这两个服务端调用复用既有 `settings.StudioClient` 的有效导师连接与 wire API。Responses 使用 strict schema；Chat 路径使用 JSON object 并在本地检查相同生成 schema。只对开学综合收缩任务为一个最相关的基础要点、至多一条候选引用和必要缺口，不要求推导、证明、例题或全面综述；普通研究没有这个收缩指令（`:75`）。原综合及准备的领域校验继续执行。

每次请求仍经过 `server/internal/mentorrun/core.go:321` 的 HTTP 预算、配置、租约与权限门禁；schema 和反馈计入输入预留，输出使用设置值。已收到 usage 的成功或拒绝按实际总 token 结算，无 usage 的调用保留预留和未知成本。截断不会自动重复相同任务消耗预算。

### 2. 准备活动引用不是原文

加入候选但尚未约束 quote 时，真实模型的两次准备均返回 `citation_quote_mismatch`，耗时 17.163 秒、23.501 秒。来源修订和片段范围本身正确，错误发生在输出 `citations[].quote` 没有逐字复制。基线把严格解码和引用校验一起折叠成 `invalid_source_citation`，无法看到字段问题。

`server/internal/research/citation_support.go:20` 从本次可见片段生成候选：原来源、修订、片段 ID，加上 UTF-8 安全的最多 384 字节原文前缀、文档绝对字节范围及 SHA-256。综合输入位置为 `server/internal/mentorrun/research.go:158`，准备输入为 `server/internal/mentorrun/start.go:119`；准备只使用已经采用的来源。原片段仍在输入中。

`server/internal/mentorrun/structured.go:44` 把本次候选 quote 放入内部生成 schema 的 enum，要求模型照抄。模型仍需正确配对 ID 和 quote；`Synthesis.Validate` 及 `Prepared.Validate` 最终核验来源、修订、片段、原文与范围，候选不能替代校验。

`server/internal/research/types.go:25` 为这些拒绝提供固定分类，包括 `citation_source_missing`、`citation_revision_mismatch`、`citation_fragment_missing`、`citation_quote_mismatch`、`citation_range_mismatch`；仍可通过 `errors.Is(err, research.ErrPolicy)` 判断原错误。准备字段的长度和 UTF-8 检查保留，新增 `prepared_field_invalid`。逆序片段范围也明确拒绝。

可修复的结构或引用拒绝最多重试一次，总共两次调用。反馈仅含固定原因和修正指令；每次重新执行完整解码、schema 和领域校验，经过同一预算门禁。持续错误不会保存综合、发布活动或课堂。日志 `mentor_generation_rejected` 和 `mentor_run_failed` 提供 `rejection_reason`，不包含模型正文或引用文本。

### 3. 422 提案原因丢失；评估风险值超出领域允许列表

基线 `server/internal/learning/proposal.go:512` 把领域拒绝折叠成 `domain_invalid`，而提案生成/失败路径没有对应原因日志。修复后的 `proposalFailureCategory`（当前 `:523`）只保留服务端固定原因，其余归为固定兜底分类。`tutoring_proposal_attempt_rejected` 记录类型、次数和原因；`tutoring_proposal_failed` 记录最终分类与各次分类。模型前的请求校验拒绝也记录固定原因，如 `duplicate_node_revision`。既有提案运行记录保存生成的各次分类，不增加 API 字段，不记录正文、token 凭据或 lease。

在本地使用旧教学适配器诊断时，提示成功，但评估连续两次 422，固定原因都是 `unknown_risk_flag`。根因是基线 `server/internal/integrations/tutormodel/adapter.go:123` 的生成 schema 允许任意风险字符串，而 `server/internal/learning/assessment.go:59` 仅允许九个既定值。修复后的生成 schema（当前适配器 `:126`）枚举同一九个值，领域允许列表没有变化。

`server/internal/integrations/tutormodel/quote_support.go:38` 同时向活动、提示/自由回答、讲解和评估提供可复制引用，并过滤原 work item/retrieval 数组中的未选节点。候选区分两类范围：`reference.range` 是完整冻结 slice 的文档绝对范围、`slice_sha256` 是完整 slice 的 hash；评估 quote 的范围是 slice 内相对范围、hash 对应最多 2048 字节的 quote。提示明确要求复制，不计算或猜测。原冻结请求、输入 hash 和所选资料不因重试改变。

`server/internal/learning/proposal_retry.go:14` 将既有两次调用上限用于指定的可修复拒绝；不会再叠加另一组重试。反馈通过内部 context 传递。每次仍调用 `decodeProposal`、引用解析、冻结上下文检查与原领域评估；越出允许节点的拒绝不会靠重试扩大范围。

评估提示明确说明 confidence 的整数标度为 0–1000，没有转换模型输出。850 的接受阈值、帮助等级和冻结 rubric 保持原样。原演示中的“要提示”422在本地旧适配器实验中未复现，不能把它断言为某个字段；现在具备固定日志分类、候选和有界修正，后续出现时可直接定位。

### 4. Wikipedia 正文被导航挤占

基线 `server/internal/research/fetch.go:275` 将整个 HTML 中的可见文本依次拼接，然后在 `:246` 截到 16000 字节。侧栏、导航和目录排在正文前面，消耗了正文预算。

当前 `parseHTML`（`:278`）优先内容区，其次 article/main，再退回可见正文；移除 nav、目录、页脚、侧栏、隐藏元素及原有脚本等。`htmlContentArea`（`:375`）识别 Wikipedia 的 `mw-content-text`、`mw-parser-output` 和常见文章内容区。仍扫描页面中的存储限制 meta，不能因主内容选择忽略外部 `noarchive`。

网络公网地址检查、DNS 固定、重定向、存储策略、128 层深度、1 MiB wire、2 MiB decoded 和 16000 字节正文限制均保留。HTML parser 标记为 `safe-main-text-v2`；plain text 保持原标记。

对指定 URL 的真实抓取结果：

| 版本 | 正文字节 | 前 350 字符是否出现导航标记 | 含线性组合定义 | 覆盖 |
| --- | ---: | --- | --- | --- |
| 基线 `safe-text-v1` | 16000 | 是 | 是 | `partial_text_limit` |
| 修复 `safe-main-text-v2` | 16000 | 否 | 是 | `partial_text_limit` |

定义原来并非完全不存在；修复改善的是导航占比和正文覆盖，没有把有限文本抓取标成完整覆盖。

### 5. 拒绝调用的 token 不可见

基线 `server/internal/integrations/llm/client.go:262`、`responses.go:60` 在截断或 schema 拒绝时丢弃已经收到的 usage。现在仍拒绝输出、返回空 JSON，同时保留输入、输出、推理、总 token。新增 `mentor_model_completed`（含调用阶段、尝试次数和输出预算）与 `tutor_model_completed`，只记录耗时、计数和固定分类。

Responses 的 JSON 客户端还拒绝文本之外的工具/动作输出（`responses.go:74`），防止合法 JSON 夹带工具调用后被当作完整提案。没有执行模型工具或修改 agentcore 协议。

## 检查

在 `server` 目录执行 Go 检查，先执行 `umask 022`，并设置 `GOCACHE=/tmp/robust/go-cache`。

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable \
go test -p=1 ./internal/mentorrun ./internal/learningstart ./internal/learning \
  ./internal/tutoring ./internal/integrations/tutormodel ./internal/integrations/llm \
  ./internal/research ./internal/settings

go vet ./internal/mentorrun ./internal/learningstart ./internal/learning \
  ./internal/tutoring ./internal/integrations/tutormodel ./internal/integrations/llm \
  ./internal/research ./internal/settings
go build ./cmd/edu-agentd
```

最后一轮八个包的 `go test` 全部通过：mentorrun 的真实 PostgreSQL 回归为 485.784 秒，settings 为 23.089 秒，其余六个包复用相同文件状态下的有效缓存结果。八个包的 `go vet`、不带验收 overlay 的服务构建、改动 Go 文件的 gofmt 检查和 `git diff --check` 均通过。改动文件与日志的真实密钥匹配检查为零匹配。

首次全包检查发现旧夹具只供给一次错误响应，以及参考资料夹具引用完整长片段而未复制生成候选；已更新夹具，保留并加强拒绝后无发布、无证据、无冻结状态修改的断言。新增工具调用拒绝和模型前拒绝日志用例也先观察到失败，再修正并通过。Go 回归使用本地假模型检验协议、字段及预算边界，真实模型调用独立见下文。

重点回归：

- `TestPostgreSQLStartCitationRetryRevalidatesAndChargesBudget`：修正成功、第二次仍错误、请求预算不足三条路径；原文/未选资料隔离、固定反馈、实际 token 结算。
- `TestProposalRepairFeedbackPreservesFrozenRequestAndFullValidation`：重试输入和 hash 不变；第二次 schema 错误或越出节点仍拒绝。
- `TestStrictFakeAssessmentModelFailureAndDecodeMatrix`：未知风险、rubric、范围、hash 与跨修订引用在重试后仍拒绝；未选节点立即拒绝；不产生证据或 canonical 事件。
- `TestReferenceSupportCopiesDocumentRangeAndExcludesUnselectedText`：四类提案都只发送选中资料，完整文档范围与相对 quote 范围不混用。
- quote enum、风险 enum、固定原因和日志脱敏回归；准备字段原长度限制；普通研究不会被开学压缩指令改变。
- `TestProposalRequestRejectionLogsFixedReasonBeforeModel`：模型前 422 也有固定原因，模型零调用，日志不包含节点内容。
- `TestRejectedOutputRetainsUsageWithoutJSON`：Chat/Responses 截断分类、schema 拒绝和工具调用拒绝，均保留 usage、不给无效 JSON。
- HTML 主内容/导航/目录剔除，以及主体之外存储限制的拒绝回归。

## 连续两次真实端到端

使用正式 API 和普通设备授权，从两个新目标分别执行：开学自动研究与采用 → 发布活动并进入课堂 → 要提示并应用自由回答 → 恢复活动 → 以 hint 帮助等级提交答案 → 生成并记录评估 → 查询 `Feedback`。没有直接写入课堂状态或放宽任何校验。

| 轮次 | 开学 | 提示 | 提交 | 评估并记录 | 全程 | 结果 |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| final-1 | 53.920 s | 6.320 s | 0.467 s | 14.663 s | 76.170 s | `succeeded` → `Feedback` |
| final-2 | 56.341 s | 6.212 s | 0.461 s | 11.739 s | 75.695 s | `succeeded` → `Feedback` |

全程时间包含建目标、准备动作和查询等。两轮各四次模型调用，所有步骤第一次生成就通过，没有用重试掩盖失败。开学各消耗五个运行请求（一次 Brave、两次网页读取、两次模型），模型实际总 token 分别为 17922、19046，对应开学预算余额 82078、80954。

| 轮次/调用 | 输入 | 输出（含推理） | 其中推理 | 总 token | 模型耗时 |
| --- | ---: | ---: | ---: | ---: | ---: |
| final-1 综合 | 6343 | 1414 | 1102 | 7757 | 9.585 s |
| final-1 准备 | 3773 | 6392 | 5896 | 10165 | 34.200 s |
| final-1 提示 | 23141 | 575 | 325 | 23716 | 5.243 s |
| final-1 评估 | 24629 | 2998 | 2059 | 27627 | 13.988 s |
| **final-1 合计** | **57886** | **11379** | **9382** | **69265** | |
| final-2 综合 | 6355 | 3957 | 3640 | 10312 | 23.500 s |
| final-2 准备 | 3808 | 4926 | 4560 | 8734 | 27.953 s |
| final-2 提示 | 22462 | 593 | 343 | 23055 | 4.964 s |
| final-2 评估 | 23964 | 2299 | 1353 | 26263 | 11.112 s |
| **final-2 合计** | **56589** | **11775** | **9896** | **68364** | |

推理 token 已包含在输出中，不能再叠加到总量。两轮评估置信度都是 720，按原规则保存为 `provisional` 并提供反馈；第二轮还报告 `insufficient_answer_evidence`。课堂完成流程不等同于接受能力证据，未自动提升置信度或掌握度。

本轮诊断及验收共 20 次真实模型请求、5 次 Brave 搜索，低于 40/6 上限。其中一次旧进程切换期间的中断请求计入次数，未收到可用 usage，不能将其成本写成零；其余 19 次有 usage。临时 HTTP 计数/usage overlay 只保存在 `/tmp/robust`，不修改 wire 请求和模型输出，不纳入仓库。凭据及正文快照只用于权限受限的本地验收，文档和摘录不包含这些数据。

## 改动文件

- `server/internal/mentorrun/{structured.go,research.go,start.go,worker.go}`：结构化引用生成、候选、预算与诊断；`structured_retry_test.go`、`prepared_schema_test.go`、`research_test.go`、`worker_diagnostics_test.go` 验证重试、范围隔离及诊断。
- `server/internal/learning/{proposal.go,proposal_retry.go}`：固定拒绝分类与一次修正重试；`proposal_diagnostics_test.go`、`proposal_retry_test.go`、`proposal_test.go`、`proposal_audit_test.go`、`strict_fake_model_test.go` 验证完整校验及冻结状态。
- `server/internal/learningstart/{start.go,start_test.go}`：准备字段拒绝分类，保留长度/编码限制。
- `server/internal/integrations/tutormodel/{adapter.go,quote_support.go}`：候选、固定反馈、风险 enum 和 usage 日志；`reference_support_test.go`、`risk_schema_test.go` 为回归。
- `server/internal/integrations/llm/{client.go,responses.go,rejected_usage_test.go}`：拒绝仍保留 usage、工具输出仍拒绝。
- `server/internal/research/{types.go,citation_support.go,fetch.go}`：引用分类/候选、主体提取；`citation_diagnostics_test.go`、`fetch_test.go` 为回归。
- 本文。

## 人工关注与限制

- 两次同一主题的 API 验收不能证明其他主题、其他提供商或所有随机生成都可靠；结构/引用失败仍可能在一次修正后正确拒绝。截断仍会报告真实失败，8192 不保证任何长度的推理都能完成。
- HTML 仅做文本提取，公式、复杂布局和页面尾部可能缺失；本例仍为 `partial_text_limit`。未为改善覆盖绕过大小、网络或存储限制。
- 候选是有限原文前缀。模型必须选择与结论相关的候选；存在候选不意味着结论正确或证据足够。更广泛的课程资料与引用覆盖仍需教学内容审阅。
- 两轮反馈均为临时评估；后续接受证据仍走原有人工决策/证据流程。
- 未进行浏览器界面验收。API 路径覆盖了这三个后端目标；原演示提示 422 的精确历史字段仍无法从旧日志反推。

已停止验收服务，确认端口 33500 无监听；已删除 `robust_local`，确认容器中没有本任务的 `robust_` 数据库。验收创建的凭据、配置、加密 key 和正文快照已从临时目录清除，保留脱敏计数、耗时和检查日志。未删除或修改共享的 `edu_agent_test` 数据库；数据库测试只创建并清理各自 schema。

无需要用户决定的实现问题；未暂存、提交、推送或部署。
