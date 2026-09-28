# Issue #74：正式答案的帮助等级与活动限制不一致

## 架构与问题确认

基线 `2ca85f5`，工作树初始干净。项目由 Go 服务端、PostgreSQL、React/TypeScript
Web、独立 Go CLI 与共享 agentcore 组成。learning 冻结活动及其允许帮助等级，
tutoring 管教学状态；learningcontent 保存加密正文版本，HTTP 内容答案入口复用
learning 的正式答案提交事务。Web 从会话工作项读取活动并保存标签页内草稿。

对原报告保留的隔离测试库只读查询确认：该开放题的正文与 issue 完全一致，
`allowed_help` 为 `hint/scaffold/answer_revealed`，不含 `none`；同一课堂的两条
拒绝回执均为 `{"code":"invalid_request","reason":"help_not_allowed"}`。

根因链路（修复前行号）：

- `server/internal/learning/proposal.go:766` 接受任意非空、合法且不重复的帮助等级集，
  不要求包含 `none`；这是合法活动，文本作答方式本身受支持。
- `clients/web/src/teaching-page.tsx:241`、`:285` 无条件将新草稿或恢复默认值设为
  `none`，而 `:807` 的下拉框只渲染活动允许的选项。没有匹配选项时，浏览器展示
  首项，但 React 状态仍为 `none`；`:465` 将该隐藏的状态发送给服务端。
- `server/internal/learning/command.go:388` 严格检查活动允许帮助等级，拒绝此次
  `none`，原因是 `help_not_allowed`。
- `server/internal/transport/problem/problem.go:152` 将其映射为报告中的 HTTP 400。

活动的帮助规则属于冻结合同，答案必须记录实际获得的帮助；不能为了提交成功
自动把独立作答改成已获提示，也不能绕过服务端限制。

## 开发方案与验收标准（实施前）

本次只有 Web 正式答案的帮助选择闭环，无需拆分，不修改公共协议或冻结活动。

1. 新草稿仍默认独立作答；当前值不在允许列表时，下拉框明确显示待选择状态，
   说明活动限制，保留答案和原帮助记录，等待用户按实际情况选择。
2. 按钮及 Ctrl+Enter 共用允许等级检查；非法默认值或恢复草稿均不得发出请求。
3. 确定性教学模型夹具生成与报告一致的开放题及帮助限制，通过正式 API 采用、
   开始和提交；修复前先运行新增回归并观察失败。
4. 回归验证限制提示、草稿恢复、用户明确选择后的请求/存储帮助值一致、答案
   进入 `Evaluating` 并显示反馈入口；保留原允许 `none` 活动的正常提交流程。
5. 运行前端类型检查、单测、生产构建和定向浏览器验收；依赖或环境缺口如实记录。

## 验证记录

2026-09-28，Linux amd64、Go 1.26.6、Node.js 24.20.0。

| 检查 | 结果 |
| --- | --- |
| 原隔离测试库的活动与拒绝回执只读核对 | 确认原题缺少 `none`，两次拒绝原因均为 `help_not_allowed` |
| `go test ./internal/learning ./internal/learningcontent ./internal/transport/httpapi` | 三个包通过；未包含 PostgreSQL 集成测试 |
| 对应三个包的 `go vet` | 通过 |
| `node --check clients/web/scripts/workspace-model.mjs` | 通过 |
| 直接调用模型夹具并断言输出 | 新场景输出开放题、受限帮助及合法空客观规则；原默认场景仍含 `none` |
| `git diff --check` | 通过 |
| `npm run check` | 环境缺口：退出 127，`tsc: not found`，工作树没有 Web 依赖 |
| Web 单测、生产构建、新增浏览器回归及原 `none` 场景回归 | 尚未执行，等待按锁文件安装依赖的明确授权 |

按用户全局规范，安装依赖需要明确授权；已申请执行 `npm ci --no-fund`，
尚未获得答复，因此没有安装或升级依赖，也未把待执行的浏览器断言当作通过。
问题本身已确认，修复及回归用例已完成；前端验收尚未完成，不宣称完整验收通过。
原报告测试库仅做读取，没有修改原课堂、回执或重新调用真实模型。
