# Issue #52：WebKit 教学工作区横向溢出

## 架构与问题确认

基线 `d591bcc`，初始工作树干净。项目由 Go 服务、PostgreSQL、同源
React/TypeScript Web 和独立 Go CLI 组成。旧教学会话沿用 tutoring/learning
状态与正式作答协议，learningcontent 适配版本化正文；本次问题在 Web 布局层。

修复前使用本机已有的 Playwright 1.62.1 与同版本容器内的 WebKit，直接加载
`clients/web/src/styles.css`，复用教学三栏结构和“调整模式”的真实选项形成最小案例。
768px 视口下 WebKit 的 `document.documentElement.scrollWidth` 为 795，
Chromium 为 768。移除长代码块后仍然失败，排除了正文代码滚动区。

- `clients/web/src/styles.css:231`：平板视口的导师栏固定为 280px。
- `clients/web/src/components/change-panel.tsx:77`：导师栏内嵌面板，
  `:79` 的原生下拉框包含“自适应：同目标调整在安全点应用”。
- `clients/web/src/styles.css:482`：标签使用 Grid，控件在 `:488` 起设置
  `min-width: 0; width: 100%`，但没有覆盖原生下拉框的空白处理。
- WebKit 中下拉框和标签的可见宽度均已缩为 188px，标签的滚动宽度仍为
  273px；较长选项的内部溢出继续传到页面。单独添加 `max-width: 100%`
  无效，给下拉框设置 `white-space: normal` 后页面宽度恢复为 768px。
- 原端到端测试位于 `clients/web/tests/browser/workspace.spec.ts:286`，
  已有四种视口、两种主题的整页溢出断言。

## 开发方案与验收标准（实施前）

只修改教学工作区下拉框的空白处理，保留原生控件和完整选项。
在既有端到端场景内补充导师页签、控件局部溢出及代码滚动检查。
本次为单一布局修复，无需拆分交付，不改变服务端、数据库或教学语义。

验收要求：

1. WebKit 最小案例由失败变为通过，Chromium、Firefox 同案例无回归。
2. 深浅主题、390/768/1280/1440px 下页面及下拉框标签均无横向溢出，
   窄屏切到导师页签仍成立。
3. 下拉框可用键盘切换；长代码保持局部滚动，不依靠整页裁剪掩盖问题。
4. 完整工作区浏览器场景以及前端类型检查、单测、生产构建通过。

## 验证记录

已完成的检查（2026-09-20）：

| 检查 | 结果 |
| --- | --- |
| WebKit 最小复现 | 修复前 768px 视口得到页面宽度 795px、标签宽度 188px、标签滚动宽度 273px；修复后页面宽度 768px，标签不再溢出 |
| WebKit、Chromium、Firefox 布局 | 两种主题 × 四种视口 × 三种引擎，共 24 组通过；每组分别检查学习和导师页签，共 48 次整页宽度断言 |
| 长代码块 | 各组均保留内部横向滚动，设置 `scrollLeft` 后确认可滚动 |
| 原生下拉框操作 | 三种引擎均可通过方向键及 Enter 切换至谨慎模式，也可重新选择自适应模式 |
| `git diff --check` | 通过 |
| 完整 `workspace.spec.ts` | 未运行：当前工作树无前端依赖，等待按锁文件安装的授权 |
| `npm run check`、`npm test`、`npm run build` | 未运行：同上；不将最小布局案例视为这些检查通过 |

最小案例使用当前生产样式（仅跳过需要构建的 `@import` 与 `@custom-variant`），
实际控件文字、三栏及嵌套面板结构。它不依赖模型或数据库，也不替代完整 React 页面。
生产改动仅为 `.teaching-workspace select { white-space: normal; }`，
WebKit 可在窄控件内折行显示较长选项，保持原生外观、焦点和选择操作。
没有给页面或容器添加裁剪，也没有缩短选项或修改视口验收要求。

本机证据位于 `/tmp/edu-agent-issue52.g4b5Bg/`，包含 `layout.cjs` 和三个引擎的
768px 截图；它们是本地临时证据，不提交进仓库。运行方式：

```bash
docker run --rm --pull=never --network none --user 1000:1000 \
  -v /tmp/edu-agent-playwright/node_modules:/pw/node_modules:ro \
  -v "$PWD":/work:ro \
  -v /tmp/edu-agent-issue52.g4b5Bg:/evidence -w /pw \
  mcr.microsoft.com/playwright:v1.62.1-noble node /evidence/layout.cjs
```

待获准按现有锁文件安装依赖后，应构建 Web 与嵌入资产的 Go 服务，使用独立空库运行：

```bash
cd clients/web
npm ci --no-fund
npm test
npm run build
cd ../../server
go build -tags web_release -o edu-agentd ./cmd/edu-agentd
cd ../clients/web
WEB_RELEASE_MATRIX=1 WEB_WORKSPACE_FIXTURE=1 TEST_DATABASE_URL='专用测试库 URL' \
  npm run test:browser -- workspace.spec.ts --project=webkit --reporter=line
```

`npm run build` 包含前端类型检查。浏览器使用匹配的既有 Playwright 容器即可，
无需安装或升级浏览器。没有合并或推送当前分支。
