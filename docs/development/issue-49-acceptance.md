# Issue #49：离线浏览器验收配置与能力边界

## 问题确认与开发方案

基线 `d591bcc`，工作树开始时无修改。项目由 Go 服务端及 PostgreSQL、React Web、
Go CLI 和共享 `packages/agentcore` 组成；离线包签发、同步、清除属于服务端既有 owner，
浏览器通过独立离线身份连接，正文保存在本地加密库，Service Worker 只缓存公开页面壳。
本次只修复浏览器验收入口、夹具前置条件和引擎覆盖，不改变生产协议或离线状态机。

修改前已从代码确认：

- `scripts/check-web-release.mjs:94–96` 启动离线文件时没有设置 `WEB_OFFLINE_FIXTURE`；
  `clients/web/scripts/test-server.mjs:163–171` 因此不传入离线开关、签发器及清除密钥。
- `server/internal/transport/httpapi/web_offline.go:47–51` 返回 `enabled: false`，
  `clients/web/src/offline-page.tsx:244–246、389–397` 据此持续禁用创建按钮。
  填写口令和勾选授权不会绕过服务端能力开关，这是产品的正常保护。
- `clients/web/tests/browser/offline.spec.ts:193–205` 未检查能力就直接改写
  `navigator.storage.persist/estimate`，遇到缺少 Storage API 的 WebKit 会抛出 TypeError。
- 同文件 `16–29、108–120、219–231` 的三个完整场景直接启动 Chromium，
  即使项目名为 Firefox/WebKit 仍运行 Chromium，不能作为对应引擎证据。
- 既有 #39 验收命令要求同时设置 `WEB_WORKSPACE_FIXTURE=1 WEB_OFFLINE_FIXTURE=1`，
  且只承诺 Chromium 完整流程；发布入口没有保留这两个前提。

开发方案：

1. 增加显式离线验收命令，集中设置本地教学模型和离线签发器夹具；发布矩阵复用入口。
2. 在离线测试开始前断言真实服务端能力，缺配置时快速报错；创建前断言按钮可用。
3. 用显式标签限定依赖 Chromium 持久存储授权的四个原场景；其他引擎实际执行能力
   探测及缺少 Storage API / BroadcastChannel 时的拒绝保存测试，不伪造 API 支持。
4. 保留真实签发、断网、跨页、重启、丢响应及清除断言，不增加超时、不使用 skip 或期望失败。

配置修复后的精确复验另确认一项测试时序问题：Playwright 1.62.1 的 Chromium
`doUpdateOffline` 先逐页更新网络，再更新 Service Worker。重启持久配置后，页面
`online` 事件发出的身份查询仍可能得到 `ERR_INTERNET_DISCONNECTED`，同步请求未发出，
不是已发送答案的状态机卡住。切换完成后相同身份查询返回 200。
因此补充测试夹具：拦截切换期间的 `online`，待 `setOffline(false)` 完成后再交付一次
重连事件；仍由生产代码查询身份、自动同步，仍通过真实服务提交后丢响应验证原操作核对。

验收标准：

- 独立空数据库和生产资产下，四个原 Chromium 场景全部在既定预算内通过。
- Firefox/WebKit 使用自身引擎验证实际能力及缺少能力时不创建离线库、不写明文。
- 未启用离线夹具时在前置检查明确失败，不再等待创建按钮超时。
- 前端单测、相关脚本检查、发行构建及改动格式检查完成；既有失败单独记录。

## 验收记录

验收日期：2026-09-20。使用本任务独占的 PostgreSQL 17.11 临时容器，每轮创建全新数据库，
并持有宿主 PostgreSQL 验收锁。浏览器运行在本机已有的
`mcr.microsoft.com/playwright:v1.62.1-noble` 镜像内，未下载镜像或安装依赖。

运行原提交的测试快照，实际确认：

- Chromium 的持久化拒绝用例在创建按钮处达到 60 秒超时；真实
  `/v1/web/offline/capabilities` 返回 `enabled: false`，与代码链一致。
- WebKit 同一用例在修改 `navigator.storage.persist` 时抛出原报告的 TypeError。
- 补齐配置后，重启场景的前置身份查询因模拟网络时序失败；记录到 `sync` 调用次数为零，
  切换完成后的身份查询为 200。仅调整测试中重连事件交付顺序后，该原场景通过，
  仍断言 `sync → status`，且在线会话保持不变。

| 检查 | 实际结果 |
| --- | --- |
| `npm run test:offline -- --project=chromium --reporter=line,json` | 7 项通过，28.5 秒；包含原四项完整离线场景 |
| `npm run test:offline -- --project=firefox --reporter=line,json` | 3 项通过，3.8 秒；仅声明能力探测和拒绝保存覆盖 |
| `npm run test:offline -- --project=webkit --reporter=line,json` | 3 项通过，2.5 秒；真实缺少 Storage API 时明确拒绝保存 |
| 三份 JSON 报告交给既有 `checkBrowserResults` | 分别确认 7 / 3 / 3 项实际通过，无 skip、期望失败或重试 |
| 不设置夹具，直接选择持久化拒绝用例 | 在 `beforeAll` 明确报告需使用 `npm run test:offline`；没有等待禁用按钮超时 |
| `GOPROXY=off go -C server build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过，嵌入生产资产 |
| `node --test scripts/web-release-results.test.mjs` | 2 项通过 |
| 两个修改脚本的 `node --check`、修改的 Web 文件格式检查、`git diff --check` | 通过 |
| `npm run check`、`npm test` | 无法执行：分别缺少 `tsc`、`vitest`，退出码 127；不计为通过 |

本工作树没有 `node_modules`。浏览器检查复用本机已安装的 Playwright 1.62.1，临时模块
解析把 `@playwright/test` 和其 CLI 转发到同版本正式模块；已读取 npm 缓存中的正式包，
核对其 exports 和原本的 `playwright/test` 转发关系。实际执行了新增的 `test:offline`
入口，未用模拟 CLI 代替。临时解析文件和 JSON 报告保存在仓库外的
`/tmp/edu-issue49-8LTZIB/`，不提交。

本轮没有重新运行 Vite：复用了任务 62 保留的生产 Web 资产，已用 `diff -qr` 确认两份
`clients/web/src` 完全一致，且本次未改生产源码。嵌入的主 JS SHA256 为
`8b060c1f945f932b67146efcef347bf39bd3adedeff23ad1c3d042f47a12ab83`。
锁文件 SHA256 为 `88c4c869505729bacd21503174cd4e508e9115a27e06aaee19f4223677c7f732`，
未修改依赖或锁文件。尚未获得安装授权，所以完整前端类型检查、单测和重新打包未验证。
未运行与此次测试夹具无关的整仓 Go、CLI 或完整 Web 发布矩阵。

## 复验入口与交付边界

依赖按锁文件就绪并完成生产构建后，为每个引擎设置独立空的 `TEST_DATABASE_URL`：

```bash
cd clients/web
WEB_RELEASE_MATRIX=1 npm run test:offline -- --project=chromium
WEB_RELEASE_MATRIX=1 npm run test:offline -- --project=firefox
WEB_RELEASE_MATRIX=1 npm run test:offline -- --project=webkit
```

上面三条命令之间必须更换独立数据库；Chromium 的真实隐私清除会改变该库的 learner generation。
可用 `WEB_CHROMIUM_PATH` 指向已安装的兼容 Chromium。
本任务只提交专用工作分支供审查，不推送或合并基础分支。
