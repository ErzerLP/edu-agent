# 新学习服务 CLI 接入（Issue #36）

## 缺失确认

基线 `acc4d05`，专用工作树初始干净。已阅读项目开发规范、测试策略，以及研究、
内容协作、自适应变更和进度设计；当前没有 #36 的 Comet brief/spec。
项目由 Go 服务、PostgreSQL 权威存储、React Web 和独立 Go CLI 组成。
learning/tutoring 拥有活动、答案与证据，knowledge 拥有来源/context，
learningcontent 拥有不可变正文，mentorrun 拥有运行与模型调用。

- `clients/cli-go/internal/command/goal.go:12` 仅分派 changes/plan/set 和旧目标管理。
  执行 `go run ./cmd/edu-agent goal research --help` 退出 6，报告 internal_error。
- `clients/cli-go/internal/command/changes.go:10` 仅调用变更读取，没有提出或决定入口。
- `clients/cli-go/internal/api/client.go:378` 仅发送变更版本头，没有正文协议接入。
- `server/internal/transport/httpapi/mentor_runs.go:29` 已挂载研究/开学运行和命令；
  `learning_content.go:17` 已挂载正文、版本、作答；`learning_changes.go:107`
  已挂载具体变更命令。Web 已调用这些服务，CLI 没有同等入口，非隐藏设置或别名。

## 实施前方案

保持一个结果：显式选择同一学习上下文，在 CLI 使用既有正式服务并回到 Web 继续。
公共协议和命令入口相关联，在当前工作树串行完成三个有界批次。

1. 新增 `study` 服务入口及 `goal research`/`goal start-learning` 别名。
   复用配对和端点绑定，能力/schema 协商独立于旧严格 DTO；提供稳定 JSON 输入输出、
   必填 operation_id、只读轮询和原操作查询。研究/开学/导师调整均在服务端执行，
   不修改本地 Agent 循环、Session 或仅保存目标的零模型语义。
2. 正文/版本/来源/context、具体变更审阅与决定接入原命令。文本安全投影，未知块显示
   fallback，未知交互拒绝作答；明确原会话及版本，不隐式切换 current。
   工作台用目标名、运行类型/状态和内容标题导航，复用原请求代次和进程内草稿隔离。
3. 补 API/命令/工作台回归及生产 CLI 跨端场景，更新帮助、OpenAPI 说明、能力矩阵。
   先精确失败回归，再受影响包、构建/vet、真实 PG 与浏览器检查；依赖缺失如实记录。

## 验收标准

- 新命令真实调用既有研究/开学服务，保存目标仍不发模型请求。
- 固定区、目标、会话、运行和操作身份；结果未知不自动重放写命令，不复制答案/Evidence。
- 查询既有内容、版本、来源、context；未知显示可回退，未知作答明确受限。
- 变更决定包含具体 revision/hash/interaction，排队与生效分别显示，服务端检查安全边界。
- 工作台按名称选择目标、会话、运行和内容，返回/刷新不需要解析 stdout 或复制 UUID。
- 已提交状态来自服务端；草稿仅在原进程/标签页，迟到结果不进入另一学习区。
- 新协议缺失返回升级提示；功能不可用阻止新研究/变更，读取和清除不以可写能力为门禁。
- 旧 endpoint/keychain/加密历史/no-save/Shell/PTY/离线包路径不修改；相关回归检查通过。
- 记录真实 PG、生产 CLI、浏览器、模型 fixture 与平台原生检查各自结果，不混称通过。

交付当前任务分支供审查，不合并、变基或推送基础分支。

## 补充缺陷与事务修复

`learningcontent.ValidateAttemptTx` 原来在缺少新版作答上下文时直接返回 nil，
虽然 `Validate` 允许未来交互类型、`CanAnswer` 能拒绝它，但旧入口从未调用后者。
在真实 PostgreSQL 中提交 `future_input` 正式版本后，新增旧入口回归实际失败：
“旧入口必须拒绝未知作答规则，实际：<nil>”。原作答事务现读取同活动/修订/区的
正式正文并校验交互；没有正文的旧活动保持旧合同，正文不可解密时明确受限。
HTTP 将该错误映射为 422 `learning_content_upgrade_required`，不报严格解码崩溃。

## 脚本使用与能力矩阵

请求文件使用原 OpenAPI DTO。以下开学示例中的操作 ID 应替换为本次新 UUID；
先通过 `study current --goal ... --kind start_learning` 查询明确目标及运行类型，
已有运行时复用 `run.session_id`，无历史才创建运行会话 UUID。
相同请求的显式重试保留原 ID 和全部字段。目标 ID/当前 revision 从 `goal show` 读取。

```json
{
  "operation_id": "50000000-0000-4000-8000-000000000001",
  "session_id": "60000000-0000-4000-8000-000000000001",
  "expected_version": 1,
  "prompt": "Go 并发",
  "save": true,
  "request_budget": 10,
  "token_budget": 20000,
  "research": {
    "topic": "Go 并发",
    "external_consent": true,
    "auto_adopt": true,
    "policy": { "mode": "supplement", "domains": [] }
  },
  "start_learning": { "new_session": true, "model_consent": true }
}
```

仅研究时删除 `start_learning`，按本次授权设置 `auto_adopt`，调用 `study research`。
导师调整使用 `study mentor`：删除 research/start_learning，添加明确的
`teaching_session_id`，prompt 写调整要求；服务端共享 Go 核心读取正式上下文、提出候选。
谨慎模式的路线与目标范围变更需要具体审批；自适应路线可能直接排队，不能把排队当作生效。

`study change-context --goal ... --session ...` 返回正式 base；脚本也可用
`study change-command` 的 `propose` 直接提交完整 candidate。审批文件包含
`session_id/operation_id/action/expected_revision/hash/interaction_id/immediate:false`。
`apply_now` 是独立显式动作；版本冲突后重新阅读并重新确认，不能换成新 hash 重放旧批准。

| 能力 | 新 CLI | 兼容与关闭行为 |
| --- | --- | --- |
| 研究、开学、服务端导师调整 | `study research/start/mentor`、目标/课堂工作台 | 先校验 schema/capability；旧服务提示升级，配置关闭拒绝新请求 |
| 运行、原回执、恢复、清除 | `current/runs/run/operation/run-command`，只读轮询 | current 须明确目标和类型；原设备、原区、原运行；读取与 clear 不受可写 capability 门禁限制 |
| 来源与上下文 | `sources/source/citation/context` | citation 必须指定正文版本；权限仍由原 owner 校验 |
| 内容与版本 | `library/content/history/ensure`、内容库与历史选择 | 未知块 fallback；新 DTO 独立于旧严格协议 |
| 作答 | `answer`、工作台正式正文页 | text/single_choice；未知规则所有在线入口拒绝，旧包不重签 |
| 具体变更 | `changes/change/change-context/change-command`、审阅页 | 原会话、候选 revision/hash/interaction；采用仍走原事务与安全边界 |
| 进度与复习 | 原 `progress/reviews`、工作台概览与复习 | 沿用服务端正式聚合，无本地掌握度副本 |
| 本地聊天、文件、Shell/PTY、离线 | 原命令 | 没有迁移、自动上传或重写原签名 |

原操作未知时：运行用 `operation`，答案用 `session-operation`，变更用 `change`。
运行清除仅清除该运行正文，正式知识及答案继续由原隐私 owner 处理。关功能不删表；
丢失解密密钥不是可读的回滚方案。能力协商只读，不主动发外部模型或搜索请求。
