# Issue #60：旧会话工作区调整模式定位

## 架构与问题确认

核查基线 `dc03439`，初始工作树干净。Go 服务和 PostgreSQL 保存教学、资料及答案，
React 工作区通过同源 API 展示旧会话，生产前端资产嵌入 Go 发行程序。
`ChangePanel` 读取目标的教学调整模式；Playwright 通过旧协议创建会话并使用仓库模型 fixture，
验收阅读、版本、布局、讨论、作答、反馈和续学。本问题位于浏览器测试的控件定位层。

- `clients/web/src/components/change-panel.tsx:79` 已提供调整模式下拉框，
  原生 `<label>` 包住 `<select>` 及两项 `<option>`。
- 原 `clients/web/tests/browser/workspace.spec.ts:311` 使用
  `getByLabel('调整模式', { exact: true })`，但标签文本实际为
  `调整模式 自适应：同目标调整在安全点应用谨慎：先预览后采用`。
- 修改前，从当前组件提取原标签及选项，在 Playwright **1.62.1** 的 Chromium 中
  通过 `page.setContent` 复现：上述精确标签定位匹配 **0** 个元素；
  `getByRole('combobox', { name: '调整模式', exact: true })` 匹配 **1** 个元素，
  可访问性快照含 `combobox "调整模式"`。

因此问题成立：测试错误地把标签完整文本当成控件可访问名称，并非控件缺失。
最小复现不依赖数据库、模型或旧会话数据。外部工作项仅作为现象描述使用。

## 开发方案与验收标准（实施前）

仅将这处定位改为角色 `combobox` 加精确可访问名称，说明两种定位语义的区别。
保留可见性、标签和页面不横向溢出、长代码滚动、可访问性及后续学习闭环断言。
不修改生产控件、样式、API、模型 fixture、数据库或依赖版本。

验收标准：

1. 同一最小页面中，新定位唯一命中可见下拉框，并能切换两种模式。
2. Chromium、Firefox、WebKit 定向执行原“旧会话真实阅读”用例通过，
   覆盖深浅主题、390/768/1280/1440 像素及讨论、丢响应作答、反馈和继续教学。
3. `make web-release-check` 与 `git diff --check` 通过。
   数据库使用独立测试实例；环境缺失或未执行的检查明确记录，不记作通过。

## 验收结果

- **通过**：Playwright 1.62.1 / Chromium、390 × 1000 最小回归。
  原精确标签定位仍为 0 个；新角色定位唯一命中可见控件，
  默认值为 `adaptive`，切到 `cautious` 再切回 `adaptive` 均成功。
- **通过**：`git diff --check`；发布结果解析器的 2 项 Node 测试。
- **未通过环境前置检查**：`make web-release-check` 在依赖检查处退出，
  提示缺少本工作树的 Web 锁定依赖；尚未执行前端类型、单测及发行构建。
  机器报告：`/tmp/codeg-acp/801144-b1f08c2b/edu-web-release-XYLyko/report.json`。
- **尚未运行**：真实 PostgreSQL 和三浏览器的完整定向用例。
  工作树没有 `node_modules`、生产服务二进制或 `TEST_DATABASE_URL`，
  默认 Playwright 缓存也没有 Firefox/WebKit。
  根据用户的全局规范，已请求安装锁定依赖及缺失浏览器的授权；未把最小回归记成完整闭环通过。
