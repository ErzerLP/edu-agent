# Issue #65：WebKit 重连验收与刷新时序

## 问题确认

基线 `dc03439`，初始工作树干净；相对报告的 `364fbe1` 仅新增文档。
已阅读开发规范、测试策略、Web 发布、导师运行及任务中心设计。项目由 Go 服务组合
身份、教学、知识与持久运行，PostgreSQL 保存权威数据；React 学习端通过同源 API、
TanStack Query 与 SSE 读取状态，CLI 使用独立入口及共享领域协议。

使用 Playwright 1.62.1 Noble 的 WebKit、独立 PostgreSQL 17 空库、本地模型 fixture
及生产 Web 资产执行原始 `mentor.spec.ts`，业务断言完成后，第 96 行页面异常检查失败。
列表包含 `/v1/tutoring/sessions` 等路径的 `due to access control checks.`。
没有据此认定回答、清除或停止失效。

原代码位置及原因：

- `clients/web/tests/browser/mentor.spec.ts:61–63` 和 `tasks.spec.ts:182–184` 在恢复网络后
  立即刷新；`setOffline(false)` 返回并不表示应用完成了重连读取。
- `clients/web/src/teaching-page.tsx:61–67`、`components/workspace-shell.tsx:16–24`
  和 `import-page.tsx:80–97` 使用 Query 读取会话、学习区及导入任务。
- `clients/web/src/api/client.ts:68–80` 在业务请求之前异步读取当前会话，核对身份后
  才发送业务请求；`unwrap` 和 Query 负责传播、保存请求错误。
- 原场景增加临时诊断后，记录到 `online` 后 12 毫秒进入 `beforeunload`，再过约
  3 毫秒才开始上述业务 fetch。此时 WebKit 拒绝创建请求，并输出访问控制诊断；
  fetch 的 `TypeError: Load failed` 已被捕获，没有对应的 `unhandledrejection`。
  Playwright WebKit 将 JavaScript 来源的 error 级控制台诊断转为 `pageerror`。

因此本项确认的是浏览器验收的重连与刷新竞态。无需放宽 CORS、关闭身份校验，或修改
业务状态。原生 fetch 的离线、取消和刷新最小实验均没有单独复现该错误；完整生产页面
中的异步会话校验链及立即刷新的组合才提供了本次定位证据。

## 开发方案与验收标准（实施前）

这是一个测试时序修复批次，不涉及公共协议、数据库迁移或生产功能变更。

1. 为两条用例增加共用重连等待：先订阅请求事件，再切换离线/在线；必须观察到当前页面
   对应业务 API 的成功 GET 响应及完整响应体，并等待已发起的有限 API 读取结束。
2. 导师 SSE 是持续连接，不能把全页 `networkidle` 当作恢复完成条件；仅排除明确的
   运行事件流，仍等待会话校验及快照等普通 GET。完成或失败的请求均解除在途计数。
3. 确认重连后才执行原来的刷新、重启及后续业务断言；保留原始 `pageerror` 收集和
   空列表断言，不过滤访问控制消息，不通过忽略错误或固定休眠获得通过。
4. 验收须包含修复前失败、修复后 WebKit 导师/导入完整场景通过，以及其他浏览器引擎
   的相同场景；导入仍须证明五批不重复、刷新/重启/响应丢失恢复、取消与归档隔离。
5. 只检查受影响测试及差异格式；未修改生产源码，不重复执行无关 Go/数据库单元矩阵。

## 环境与结果

2026-09-21，Linux amd64，Go 1.26.6，PostgreSQL 17.11，本机已有
`mcr.microsoft.com/playwright:v1.62.1-noble` 镜像及 Playwright 1.62.1。
PostgreSQL 使用本任务新建的独立容器，每个测试文件、引擎使用独立空库；定向验收
串行运行，并持有 `/tmp/edu-agent-operations-candidate.lock`。

当前工作树没有前端依赖。安装授权未收到回复，未执行安装。复用本机已有 Playwright
测试入口，通过仓库外的 Node 模块解析钩子将 `@playwright/test` 映射至同版本的
`playwright/test`；没有修改项目依赖、测试配置或断言。测试使用原仓库配置与本地模型
fixture，显式设置 `WEB_RELEASE_MATRIX=1 WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1`。
实际测试命令为：

```sh
node /pw/node_modules/playwright/cli.js test mentor.spec.ts --project=webkit --reporter=line
node /pw/node_modules/playwright/cli.js test tasks.spec.ts --project=webkit --reporter=line
```

同样命令分别替换项目为 `chromium`、`firefox`；它们执行的测试入口等价于仓库的
`npm run test:browser -- ...`。原始用例诊断只在仓库外增加事件追踪，不进入提交。

生产资产复用本机 task-76 的既有构建，已比较其全部 `src/` 和锁文件，与当前工作树
完全一致。随后在当前 `server/` 执行
`GOPROXY=off go build -tags web_release -o edu-agentd ./cmd/edu-agentd`，构建通过。
复用输入标识：

- 前端源码 Git tree：`c1a2f8a7dc1b32360959b073e8f7fcd972d2d0e6`。
- 锁文件 SHA-256：`88c4c869505729bacd21503174cd4e508e9115a27e06aaee19f4223677c7f732`。
- `index-v3B7c-0W.js` SHA-256：`eb6ba245374ff99f58795e31e46e965508ff0bf5afdfc273047a69aafef346df`。

| 检查 | 结果 |
| --- | --- |
| 原始 WebKit 导师 | 0/1，最终第 96 行失败；功能断言已完成，记录 12 条访问控制诊断 |
| 原始 WebKit 导入 | 2/2，33.7 秒；本轮未复现导入报错，不能把报告中的每次失败都视为稳定复现 |
| 修复后 WebKit 导师 | 1/1，27.7 秒，页面异常为空 |
| 修复后 WebKit 导入 | 2/2，33.7 秒，页面异常为空，五批不重复及取消/归档场景通过 |
| 修复后 Chromium 导师 | 1/1，18.4 秒 |
| 修复后 Chromium 导入 | 2/2，32.5 秒 |
| 修复后 Firefox 导师 | 1/1，21.5 秒 |
| 修复后 Firefox 导入 | 2/2，34.6 秒 |
| 三个受影响测试文件的 `node --check --experimental-strip-types` | 通过；属于语法检查，不是 TypeScript 类型检查 |
| `git diff --check` | 通过 |

原导师场景的页面异常已证实由重连后的刷新竞态触发；导入具有相同代码时序，使用同一
等待方法消除该竞态，并保留全部原有断言。修改没有过滤错误、关闭浏览器检查或改变
生产请求行为。诊断及验收日志保留在本机 `/tmp/edu-issue65.gSbUT6/`，不随提交交付。

未运行 `npm run check`、`npm test` 和 Vite 重新构建：当前缺少项目依赖，本次也未修改
生产源码。以上复用资产与定向浏览器结果不等同于完整 Web 发布候选通过。
按专用工作树约束提交当前任务分支供审查，不合并、变基或推送基分支。
