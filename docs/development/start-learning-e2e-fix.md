# “开始学习”真实推理模型修复与验收

验收日期：2026-10-12。工作树：`start-e2e`，分支：`fix-start-learning-e2e`，基线：`main@8e4e2b6`。保留上一轮未提交修改，本轮未暂存、提交或推送。

## 根因链路

1. 开学研究综合在 `server/internal/mentorrun/research.go:181`、课堂准备在 `server/internal/mentorrun/start.go:132` 调用非流式模型。此次真实模型为 `deepseek-v4-flash`，推理阶段可能一直不发送响应头。
2. `server/internal/settings/service.go:345` 将配置的空闲超时交给导师客户端，`:355` 同样配置教学客户端；但旧版 `server/internal/settings/network.go:43` 的 `modelTransport()` 同时设置了固定 `ResponseHeaderTimeout: 15s`。因此配置的 60 秒等待尚未用完，请求已在约 15–20 秒被传输层截断。
3. `packages/agentcore/modelclient/client.go:107` 设置请求截止时间，`:118` 将传输错误归一化为“无法连接模型服务”。旧版 `server/internal/mentorrun/core.go:367` 没有保存底层错误类型，工作器只能记录通用 `model_failed`，掩盖了响应头超时。
4. 收尾时另发现评估阻碍：模型需要返回严格匹配的引用 ID、切片相对字节范围及 SHA-256。原通用提示没有给出可直接复制的完整候选；面对 18,304 字节资料切片，真实返回出现全部输出预算用于推理，或引用 ID 错误被服务端拒绝。增加预算到 16,384 仍出现耗尽，因此预算增加不能单独解决此问题。

旧 transport/core/worker 用只读基线副本与 Go overlay 复现：16 秒延迟响应头的三种客户端均失败；Complete/Stream 的超时类型和新增日志分类回归也失败。overlay 全部位于 `/tmp/start-e2e/`，没有回滚或替换工作树。

## 修复范围与校验边界

| 文件 | 变更 |
| --- | --- |
| `server/internal/settings/network.go` | 模型 transport 移除固定响应头超时，由已有客户端配置截止时间控制；搜索 transport 明确保留 15 秒响应头超时。 |
| `server/internal/settings/model_timeout_test.go` | 三种路径（导师 Chat、教学 Chat、教学 Responses）等待 16 秒仍成功；配置 5 秒超时则在 5–6 秒内终止。 |
| `server/internal/mentorrun/core.go` | 在底层 RoundTrip 失败时保存原始错误，供已有 Complete/Stream 返回路径保留错误类型。 |
| `server/internal/mentorrun/worker.go` | 诊断增加网络超时/不可用、内容/研究错误及白名单内的模型错误分类；只输出固定分类，无原始错误或响应正文。 |
| `server/internal/mentorrun/worker_diagnostics_test.go` | 覆盖包装的 DNS/模型/领域错误、不泄露哨兵字符串，以及 PostgreSQL 执行路径的 Complete/Stream 超时。 |
| `server/internal/integrations/tutormodel/adapter.go` | 仅在评估模型输入中派生 `assessment_quote_support`；提供答案原文和摘要、允许资料 ID、相对字节范围和引用摘要，避免模型计算哈希或猜测 ID。 |
| `server/internal/integrations/tutormodel/assessment_support_test.go` | 经过真实 llm 结构校验和领域评估，覆盖中文答案、文档偏移与切片相对偏移、长 UTF-8 资料截取，以及模型/输入哈希篡改被拒绝。 |
| `docs/development/start-learning-e2e-fix.md` | 本验收记录。 |

`modelTransport()` 当前实现在 `network.go:46`；拨号及 TLS 握手仍为 5 秒，仍禁用环境代理，重定向拒绝及模型端点允许列表未变。搜索公共地址、DNS 重绑定防护和 15 秒响应头限制均保留（`network.go:50`）。

原始传输错误保存在 `core.go:369`，已有 Complete/Stream 分别在 `:398`、`:426` 取回；固定分类日志在 `worker.go:238`。预算、授权、配置指纹和执行租约检查保持原有语义。

引用候选实现见 `adapter.go:41`：答案范围为完整 UTF-8 字节范围；资料不超过 2,048 字节时使用冻结切片及摘要，较长资料使用不超过 2,048 字节的 UTF-8 完整字符前缀，并计算该前缀摘要（`:54`）。完整资料仍保留在原模型上下文。提示明确候选存在不代表通过，资料不足应如实报告；上下文和候选均为数据。

