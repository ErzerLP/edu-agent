# Issue #50：动态进度范围选择器定位修复

## 问题确认

基线为 `d591bcc`，分支为 `task/72`，初始工作树干净。已阅读项目开发流程、
测试策略、Web 路由与动态进度设计。项目由 Go 服务、PostgreSQL、React Web、
Go CLI 和共享 agentcore 组成；学习事件和进度投影属于 learning，Web 通过
同域认证调用已有进度 API，Nocturne 和 NoteSync 为独立集成。

报告中的超时可以复现，但“页面缺少范围选择器”并不准确：

- `clients/web/src/progress-page.tsx:164` 已渲染“范围”下拉框，并在切区时
  更新 URL 的 `space`、清除 `goal`；进度查询通过学习区请求头和 `global`
  参数交给服务端过滤，不缺少另一套跨区功能。
- 原 `clients/web/tests/browser/workspace.spec.ts:196` 使用
  `getByLabel('范围', { exact: true })`。页面的标签包含整个 select，
  Playwright 标签文本包括选项文字，因此不等于“范围”。同场景原第 222 行
  对“列表”的定位存在相同问题。
- 使用已有 Playwright 1.62.1 和 Chromium，对相同 HTML 结构执行最小案例：
  select 可见，标签文本为“范围全部学习区进度验收英语区”，精确标签匹配数为 0，
  `selectOption` 在 1 秒内超时，命令退出码为 1。
  同一页面的可访问性快照明确显示 `combobox "范围"`。

## 开发方案与验收标准（实施前）

本次是一个测试定位缺陷，保持单批交付：将动态进度场景中的“范围”和“列表”
改为通过 `combobox` 角色和精确可访问名称定位，补充范围初始值、选择值及 URL
断言。不改变产品页面、API、数据库、依赖版本或超时预算。

验收要求：

1. 相同最小案例中，按角色及名称能唯一找到可见下拉框，并成功选择另一学习区。
2. 独立空 PostgreSQL 数据库及真实 Go/Web 资产上的动态进度场景通过：
   两区三目标初始可见，切区后只剩该区目标，续学、原答案、移动视口、
   投影失败提示与复习列表仍正常。三个浏览器引擎分别使用独立数据库，串行执行。
3. 执行 Web 单测、类型检查、构建和 diff 检查；已有无关失败单独记录，
   不把跳过或未运行的检查写成通过。

## 验收结果

已修改上述两处测试定位并补充范围断言，生产代码未变更。

| 检查 | 实际结果 |
| --- | --- |
| 修复前 Chromium 最小案例 | 下拉框可见，旧定位匹配数为 0，选择操作超时，退出码 1 |
| 修复后 Chromium 最小案例 | “范围”和“列表”均唯一匹配可见控件，分别选中另一学习区和复习列表 |
| 三引擎最小回归 | 复用本地已有 `mcr.microsoft.com/playwright:v1.62.1-noble` 镜像及 Playwright 包；Chromium、Firefox、WebKit 串行通过，分别确认旧定位匹配数为 0、新定位唯一可见且选值成功 |
| `npm test` | 未能启动：`vitest: not found`，退出码 127 |
| `npm run build`（包含 `npm run check`） | 未能启动类型检查：`tsc: not found`，退出码 127；未执行 Vite 构建 |
| `git diff --check` | 通过 |
| 真实 Go/Web/PostgreSQL 动态进度场景 | 未运行；工作树缺少 `node_modules`，尚无本次构建的生产 Web 资产 |

最小案例使用与生产页面相同的原生标签结构：

```html
<label>范围<select><option value="">全部学习区</option><option value="other">进度验收英语区</option></select></label>
<label>列表<select><option value="goals">活动、概念与继续位置</option><option value="reviews">到期复习</option></select></label>
```

通过 Playwright `page.setContent` 载入后，对两个控件分别断言旧
`getByLabel(name, { exact: true })` 的匹配数为 0，以及新
`getByRole('combobox', { name, exact: true })` 的匹配数为 1、可见、
`selectOption` 后的 `inputValue` 等于期望值。该证据只证明浏览器定位修复，
不代替真实跨区数据过滤、Go 服务或数据库验收。

用户提供的 AGENTS.md 明确要求安装依赖另行授权。首次交付时已请求按现有锁文件
执行 `npm ci`，未获得安装授权，未安装或升级依赖，也未修改锁文件。
后续用户接受本次交付并明确授权合入 `main`、推送及处理对应 issue；上述完整项目
验收缺口仍保留，不将接受交付视为检查通过。
