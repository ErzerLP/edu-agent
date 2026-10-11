# “开始学习”真实模型 research_failed 定位与诊断修复

日期：2026-10-11（UTC）。工作分支：`fix-start-research-model`，基线
`16fafcd`。本轮仅修改指定 worktree；演示数据库只执行 SELECT，未修改部署、
设置或容器，未在演示部署上重跑开学。

## 根因与调用链

失败的是读取来源后的**第一次来源综合请求**，不是第一项活动准备，也不是教学
Responses API 的 Schema 请求。该请求的 2048 输出 Token 上限被模型推理耗尽，
HTTP 200 返回 `finish_reason=length`、空正文，agentcore 将其分类为
`response_truncated`。服务端结算把分类覆盖为 `research_failed`，且正常结算
返回 nil，因此后台 worker 没有记录执行错误。

证据链（行号对应本次提交）：

1. `server/internal/mentorrun/research.go:171` 使用
   `h.limits.OutputTokens` 构造来源综合请求；`:181` 调用
   `(callModel{h}).Complete`；`:185` 才进入综合内容检查。提示词要求 JSON，
   请求没有 `response_format`、原生 Schema 或工具。
2. `server/internal/mentorrun/core.go:393` 委托 `h.model.Complete`；
   `server/internal/settings/service.go:316` 的 `MentorClient` 构造
   `modelclient.Client`（`:345`），复用教学端点、模型和凭据。
   `service.go:134`、`:272` 明确把导师有效接口与探测保持为 Chat Completions。
3. `packages/agentcore/modelclient/client.go:174` 固定请求
   `/chat/completions`，`:149` 检查 finish reason，`:244` 将 `length` 分类为
   `response_truncated`。本次没有修改 agentcore 或 CLI。
4. `server/internal/mentorrun/worker.go:199` 将研究执行错误的运行说明设为
   `research_failed`；`:319` 返回 `finish` 的结算结果，原始执行分类不会传给
   上层 worker 日志。

演示运行 `6c6280dc-f622-4ed4-a4b8-2fec1cfe1115` 的只读事件显示：第一次
`model_started`（seq 14）后直接 `finished`（seq 15）；重试也是 seq 25 后直接
seq 26。脱敏快照显示综合与活动准备均未完成，第二次最后阶段为 `waiting_model`。
读取的部署配置是 `output_tokens=2048`、`context_tokens=32768`、
`idle_timeout_seconds=30`，导师显式复用教学连接。

等价复现使用同一公开主题、同一 Wikipedia 来源重新读取的前三片段、生产提示词、
真实 agentcore 客户端，输入估算为 1835。预留量
`1835 + 2048 + 256 = 4139`（`server/internal/mentorrun/core.go:293`），与两次
运行的 Token 扣除量一致。历史请求的原始响应与错误分类没有被保存，无法恢复；
历史根因由调用阶段、预算和真实等价复现共同支持。

## 最小修复及配置处理

- `worker.go:235` 新增固定分类的结构化 WARN 日志；`:316` 在错误结算前记录。
  日志业务字段仅为 `run_id`、`kind`、`stage`、`error_category`。本次失败会记录
  `mentor_run_failed / start_learning / waiting_model / response_truncated`。
- 仅允许已知的模型错误分类；未知分类和无类型错误使用固定 `model_failed`。
  超时、取消、上下文变化、引用校验与上下文限制也有固定分类。不记录原始 error、
  Key、端点、提示词、来源正文、模型正文或隐藏推理。
- 保留现有 `research_failed` 运行说明、API/OpenAPI、数据库结构和用户可见语义；
  严格 JSON 和引用校验不变。没有协议切换、自动重试或自动增加预算。
- 根因是配置预算，因此按任务要求**不修改默认 2048**。本样本 4096 仍失败，
  8192 成功；成功请求用时约 29.97 秒，已接近部署的 30 秒超时。

后续部署负责人员可在设置页预算项将“单次输出 Token 上限”
（`limits.output_tokens`）设为 **8192**，建议将“无响应超时（秒）”
（`limits.idle_timeout_seconds`）设为 **60**。单次成功样本不保证所有任务均能在
该预算内完成。输出上限仍须小于上下文、且不超过研究 Token 预算；本部署
32768 上下文与 100000 研究预算满足这些限制。

