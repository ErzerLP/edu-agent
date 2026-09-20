# Issue #53：WebKit 跨标签页清除导师正文

## 问题确认与开发方案

基线 `d591bcc`，初始工作树干净。已核对项目开发规范、分层测试策略、导师运行及历史设计。
Go 服务与 PostgreSQL 保存权威状态，共享 Agent 核心执行模型；React 学习端通过同源
Cookie API 和共享 SSE 订阅观察运行，CLI 为独立客户端。本项仅修复 HTTP 通知时序。

基线 `server/internal/transport/httpapi/mentor_runs.go:270–320` 的 `mentorEvents` 每 500 毫秒
检查事件，但无事件时默认等十秒才写心跳。客户端 `clients/web/src/api/mentor.ts:88–99`
的 `watch` 必须读到完整 SSE 帧才重读快照。
清除页在命令成功后主动重读，第二页依赖这条通知。

使用本机已有的 Playwright 1.62.1 镜像与依赖，模拟同样的初始心跳和清除事件：WebKit
读取事件的前 128 字节后，5.5 秒内没有交付尾部，Chromium 约 0.5 秒收到完整帧。
增加 SSE 注释填充、指定 UTF-8 或 `nosniff` 均未解决；下一次独立心跳写入会释放尾部。
行为与 [WebKit 流读取缺陷 322545](https://bugs.webkit.org/show_bug.cgi?id=322545) 一致。
本次依据为实际浏览器实验，不将上游已修复等同于项目锁定引擎已包含修复。

开发方案：每批事件后，在下一次原有轮询中补发一次心跳；继续经过原身份、权限、隐私
门禁和写超时检查。空闲心跳配置、事件序号、快照协议、前端和数据库结构保持原合同。
这一批次只解决通知及时送达，不扩展为 SSE 重构或浏览器依赖升级。

## 验收标准

- 原 WebKit 最小场景在五秒内收到完整事件，Chromium、Firefox 行为正常。
- 真实 PostgreSQL 与 Cookie/SSE 路径中，清除通知身份和版本正确，通知后及时补发心跳。
- 清除后快照不含正文，观察不会重新调用模型；受影响 HTTP 包测试、vet 和构建通过。
- 明确记录原完整浏览器用例和其他未运行检查，不以最小实验替代整套 UI 验收。

## 验收结果

### 实施与回归

- `mentorEvents` 在事件批次后记录待补发状态，下次轮询无新事件时写入一次心跳，
  然后回到既有空闲心跳策略；每批结束最多额外一帧，不增加数据库轮询频率。
- 在既有真实 PostgreSQL/Cookie/SSE 测试中增加清除场景，核对通知身份、连续序号、
  版本、后续心跳、清除后的空快照和模型调用次数。
- 浏览器原用例先等待第二页 SSE 订阅成功，再执行清除，避免初始快照请求掩盖通知问题。

### 已运行

2026-09-20，Go 1.26.6、本机已有 `postgres:17` 独立临时容器，数据库专供本任务。
数据库测试串行执行。以下 Go 命令均在 `server/` 下运行，使用 `GOPROXY=off`，
数据库命令均显式设置该临时库的 `TEST_DATABASE_URL`。

| 检查 | 结果 |
| --- | --- |
| 修复前 `go test -count=1 ./internal/transport/httpapi -run '^TestPostgreSQLMentorCookieHTTPAndSSERecovery$'` | 新回归按预期失败：收到清除通知，三秒内未收到后续心跳 |
| 修复后同一精确回归 | 通过，2.908 秒 |
| `go test -p=1 -count=1 ./internal/transport/httpapi` | 通过，24.198 秒，启用真实 PostgreSQL |
| `go vet ./internal/transport/httpapi` | 通过 |
| `go build ./cmd/edu-agentd` | 通过；普通服务端构建，未生成 Web 发布资产 |
| `node --check --experimental-strip-types clients/web/tests/browser/mentor.spec.ts`（仓库根目录） | 语法检查通过，不代表 TypeScript 类型检查或浏览器执行 |
| `git diff --check` | 通过 |

另用现有 `mcr.microsoft.com/playwright:v1.62.1-noble` 镜像及 Playwright 1.62.1 依赖运行
双标签页最小 HTTP 实验：初始快照显示原文，两页订阅 SSE；第一页清除后主动重读，
第二页沿用生产客户端的帧拼接/解析方式，在完整事件到达后重读。实验只切换事件批次后
是否在下一轮 500 毫秒轮询补发心跳，断言保持五秒超时。

| 引擎 | 原发送时序 | 修复后的发送时序 |
| --- | --- | --- |
| WebKit | 5.029 秒后第二页仍显示“浏览器真实导师：已读取本次绑定目标。” | 1.017 秒内两页清空 |
| Chromium | 0.506 秒内两页清空 | 0.516 秒内两页清空 |
| Firefox | 0.505 秒内两页清空 | 0.492 秒内两页清空 |

### 未运行范围

最小实验使用受控 HTTP 快照与 SSE，不是完整 React 页面验收。当前工作树未安装前端
依赖，遵循用户的安装授权规则没有执行 `npm ci`。因此未运行前端类型检查、单测、
Web 发布构建及原 `mentor.spec.ts`；安装授权已询问，记录时尚未收到回复。
复核者准备好锁定依赖和浏览器后，可在独立空数据库上运行：

```bash
# 先按项目文档构建 Web 资产和服务端，再在 clients/web/ 下执行。
TEST_DATABASE_URL='postgres://专用空测试库' \
WEB_MENTOR_FIXTURE=1 WEB_RELEASE_MATRIX=1 \
npm run test:browser -- mentor.spec.ts --project=webkit --reporter=line
```

当前配置必须设置 `WEB_RELEASE_MATRIX=1` 才会注册 WebKit 项目；issue 原示例省略了此项。
生产前端、依赖、公共协议、数据库结构未改，不扩展运行无关 CLI 或全仓候选矩阵。
