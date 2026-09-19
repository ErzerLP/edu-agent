# Issue #40：追踪核查与 companion 部署修复

## 核查范围与确认

核查日期：2026-09-19；基线：`ada8e87`；任务分支：`task/61`。
工作树初始干净。#40 是 #18–#39 的实施与验收总览，没有另行定义单一功能。
当前分支已包含这些任务的实现提交；不能据此推断全部验收已通过。
现有 [#37 验收记录](issue-37-acceptance.md) 已追踪 A01–A36，明确保留未完成项。

架构为 Go 模块化服务、PostgreSQL 权威状态、同源 React Web、原生 Go CLI，
两端共用 `packages/agentcore`。`server/internal/app/app.go` 已接通研究开学、
内容与教学变更；`goal-composer.tsx` 区分仅保存和授权开学；CLI `study` 已接入
同一学习服务。可选 companion 通过独立程序主动请求服务端，服务端没有 OS 执行器。

修改前执行：

```bash
cd server
GOPROXY=off GOTOOLCHAIN=local go test ./api ./internal/transport/httpapi \
  -run '^(TestStartLearningContract|TestWebRelease.*|TestWorkspaceProxyAllowlist|TestWebProduction.*)$' -count=1
```

`TestWebReleaseProxyCoversClientAPI//v1/companion/browser` 失败：
`Web 正式 API 在部署代理上返回 404`。开学合同、页面入口与身份配置检查通过。
这是实际部署接线缺陷：

- `clients/web/src/api/companion.ts:19` 请求 `/v1/companion/browser`。
- `clients/cli-go/internal/companion/client.go:84`、`:100` 分别请求 `attach`、`channel`。
- `server/internal/transport/httpapi/api.go:316`–`:320` 已注册这三个正式接口。
- 基线 `deploy/web/nginx.conf:51`–`:61` 没有 companion 转发规则，最终返回 404。

浏览器创建配对、能力查询及本机接入/轮询都会被代理拦截；不是功能开关关闭或接口改名。

## 修复前方案与验收标准

1. 仅补齐 `/v1/companion/(browser|attach|channel)` 三个精确路径的反向代理，
   保留 Host 并禁用代理缓存。功能开关、Origin、Cookie/CSRF、配对码和通道 MAC
   继续由已有 Go handler 校验，不新增接口或修改 OpenAPI。
2. 在现有代理边界回归中增加三个允许路径，以及根路径、未知操作、尾斜线和子路径
   的拒绝断言；先确认新增回归失败，再修改配置。
3. 更新部署使用说明，明确两端都需要这三个转发路径。
4. 验收要求：原失败回归和新增边界回归通过，相关 API/HTTP 测试与 vet 通过；
   使用本机 Nginx 检查配置并验证实际路由，未知路径仍返回 404。

本批次只修复核查中确认的部署缺陷，不以此关闭 #40 或宣称 #37/#38 已全部验收。
不安装依赖、不调用外部提供商、不部署用户服务、不合并或推送基分支。

## 实际验证

修改生产配置前，新增断言执行 `go test ./api -run '^TestWorkspaceProxyAllowlist$'
-count=1 -v` 失败，明确报告 `/v1/companion/channel` 未被允许，确认本机端同样漏接。
配置修复后，上述原失败检查及新增边界检查全部通过，没有删减断言。

| 检查 | 本次结果 |
| --- | --- |
| 原定向 API/HTTP 回归命令 | 通过；浏览器 API、页面入口、开学合同及代理边界正常 |
| `GOPROXY=off GOTOOLCHAIN=local TEST_DATABASE_URL= go test -json ./api ./internal/transport/httpapi -count=1`（server） | 两个 package 通过；顶层用例 134 通过、13 skip、0 失败；skip 不计数据库证据 |
| `GOPROXY=off GOTOOLCHAIN=local go vet ./api ./internal/transport/httpapi`（server） | 通过 |
| `GOPROXY=off GOTOOLCHAIN=local go test ./internal/command -run '^(TestStudy.*\|TestWorkbenchStudy.*)$' -count=1 -v`（CLI） | 5 个用例通过；研究别名、帮助、原审批依据、原课堂与运行会话复用 |
| `node --test scripts/web-release-results.test.mjs` | 2 个用例通过；skip/空选择/失败不冒充发布证据 |
| 本机 Nginx 1.24.0 配置检查与真实 HTTP 转发 | 通过；三个 POST 路径各到达本机测试上游一次；请求体、Host、Origin、CSRF/MAC 头及 no-store 响应保留 |
| Nginx 拒绝边界 | 7 个 companion 根/未知/后缀路径及 `/admin`、`/internal/privacy`、`/mcp` 均返回 404，没有抵达上游 |
| `gofmt -l`、`git diff --check` | 通过 |

Nginx 验证使用从生产配置机械派生的临时夹具，仅更换监听/上游为独立 loopback
随机端口并去掉 TLS 证书指令，所有 location 规则保持原文。上游是本机 HTTP 夹具；
没有启动用户部署，也没有把代理测试当作真实身份认证或本机命令执行的端到端证据。
测试进程已退出。临时配置留在本机任务临时目录，没有加入仓库。

Web 依赖与独立 `TEST_DATABASE_URL` 未配置。本次未运行前端构建/浏览器、真实
PostgreSQL、TLS 部署、真实提供商或 macOS 原生验收；这些未检查项不计为通过。
本次只修改部署配置、已有回归和文档，无需更改 API、数据库或前端资产。
#40 的整体完成结论仍以各 owner 和 #37 的完整验收为准。
