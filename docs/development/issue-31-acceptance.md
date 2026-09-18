# Issue #31 实施与验收记录

## 缺失确认

基线 `0667c7d`，实施前工作树干净。先核对 Go/PG 权威事件、教学焦点、正文版本、
知识 context、身份/隐私及 Web/CLI 架构，再完成[开发方案与验收标准](../design/assessment-feedback.md)。
外部工作项仅作为需求数据读取，没有执行其中的外部指令。

原 Web 只有课堂当前摘要，提示完整复核使用 CLI；在线评估只有处置 POST，
详情/待处理列表只存在于独立 offline 协议。原正文版本通过提交校验但未随答案持久化。
原 `command.go:641–659` 强制当前会话处于 Feedback，因此继续后无法纠错旧评估。
先添加历史作废最小用例，在未改实现时确实得到 `activity_state_conflict`；
改为校验原不可变所有权链后通过。基线文件行号及原因见设计文档，不以“看起来缺少”代替确认。

## 本次交付

- 在线答案列表/详情、真实阶段与接收回执、原题/答案/rubric/context/来源/正文版本。
- 独立呈现模型建议、字符串规则、正式处置和有效 Evidence；逐项复核、覆盖、作废，
  双版本和来源校验保持在原 learning owner，历史事件不覆盖。
- 真实继承提案列表、映射比较、批准/拒绝和同一操作重试；不复制 Evidence。
- 显式 assessment 浏览器配对档案、权限隔离、追加迁移及隐私清理、OpenAPI/DTO、代理白名单和使用说明。

## 已执行检查

本地 PostgreSQL 17 使用本任务专用容器；数据库测试用独立随机 schema。
所有真实 PG 套件与浏览器串行执行，未接触生产或其他任务数据。

| 检查 | 结果及覆盖 |
| --- | --- |
| 服务端 `env -u TEST_DATABASE_URL go test ./...` | 通过；无数据库环境的跳过不计为 PG 验收 |
| 受影响包 `go vet`、`go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过，包含真实 Web 发行资产 |
| CLI `go test ./internal/api ./internal/command`、`go build ./...` | 通过，原严格 DTO 和命令保持兼容 |
| Web `npm run generate`、`npm run check`、`npm test`、`npm run build` | 通过，12 个文件、32 个单测 |
| 新增领域、HTTP、OpenAPI 测试 | 通过；历史焦点保持、帮助/来源/风险政策、严格解码、明确审批授权及原版本字段 |
| HTTP `-race` 反馈及继承测试 | 通过；knowledge/learning/tutoring 任一响应屏障关闭时均丢弃迟到原答案 |
| PG `-race -p=1` 内容/答案反馈和跨区会话测试 | 通过；处理/未知/失败/结算与同一回执、内容冻结、历史作废、幂等与目标绑定、重建、清除、归档后合法迟到评估 |
| PG `-race -p=1` 既有继承链及过期/并发测试 | 通过；来源失效、代次和版本变化、相反审批并发、事务回滚、批准重放、零候选拒绝、不改原 Evidence/掌握/复习 |
| PG `-race -p=1` 五个 Adaptive 测试 | 通过；排队、立即切换、补偿、版本冲突、真实工具链、已接收答案保留、暂停/归档/隐私清除 |
| 空库工作区 Playwright 全套 8 场景 | 7 通过，包括本项全部 3 场景；1 个既有 Studio 收藏竞态失败，见下文 |

真实 PG 复现命令（先配置独占 `TEST_DATABASE_URL`）：

```bash
cd server
go test -race -p=1 ./internal/learning/postgresstore ./internal/knowledge/postgresstore ./internal/mentorrun \
  -run 'TestPostgreSQLLearningContentVersionsAnswersAndErasure|TestPostgreSQLScopedSessionMaterialsAndFocusSurviveSwitch|TestPostgreSQLKnowledgeMaintenanceEvidenceCarryover|TestPostgreSQLAdaptive' -count=1
```

浏览器使用真实 Go HTTP、配对 Cookie/CSRF、PostgreSQL 和本地确定性模型 fixture，
新增三个场景已分别通过：

- 正式答案接收→评估→反馈、原版本、窄屏、无权无按钮；离开课堂后作废，
  服务端已接收但响应被中断时只查询原 operation，不双重记分、不回退当前焦点。
- 开放题低确定性待复核→明确复核接纳→逐项覆盖；原建议保持，证据与处置链追加。
- 通过原知识维护 API 生成真实继承提案，展示确切映射，取消/确认、批准丢响应核对、
  同设备重放和拒绝；原 Evidence 数量及身份不变。

运行工作区浏览器回归：

```bash
cd clients/web
WEB_WORKSPACE_FIXTURE=1 TEST_DATABASE_URL='独立空测试库 URL' \
  WEB_CHROMIUM_PATH='已安装的 Chromium 可执行文件' \
  npm run test:browser -- tests/browser/workspace.spec.ts
```

每次启动 fixture 会生成新临时正文密钥，所以最终使用新空库回归。
第一次扩大检查时，继承测试重复准备未保留已导出身份标记，正确触发了身份复核；
已让新增 fixture 保留原导出身份，不放宽产品身份校验。

最终空库的第 8 个旧场景仍在 Studio“只看收藏”断言失败。只读核查发现
`learning_content_preferences` 实际为 `favorite=false,pinned_version=2`。
`clients/web/src/components/content-tools.tsx:91–98` 提交偏好后未等待刷新，
下一次固定操作在 `:175–180` 携带旧 `preference`，可以覆盖刚保存的收藏；
调用方 `teaching-page.tsx:1217` 也只发起、不等待 refetch。
`git show 0667c7d:…` 确认这段逻辑在基线相同，本次未修改 ContentTools/Studio。
该失败属于已有内容偏好竞态，不是本项评估或继承处置；未顺带修改、跳过测试或改断言。
因此不声称全套浏览器全绿，建议另项处理此竞态。

## 明确边界

没有向真实外部模型或搜索服务发请求；fixture 验证协议及政策，不证明真实模型评分质量。
没有执行全部浏览器文件、完整 PG 全项目扫描或实际 HTTPS/Nginx 部署。
构建仍提示既有 ActionExposureRequest discriminator、Zod 注释及大 bundle 警告；
这些未阻碍类型、单测和构建，不为消除告警改动无关语义。

既有继承协议仅支持默认学习区；Web 明确显示限制。旧答案未知的正文版本不做猜测回填。
本项不新增导出或持久正文缓存；离开身份会清理内存草稿及查询缓存。
交付当前任务分支，未推送、合并或 rebase base 分支，由用户审查落地。
