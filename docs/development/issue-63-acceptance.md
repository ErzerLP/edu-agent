# Issue #63 开发方案与验收

## 问题确认与架构

基线 `dc03439` 包含报告中的 `364fbe1`。React 浏览器测试通过 Cookie/CSRF
访问 Go HTTP API；privacy owner 在 PostgreSQL 中提交代次屏障，协调各 owner
清除并保存回执；Nocturne 远端验证另行推进。

已确认候选夹具的状态隔离缺口：

- `scripts/check-web-release.mjs:90–105` 逐浏览器、逐文件重启进程，但一直继承
  同一个 `TEST_DATABASE_URL`，持久数据不随进程重启隔离。
- `clients/web/tests/browser/offline.spec.ts:32、129、275` 的前序用例创建离线包持有
  设备；最后的清除场景只确认当前设备，并不清理前两个用例的持久持有记录。
  `server/internal/privacy/postgresstore/store.go:361–381` 将所有旧代次持有设备纳入
  清除子回执；尚未确认的设备使 `offline_device_cache` 保持 pending，阻止全局验证。
  重启进程或更换配对设备不会消除这些记录。
- `server/internal/privacy/postgresstore/store.go:194–199` 拒绝已有未验证清除时
  提交新清除；`server/internal/transport/problem/problem.go:228–230` 将其映射为
  `409 erasure_conflict`。`000004_memory_bridge.sql:517–518` 同时通过唯一索引
  强制单活动清除。这符合 `memory-bridge/spec.md` 的正式合同。
- `clients/web/tests/browser/memory.spec.ts:98` 无条件期待 202，因此共享数据库中
  第二次执行无法完成后续重新配对与回执核对。#47 的响应等待无法消除数据库状态。

禁用 Nocturne 并不必然导致永久未完成：`composition.go:270–276` 已接入禁用状态
验证器，没有远端历史且其他步骤完成时可以验证通过。本次修复不改变这一行为。
报告没有保留原候选库，因此不能断言原始 409 唯一由哪个子回执触发；上述前序状态
来自当前矩阵代码，并由真实数据库最小场景验证。

## 开发方案与验收标准

保持单一测试基础设施批次，不改变生产隐私状态机、授权、代次或 API：

1. 在真实 PostgreSQL 上复现同一库、有效新 grant、当前代次下第二次清除的 409。
2. 候选矩阵每个浏览器/文件使用随机独立 schema，服务与配对/grant 子命令使用
   相同连接参数；结束后只清理该次创建的 schema，保留调用方原有数据。
3. 增加真实数据库回归，明确验证未完成清除继续拒绝新操作，原回执与代次不变。
4. Chromium、Firefox、WebKit 串行在同一基础测试库上通过隔离入口运行隐私场景，
   保留 202、本地清除成功、401、新设备/新代次、原回执核对等原断言。
5. 检查隔离工具成功/失败的退出与清理、Web 类型/单测/构建、相关 Go/PG 契约、
   vet、构建和 `git diff --check`。不将局部验证声明为完整发布矩阵通过。

## 验收结果

独立 PostgreSQL 17 容器 `edu-agent-task86-postgres`，仅监听本机 32986，基础库
`issue63`。没有访问业务数据库、真实模型或 Nocturne 服务。

最初扩展原 Go/PG Web 用例，按报告无条件期待第二次清除返回 202，实际失败：

```text
POST /v1/privacy/erasures: 409，预期 202
{"error":{"code":"erasure_conflict","message":"Privacy erasure conflicts with current state",…}}
```

随后将回归强化为前序离线设备持有包、未确认清除的持久状态，并重建服务组合、
显式推进后台恢复。结果仍为 `partial`、`offline_device_cache=pending`；新配对的
设备使用新 grant 和当前代次发起清除，返回 409。回归断言新操作没有回执、当前
会话有效且代次不变、原回执身份和版本不变。没有将所有 409 当作可接受结果。

修复范围：候选运行器构建测试专用 `server/testutil/webdatabase`，每个浏览器/文件
通过它启动原命令。服务及 CLI 继承同一个带随机 `search_path` 的连接；不回退到
调用方原 schema，退出后清理本次 schema。测试失败或清理失败仍使候选失败。
浏览器原 202 断言保留，并将真实响应正文附在断言失败信息中，便于定位错误码。

已执行：

```bash
# 以下数据库测试均显式配置本任务的 TEST_DATABASE_URL，没有数据库 skip。
go -C server test -p=1 -count=1 ./testutil/webdatabase ./internal/app \
  -run 'TestScopedURL|TestPostgreSQLBrowserSchemaIsolationAndCleanup|^TestPostgreSQLWebMemoryApprovalPrivacyAndRevocation$' \
  -timeout=2m
# 加入遗留离线状态和重建服务后，重跑修改过的用例。
go -C server test -count=1 ./internal/app \
  -run '^TestPostgreSQLWebMemoryApprovalPrivacyAndRevocation$' -timeout=2m
go -C server vet ./internal/app ./testutil/webdatabase
go -C server build ./...
node --check scripts/check-web-release.mjs
node --test scripts/web-release-results.test.mjs
git diff --check
```

以上通过。隔离工具的 PostgreSQL 回归覆盖同一基础数据库中的嵌套候选、前序表
不可见、成功/失败后的清理、原有数据保留，以及 URI/libpq 连接参数保留。

另外将当前源码构建为真实 `edu-agentd` 进程，使用隔离工具连续运行两次 API
冒烟流程（Node 内置 fetch/child_process，无新增依赖）：启动服务、CLI 签发配对码
和 grant、清除返回 202、本地 identity/knowledge/learning 步骤成功、新设备配对、
按原 operation/device 读取相同回执。两次都从代次 1 清除至 2，均通过，说明第二次
运行不再继承前一次候选状态。测试脚本和二进制保存在
`/tmp/edu-issue63-verify.2J5FLb/`，不提交为项目文件。

未运行：Chromium/Firefox/WebKit、本工作树 Web 类型/单测/生产资产构建及完整发布
矩阵。工作树没有 `node_modules`，已询问锁文件固定版本的 `npm ci` 安装授权，尚未
收到答复；遵守全局 AGENTS.md 的安装授权要求。上述 API 冒烟不冒充浏览器验收，
当前 Go 构建也不冒充带 `web_release` 的资产构建。补齐授权后按本文件的三个浏览器
标准继续验收，不需要改变生产隐私门禁。