这只是模型提示输入，不修补模型输出，不新增公共字段，不修改公共 API、OpenAPI、migration、CLI 行为或 agentcore 协议。领域仍核验答案引用（`server/internal/learning/assessment.go:116`）、允许资料 ID（`:129`）、知识引用（`:137`）以及范围、原文和 SHA-256（`:164`）。篡改哈希的新增测试仍得到 `invalid_knowledge_quote`。低置信度仍按原阈值 850/1000 进入待复核（`:147`）。

## 检查结果

所有以下 Go 检查均先执行 `umask 022`，工作目录为 `server`。使用 `/tmp/start-e2e/go-cache`；数据库测试指向独立的 `start_e2e_tests`，未使用演示数据库。

```sh
go test ./internal/settings ./internal/mentorrun ./internal/learningstart
go vet ./internal/settings ./internal/mentorrun ./internal/learningstart
go test ./internal/integrations/tutormodel
go vet ./internal/integrations/tutormodel
```

| 检查 | 结果 |
| --- | --- |
| settings 测试 | 通过，23.145 秒；包括延迟响应头及配置截止时间。 |
| mentorrun 测试 | 通过，421.038 秒；连接真实本地 PostgreSQL，新增传输回归未跳过。 |
| learningstart 测试 | 通过，0.005 秒。 |
| tutormodel 测试 | 最终文件状态通过，0.024 秒；包括有效引用及篡改拒绝。 |
| 上述四个包 go vet | 通过，无输出。 |
| 七个改动 Go 文件 gofmt -l | 无输出。 |
| git diff --check | 通过。 |
| 基线 overlay 回归 | 如预期失败，证明覆盖旧响应头限制及错误类型丢失。 |

三个指定包不依赖 tutormodel；后续适配器变化由单独的测试和 vet 验证，未无意义地重复七分钟的 PostgreSQL 套件。安全摘要与检查日志位于 `/tmp/start-e2e/finalize-acceptance-summary.json`、`finalize-go-test.log`、`finalize-go-vet.log`、`finalize-tutormodel-test.log`、`finalize-tutormodel-vet.log`、`regression-baseline.log`。

## 本地真实端到端结果

复用独立容器 `edu-agent-acc-postgres`，监听 `127.0.0.1:32995`；应用仅监听 `127.0.0.1:33400`。没有启动依赖 32929 的服务，没有操作 `edu-agent-demo*` 或 `forjx-*`。密钥只从受保护的本地 env 文件加载，未写入仓库、本文或日志摘要。

沿用目标 `99d3aa4d-2264-42b5-87ba-50e623c65430` 和课堂 `0e13cb05-ff76-4495-bca5-37f359b4d47f`，题目为 HTCPCP 控制的设备及目的。

| 步骤 | 结论 | 耗时与证据 |
| --- | --- | --- |
| 点击开始学习 → 课堂准备完成 | 上一轮真实运行 `succeeded / learning_started`，本轮复用，未再次调用研究模型。 | 67.520 秒；上一轮配置为输出 16,384 / 超时 120 秒，见 `/tmp/start-e2e/browser-state.json`。 |
| 进入课堂 → 正式答案就绪 | 上一轮浏览器证据保留。 | 1.165 秒，见 `classroom-result.json`。 |
| 提交正式答案 → 回执 | 上一轮成功，`help=none`，本轮复用该不可变答案。 | 0.842 秒，见 `classroom-result.json`。 |
| 请求反馈 → 页面显示教学反馈 | 本轮最终代码、输出 8,192 / 超时 60 秒下成功；显示“达到要求”和“待复核”。 | 7.286 秒；单次教学 Responses 请求 6.116 秒，见 `finalize-result-bounded.json` 和截图 16。 |
| 人工复核接纳 | 使用现有 assessment 配对配置及界面，检查答案/评分项/资料引用并填写理由后确认；未给原研究设备扩权。 | 从确认按钮到回执 0.927 秒；决定从 version 1 provisional 新增为 version 2 accepted，见 `finalize-review-result.json`、截图 19–20。 |
| 同一目标导师框提问 → 显示回答 | 新建同目标交流，问“HTCPCP 协议主要控制什么设备？请直接简短回答这个常识问题，无需调用工具。”；真实回答“咖啡壶（咖啡机）”，run 为 succeeded，使用 1 次模型请求。 | 4.873 秒；Chat 请求 4.340 秒，见 `finalize-goal-mentor-result.json`、截图 21–23。 |

成功反馈的模型用量为输入 12,985、输出 1,162，其中推理 303 Token；预算为 8,192。数据库只读核验确认评分项完整、原模型置信度为 **90/1000**、原待复核决定被保留、人工接纳决定为 version 2；没有将 90 乘以 10，也没有降低阈值。

人工复核后点击继续学习，单活动课堂变为 completed，课堂提问控件随之结束。因此导师回答是在**同一目标的独立交流框**验证；没有证明已完成课堂中仍能提问。

