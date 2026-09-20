# Issue #43：复习动作类型阻断 Web 构建

## 问题确认

核查日期：2026-09-20；基线：`d591bcc`；任务分支：`task/65`。初始工作树干净。
项目由 Go 服务和 PostgreSQL 保存正式知识、教学状态及学习事实，React Web 与
Go CLI 消费共用协议，共享模型循环位于 `packages/agentcore`。本问题位于 Web
课堂组件的静态类型，未涉及服务端状态、数据库或协议变更。

基线代码已确认调用与类型不一致：

- `clients/web/src/teaching-page.tsx:503`：`generate` 的动作参数只列出四个值，
  遗漏 `present_review`。
- `clients/web/src/teaching-page.tsx:881`：复习按钮实际调用
  `generate('activity', 'present_review')`，因此不符合上述参数类型。
- `server/api/openapi.yaml:6254` 和 `clients/web/src/api/schema.d.ts:4028`
  均已将该动作列入携带 `proposal_id` 的合法请求。
- `server/internal/learning/command.go:329`、`:334`、`:367` 已接收活动提案，
  按复习语义生成活动并记录复习事件。将按钮改为普通出题会改变业务语义。

首次执行 `make web-check` 因工作树尚未安装 Web 依赖而报 `tsc: not found`；
该环境错误不作为 issue 所报 TS2345 的运行复现证据。

## 修复前方案与验收标准

1. 仅在 `generate` 的有限动作联合类型中补入 `present_review`，使既有复习按钮
   与 API 合同一致。保留现有生成提案、允许动作检查及正式保存路径。
2. 获得锁定依赖安装授权后，使用基线源码运行 `make web-check` 复现 TS2345。
3. 修改后要求 `make web-check` 通过类型检查及全部 Web 单元测试，
   `cd clients/web && npm run build` 成功生成并复制生产资源。
4. 执行 `git diff --check`；只提交相关源码及本记录。既有复习按钮调用由
   TypeScript 直接检查，作为本次类型遗漏的回归检查。

真实浏览器业务验收需要独立 `TEST_DATABASE_URL`；当前未配置，不将生产构建
通过等同于真实数据库、模型或浏览器全流程验收。

## 当前结果

已按代码证据补齐 `generate` 的动作类型，仅新增 `present_review`；没有放宽为
任意字符串，没有修改按钮、API、生成类型或服务端逻辑。`git diff --check` 通过。

运行验证尚未完成：首次 `make web-check` 停在缺少 `tsc`，未执行 TypeScript
检查及单元测试；生产构建也未运行。此前按用户全局规范请求锁定依赖安装授权，
未安装依赖。用户随后明确接受当前任务并要求合并到 `main`、推送及处理 #43，
本次按该落地指令提交；上述未运行项继续作为验证限制，不计为通过。
