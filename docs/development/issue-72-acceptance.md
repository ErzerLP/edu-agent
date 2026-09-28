# Issue #72：开放评估浏览器夹具缺少严格 schema 字段

## 问题确认与架构

基线 `2ca85f5`，工作树初始干净。项目由 Go 模块化服务、PostgreSQL、React Web、
独立 Go CLI 和共享 agentcore 组成。knowledge 提供资料与冻结引用，tutoring
管理会话焦点，learning 保存提案、答案、评估、处置与证据，learningcontent
管理不可变正文；Web 通过真实 HTTP API 使用这些 owner。

本用例使用 `scripts/test-server.mjs` 启动本地 HTTP 模型，由
`workspace-model.mjs` 提供确定性输出。教学请求经过
`learning.Service → tutormodel.Adapter → llm.Client`；返回的 JSON 在进入领域
校验前先按正式 schema 校验，因此本地夹具也必须满足相同协议。

以下行号对应修复前基线：

- `clients/web/tests/browser/workspace.spec.ts:566` 调用
  `legacySession(request, space, true)`；`:164` 请求活动提案。
- `clients/web/scripts/workspace-model.mjs:50–58` 在开放题分支省略
  `objective_rule`，而 `server/internal/integrations/tutormodel/adapter.go:78–79`
  要求该字段存在，无客观规则时应传 `null`。
- 同一夹具 `:81–91` 的评估项还省略 `misconception_candidate`，正式 schema
  在 `adapter.go:83` 将其定义为必填可空字段。这会在课堂准备修好后阻断评估。
- `server/internal/integrations/llm/client.go:230–231` 校验模型输出，`:429–430`
  拒绝缺失的必填字段；`adapter.go:57–58` 将其分类为 `schema_mismatch`，
  `server/internal/learning/proposal.go:155` 返回 `proposal_rejected`。

修改前直接调用实际 `workspaceModel`，分别生成开放活动与评估，再检查上述
正式协议必填字段；Node 命令退出 1，分别报告“activity 缺少必填可空字段
objective_rule”和“assessment 缺少必填可空字段 misconception_candidate”。
这确认了夹具与 #57 修复后的协议脱节，无需改变生产校验或评估政策。

## 开发方案与验收标准（实施前）

单个局部批次恢复既有开放评估验收：开放题显式返回 `objective_rule: null`，
无误解候选的评估项显式返回 `misconception_candidate: null`。
复用现有端到端用例验证真实服务的协议与处置，不新增评分算法、API 或迁移。

1. 在未修改夹具时，用独立 PostgreSQL schema 和当前源码构建的服务运行原
   开放评估用例，记录失败位置；修复后同一用例必须通过。
2. Chromium、Firefox 均完成正式答案、待复核、人工接纳与覆盖；原建议仍为
   `pass`，处置链为 `provisional → accepted → overridden`，有效证据仅一条。
3. 每个浏览器使用独立 schema，串行运行整个 `workspace.spec.ts`，确认客观题、
   正文与其他工作台流程未受影响。
4. 运行 Web 类型检查、单元测试、前端生产构建、Go `web_release` 构建，以及
   相关模型协议测试；如有环境限制或无关失败，逐项记录而不计作通过。

本任务范围限定为浏览器夹具，使用既有工作台回归作为验收，不扩大为全仓发布检查。

## 已执行验证

环境为 Linux amd64、Node.js 24.20.0、Go 1.26.6。除了上述直接调用，还用临时
Go 验证程序启动 `httptest` 模型端点，通过 Node 调用实际 `workspaceModel`，
把返回值送入正式 `tutormodel.Adapter.Generate` 和 `llm.Client.Chat`。
基线夹具直接读取 `git show 2ca85f5:clients/web/scripts/workspace-model.mjs`，
修复后读取工作树模块；两次均未替换、复制或放宽正式 schema 校验。

| 真实适配器场景 | 修复前 | 修复后 |
| --- | --- | --- |
| 路线 | 通过 | 通过 |
| 客观活动 | 通过 | 通过 |
| 开放活动 | `tutor model failed: schema_mismatch` | 通过 |
| 开放评估 | `tutor model failed: schema_mismatch` | 通过 |
| 自由问答、讲解 | 通过 | 通过 |

修复前程序退出 1，修复后退出 0。该检查验证真实 HTTP 模型输出与生产 schema
的兼容性，未使用数据库，不代替完整浏览器处置流程。

以下检查通过：

```sh
node --check clients/web/scripts/workspace-model.mjs

cd server
go test -count=1 ./internal/integrations/tutormodel ./internal/integrations/llm ./internal/learning
go vet ./internal/integrations/tutormodel ./internal/integrations/llm ./internal/learning
go build ./...

cd ..
git diff --check
```

所选 Go 测试没有数据库 skip。临时验证程序不纳入提交；未调用真实模型服务。

## 待补验收

当前工作树没有 `clients/web/node_modules`，尚未获得按锁文件安装依赖的授权。
遵守用户全局规范中“安装／升级需要相应明确授权”的约束，已请求 `npm ci`
授权，未擅自安装。Web 类型检查、单元测试、前端及 `web_release` 构建、
Chromium/Firefox 的原用例与整个工作台回归尚未运行，不能计为通过。
首次交付保留上述验收缺口，未推送或创建 PR。

## 用户接受后的落地

用户随后明确接受本任务，授权合入 `main`、在主工作树 squash 提交，并在追加
指令中授权推送 `origin/main` 和处理 issue。未将该接受视为浏览器验收通过。

合入 `main`（`ff7eee6`）时，`workspace-model.mjs` 的 `objective_rule` 有一处
文本冲突。手动保留 #74 新增的帮助等级受限题目、类型与 `allowed_help`，
将规则条件合并为“帮助等级受限或开放复核验收时返回 `null`”，其余客观题仍
返回原规则，同时保留本任务补齐的 `misconception_candidate: null`。

解冲突后用实际夹具覆盖客观题、开放复核、帮助等级受限及两条件同时命中的
活动和评估输出，检查类型、客观规则、帮助等级、置信度及可空误解字段；
通过后才完成合并与 squash。此前 Go 生产输入未变化，复用已有测试、vet 和
构建证据；Web 浏览器验收仍未运行。