本轮共 **9 次外部模型 HTTP 尝试、0 次 Brave 请求**，包括一次 5 秒拨号超时，低于 15/5 限额。安全计数和状态摘要保存在 `finalize-metrics.json` 和 `finalize-acceptance-summary.json`。计数不含上一轮已完成的开学调用。

失败尝试保留原校验：旧提示在 8,192 和 16,384 预算下均曾全部消耗为推理；另一次实际仍为旧 16,384/120 客户端的返回因 `node_not_allowed` 被拒绝。给出完整长切片候选的中间方案仍耗尽预算，最终采用有限前缀候选后得到有效反馈。教学配置启动时冻结，验收每次调整后都需实际停止旧进程并重新启动，不能仅凭设置页面已保存认定执行配置已切换。

另一次“结合目标资料”的导师提问返回工具调用 `read_goal`、`read_learning_progress`、`read_references` 后执行失败，记录仍为 `model_failed`。该工具路径未通过，失败原因尚未定位；成功的直接常识回答不能替代资料工具检索验收。

## 截图与本地证据

请求的 `/home/yunyue/.cache/edu-agent-codex/start-e2e/` 在当前沙箱只读，实际写入返回 `EROFS`。未绕过权限；原有 01–07、10 保持原样，新截图从 11 开始续编，保存在可写的 `/tmp/start-e2e/screenshots/`。

主要截图：

- `16-assessment.png`：反馈已显示、待复核状态。
- `19-review-before-confirm.png`、`20-review-confirmed.png`：现有人工复核流程。
- `21-mentor-question.png`：同目标提问。
- `22-mentor-answer.png`：回答整页。
- `23-mentor-answer-detail.png`：导师回答局部。

11–15 等失败截图仅作为本地诊断证据。临时目录中的认证状态、配对信息和受保护设置文件不属于可分享日志，不应复制进仓库。本文列出的检查日志、计数摘要和截图不含密钥。

## 推荐部署配置

| 配置 | 建议与本次实测范围 |
| --- | --- |
| 单次模型输出上限 | 8,192 Token；包含推理预算。本次有限候选反馈和直接导师回答通过，长推理仍可能耗尽，不能保证所有任务成功。 |
| 空闲超时 | 60 秒；该设置在本次非流式客户端中作为请求截止时间使用，较小的父 context 截止时间仍生效。不要另加固定 15 秒模型响应头限制。 |
| 模型上下文 | 本次 32,768 Token；部署值须匹配模型能力和输入规模。 |
| 研究正文读取上限 | 本次 100,000 字节；原抓取/引用校验不变。 |
| 导师连接 | 本次复用教学连接、相同模型；导师实际使用 Chat Completions，教学实际使用 Responses。 |
| 拨号 / TLS / 搜索响应头 | 保留 5 秒 / 5 秒 / 15 秒。 |
| 配置生效 | 教学客户端在启动时冻结（`settings/service.go:305`）；保存后重启教学服务。导师每次承领/调用重读配置，并保留配置指纹限制。 |

不建议仅通过上调输出到 16,384 或超时到 120 秒处理引用生成问题。本次旧提示在该配置下仍失败；原开学成功证据确实来自该较大配置，不能据此宣称整条开学链路已在 8,192/60 秒重新通过。改变配置后，旧运行可能因指纹变化失效，应使用现有受控新运行流程。

## 清理与未覆盖项

清理已完成并核验：33400 不再监听，本任务服务与浏览器脚本进程已退出；`start_e2e_local`、`start_e2e_tests` 已删除，数据库中的 `start_e2e_` 前缀库数量为零。预先存在的 `edu-agent-acc-postgres` 容器仍运行，其他任务容器未操作。保留工作树修改和 `/tmp/start-e2e/` 证据，安全清理记录见 `finalize-cleanup.json`。

尚未解决或未覆盖：

- 模型置信度返回 90/1000 导致待复核，自动接纳及无需人工复核的课堂继续学习未通过；置信度提示尺度问题未修复。
- 资料工具导师交流失败尚未定位；本次只验证了同目标直接常识回答，未验证原课堂内完整资料检索问答。
- 资料引用候选只覆盖每个切片前 2,048 字节；答案依据仅在后文的场景、多个资料/评分项的复杂评估未做真实模型验收。
- 本轮未重跑从新目标开始的全部研究/开学流程；前半段复用上一轮 16,384/120 秒证据。
- 未配置 CLI 集成测试二进制，相关既有测试跳过；未运行全仓、race、前端全套、部署验收。未修改这些范围。
- 新截图无法写到指定缓存目录，当前交付位置为 `/tmp/start-e2e/screenshots/`；临时目录可能被系统清理。