预算保存会清除旧探测记录；教学执行使用启动时冻结配置，需要由部署负责人员
重新显式探测并安排重启后验收课堂。导师开学路径的新运行立即读取新 Limits，
无需为了这条调用切换协议。旧运行的配置指纹包含 Limits
（`server/internal/settings/service.go:335`），改变预算后继续旧运行会被配置门禁
拒绝（`server/internal/mentorrun/core.go:326`）；应在同一目标下创建新的开学运行，
不能仅给旧运行追加总 Token 预算。增大开学页面的“Token 预算”不会改变单次
`max_tokens`。当前没有独立的输出预算环境变量。本轮没有执行任何上述部署操作。

无需用户决定是否改 agentcore：现有 Chat Completions 在足够预算下成功，
没有依据实施协议改动。若未来确需 Responses，应另行评审服务端专用注入方案。

## 回归与检查

新增 `server/internal/mentorrun/worker_diagnostics_test.go`：

- 通过真实 modelclient 和本机 HTTP 夹具复现 `length`，验证日志保留
  `response_truncated`，且包装错误、请求、响应及运行正文中的敏感哨兵均未输出。
- 验证协议错误、过滤、超时、取消、引用、上下文限制、搜索错误和未知分类；
  检查结构化字段白名单，防止任意错误码进入日志。

测试先因缺少 `logRunFailure` 失败，最小实现后通过。执行命令（server 目录）：

```sh
umask 022
GOCACHE=/tmp/start-research-model/go-cache go test ./internal/mentorrun ./internal/learningstart ./internal/settings
GOCACHE=/tmp/start-research-model/go-cache go vet ./internal/mentorrun ./internal/learningstart ./internal/settings
```

三包 `go test` 通过，`go vet` 退出 0、无输出。受限 sandbox 不允许本机端口监听，
首次测试无法启动 httptest；在批准的本机监听权限下重跑通过。Go 编译缓存放在
`/tmp`，没有安装或升级依赖。未配置 `TEST_DATABASE_URL`，现有 PostgreSQL
集成测试按其规则跳过；没有把它们计为通过。

## 真实模型 smoke

模型 `deepseek-v4-flash`，HTTPS 主机 `api.yunyueoi.com`，实际路径
`/v1/chat/completions`。凭据仅从受保护的 acceptance 环境文件载入。
临时 Go 测试与公开来源副本位于 `/tmp/start-research-model/`，通过 Go overlay
注入测试，未提交。临时 transport 只汇总 HTTP 状态、finish reason、Token 用量、
正文长度与严格校验结果，不落盘完整响应或推理正文。

实际真实模型请求总数 **3 次**，无重试或协议回退：

| 输出上限 | 超时 | HTTP / finish reason | 输出 Token（含推理） | 推理 Token | 正文 | 结果 |
| --- | --- | --- | --- | --- | --- | --- |
| 2048 | 30 秒 | 200 / length | 2048 | 2048 | 0 字节 | response_truncated，约 11.06 秒 |
| 4096 | 30 秒 | 200 / length | 4094 | 4094 | 0 字节 | response_truncated，约 19.38 秒 |
| 8192 | 60 秒 | 200 / stop | 7525 | 5806 | 4961 字节 | 严格校验通过，4 个要点，约 29.97 秒 |

三次请求均没有 `response_format`。成功 smoke 在诊断修复后运行；使用
`DisallowUnknownFields`、单个 JSON 后 EOF、禁止工具调用、原有正文大小限制和
`research.Synthesis.Validate` 逐字引用校验，全部通过。没有用 json_object/schema
切换或放宽校验取得成功。

## 未覆盖与仍待完成

- 没有在演示部署重跑开学、修改配置、重启或确认实际进入课堂；部署预算调整与
  完整课堂手测由另一个任务负责。当前演示仍是原来的 2048/30 秒配置。
- 未验证后续 `startLearning` 活动准备、正式来源采纳/发布或教学回答的真实模型
  成功情况；本次 smoke 只覆盖实际失败的来源综合调用。
- 重新抓取来源的全文指纹与历史快照不同，未解密历史 checkpoint；本次属于
  相同来源、主题、提示词与客户端的等价复现，不宣称逐字重放历史正文。
- 未跑数据库集成或全项目检查。历史运行只能保留原有通用说明，新增日志只对
  后续部署本次代码的运行有效。
- agentcore 中尚无稳定分类的错误仍记录 `model_failed`；本次不扩大公共客户端
  错误契约，也不改变 CLI 行为。
