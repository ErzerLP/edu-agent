# Issue #47 开发方案与验收

## 问题确认

基线 `d591bcc`，任务分支 `task/69`。已阅读开发流程、测试策略、Web 学习入口及
记忆隐私设计，核对 React 会话管理、Go HTTP/identity/privacy、PostgreSQL owner
门禁和后台清除恢复流程。前端消费同源 Cookie API，清除由服务端各 owner 执行，
会话失效与全部本地清除完成是不同阶段。

在本任务独立空 PostgreSQL 中，以未修改的源码构建 Vite 资产和 Go 服务，运行：

```bash
WEB_MENTOR_FIXTURE=1 TEST_DATABASE_URL=独立测试库 \
  npm run test:browser -- memory.spec.ts --project=chromium --reporter=line --trace=on
```

第一项通过，第二项在重新配对后等待首页标题失败，第三项连锁失败，与报告一致。
网络记录中第二项的新 `POST /v1/web/pairings` 返回 `503 web_pairing_unavailable`，
原 `POST /v1/privacy/erasures` 被导航取消，并非配对成功后首页渲染错误。

基线原因链：

- `clients/web/tests/browser/memory.spec.ts:93–95` 只等旧会话 401 就刷新。
- `server/internal/transport/httpapi/memory_privacy.go:442` 的本地清除使用请求上下文；
  该刷新会取消正在进行的清除。数据库日志证实知识清除 SQL 被取消。
- `server/internal/privacy/postgresstore/store.go:533–547` 逐项执行本地清除，
  `:639–660` 按 owner 重新开放门禁；此时 identity 已开放、learning 等仍关闭。
- `server/migrations/000007_offline_sync_core.sql:612–622` 在创建新设备时初始化
  learning 所有的离线状态，因 learning 门禁仍关闭而失败。新配对事务回滚，
  不是会话缓存或首页路由问题。
- `server/internal/app/composition.go:301–305` 已有后台恢复任务，默认间隔 30 秒；
  不能把它的异步恢复与测试内的立即配对混为一谈。

原 Go 集成 `TestPostgreSQLWebMemoryApprovalPrivacyAndRevocation` 在真实 PG 下通过，
它等清除 HTTP 响应后才配对，进一步说明浏览器测试缺少正确的同步点。

## 开发方案与验收标准

保持单一小批次，只修复本场景的同步及可观察断言：

1. 点击确认前订阅原清除请求响应，等待响应正文并核对本地清除结果，再刷新。
2. 配对辅助函数断言真实 HTTP 201，保留原首页标题断言；清除后核对新设备身份、
   新隐私代次和原操作/回执身份，不延长等待时间掩盖失败。
3. Chromium、Firefox、WebKit 串行使用各自独立空库，运行完整 `memory.spec.ts`，
   启用本地导师夹具以覆盖第三项，证明没有全局状态连锁失败。
4. 运行 Web 类型检查、单测、生产构建、Go 服务构建和原 Go/PG 记忆隐私集成。

原始 `npm run build` 还发现 `teaching-page.tsx:881` 已调用 `present_review`，但
`:503` 的本地参数类型漏列该动作。OpenAPI 与生成类型均已包含它；仅补齐这一个
类型字面量，解除验收构建阻塞，不改变运行时行为。

## 验收结果

已完成的修复是等待清除响应正文，并断言身份、知识、学习的本地清除步骤成功。
随后仍要求旧会话返回 401、新配对返回 201、首页标题出现、新设备身份及递增代次
正确、重新打开的回执 ID 与原响应一致。未延长原断言超时，未放宽隐私门禁。

数据库使用本任务专用本地容器 `edu-agent-task69-postgres`，仅监听 loopback 32969，
各浏览器串行使用独立空库。Firefox/WebKit 使用已有的
`mcr.microsoft.com/playwright:v1.62.1-noble` 镜像，无真实模型或业务数据库访问。

| 检查 | 实际结果 |
| --- | --- |
| 原始 Chromium 完整记忆文件 | 1 通过、2 失败，复现报告中的重新配对失败及连锁失败 |
| 修复后 Chromium 完整记忆文件 | 3/3 通过 |
| 修复后 Firefox 完整记忆文件 | 3/3 通过 |
| 修复后 WebKit 完整记忆文件 | 前两项通过；第三项在导师审批面板更新处超时，跟踪诊断同样复现 |
| WebKit 第三项在另一空库单独运行 | 1/1 通过，不能据此宣称完整文件稳定通过 |
| `npm test` | 21 个文件、52 项通过 |
| `npm run build` | 类型检查和 Vite 生产构建通过 |
| `go -C server build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过 |
| 原 Go/PG Web 记忆隐私集成 | 通过；复用本任务前段相同 Go 源码的有效结果 |
| `git diff --check` | 通过 |

浏览器命令在 `clients/web` 中执行，设置 `WEB_MENTOR_FIXTURE=1`，后两种浏览器另设
`WEB_RELEASE_MATRIX=1`，`TEST_DATABASE_URL` 分别指向上述独立库：

```bash
npm run test:browser -- memory.spec.ts --project=chromium --reporter=line
npm run test:browser -- memory.spec.ts --project=firefox --reporter=line
npm run test:browser -- memory.spec.ts --project=webkit --reporter=line
# 只为定位 WebKit 第三项增加跟踪，并在另一空库单独运行该项。
npm run test:browser -- memory.spec.ts --project=webkit --reporter=line --trace=on
npm run test:browser -- memory.spec.ts --project=webkit --grep '导师申请' --reporter=line --trace=on
```

原 Go 集成命令为在 `server` 中、配置专用 `TEST_DATABASE_URL` 后运行：

```bash
go test -count=1 ./internal/app -run '^TestPostgreSQLWebMemoryApprovalPrivacyAndRevocation$' -timeout=2m
```

### 待复核的 WebKit 导师更新问题

#47 的清除、重新配对及原回执恢复已在三个引擎通过。WebKit 第三项的后续超时发生在
配对、保存目标、提交导师请求均成功之后。数据库全部 owner 门禁已开放，导师运行
已为 `waiting_approval`（跟踪运行中 version/watermark 为 6）。浏览器最后读取的
快照仍为 `running`、watermark 3；随后事件流返回 200，但在 5 秒断言内没有下一次
快照读取。它与本次已确认的配对 503、清除被取消不同，尚未确定订阅更新失败原因。

没有跳过第三项、增加超时或凭推测修改导师解析器。完整 WebKit 文件尚未稳定通过，
需另行复核导师事件订阅；不能将本次结果写成全部浏览器验收通过。Vite 另有依赖注释
和大块体积提示，不影响构建结果。本次未运行全部浏览器文件或全仓 Go 测试。

仅提交当前任务分支，交由用户审查落地；专用测试数据库保留以便复核，容器停止。
