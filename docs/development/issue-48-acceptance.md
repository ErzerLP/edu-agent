# Issue #48：配对后直达设置页的测试同步

## 核实与原因

基线 `d591bcc`，初始工作树干净。已核对开发工作流、分层测试策略、模型设置和
导师历史设计，以及 Go 服务、PostgreSQL、React 同源会话和设置查询的调用关系。

原 `clients/web/tests/browser/tutor-history.spec.ts:24–25` 点击配对按钮后立即调用
`configure`，后者在第 7 行执行整页导航 `page.goto('/app/settings')`。
点击完成并不代表异步配对完成：`clients/web/src/lib/session.tsx:58–67` 先等待
`POST /v1/web/pairings` 的响应，再调用 `onPaired` 接纳会话；服务端
`server/internal/transport/httpapi/web.go:209–219` 在配对成功后才设置 Cookie。
因此提前导航可能中断配对请求，或使下一页读取会话时尚未取得 Cookie。此时
`SessionProvider` 显示配对表单，测试等待的导师配置区域自然不会出现。

原 `mentor.spec.ts:19` 等待登录后才出现的导航链接，`settings.spec.ts:23` 显式
等待首页标题，两者已有同步点。直接访问设置本身已实现：前端
`clients/web/src/main.tsx:77–81` 注册路由，服务端 `web.go:276–277` 返回 SPA 入口。
`settings-page.tsx:394–407` 根据设置查询结果渲染导师卡片，与进入方式无关。
本次据代码确认的是导师历史测试的配对/导航竞态，不能据此宣称产品缺少设置路由。

## 开发方案与验收标准（实施前）

保持一个局部修复，不涉及业务接口、数据库迁移、身份权限或模型配置语义。

1. 导师历史测试在点击配对后等待登录成功的首页标题，再保留原来的设置直达路径。
2. 设置测试在已完成配对后整页直达 `/app/settings`，检查导师配置按钮可编辑，
   刷新后仍可编辑，普通学习身份仍不可编辑。
3. 使用独立空 PostgreSQL 和本地导师夹具运行原导师历史用例；运行设置回归，
   覆盖 Chromium、Firefox、WebKit。运行 Web 类型检查、单测和生产构建。
4. 仅提交任务相关文件；未执行或失败的验收如实记录。

## 验证记录

- 已按上述方案修改导师历史和设置浏览器用例，生产代码未改动。
- `cd server && go test ./internal/transport/httpapi -run '^TestWebRelease' -count=1`
  通过，覆盖实际已注册页面（包括 `/app/settings`）的服务端直达入口。
- 对两个修改的测试文件运行 `node --experimental-strip-types --check` 通过；
  这仅验证语法，不替代 TypeScript 类型检查或浏览器执行。
- `git diff --check` 通过。
- 当前工作树没有 Web 依赖，已请求按现有锁文件安装的授权；浏览器复现和
  Web 类型、单测、构建尚未执行。
