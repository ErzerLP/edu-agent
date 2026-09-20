# Issue #51：旧会话来源原文验收

## 架构与问题确认

项目由同源 React/TypeScript Web、模块化 Go 服务、PostgreSQL 权威存储和独立 Go CLI 组成。
本场景经 knowledge 检索、learning 冻结活动引用、learningcontent 适配旧活动，最后由 Web 来源对话框读取指定内容版本。

确认存在的是浏览器验收对引用顺序的错误假设：

- `clients/web/tests/browser/workspace.spec.ts:73` 导入含定义的 `even.md`，`:75` 导入仅含教学说明的 `examples.md`。
- `server/internal/knowledge/retrieval.go:419` 对等分文档按版本 ID 排序；版本 ID 由含随机文档身份的内容派生，不保证文件名顺序。
- `clients/web/scripts/workspace-model.mjs:18` 将命中顺序直接用于活动引用。
- `clients/web/src/teaching-page.tsx:650` 按冻结引用顺序生成“原资料依据 N”。原测试在 `workspace.spec.ts:281` 固定点击第 1 条，却断言仅存在于 `even.md` 的句子。
- `server/internal/knowledge/postgresstore/content.go:97` 核对历史切片及摘要，`clients/web/src/components/content-blocks.tsx:196` 渲染该切片；未发现原文截断或丢失。

修改前用真实测试模型构造两种命中顺序：`[even, examples]` 通过原断言，`[examples, even]` 触发断言失败。
随后使用真实服务、独立空 PostgreSQL 与 Chromium，将定义固定在第 2 条引用，仍点击第 1 条：
5 秒后 `toContainText('偶数可以被 2 整除')` 失败，实际显示 `偶数的阅读与核对`、教学说明和 `出处：examples.md`，与报告一致。
这是来源定位的测试问题；不同运行生成的资料身份可以解释不同浏览器上的通过/失败差异。

## 开发方案与验收标准

单批次仅修正原浏览器测试：从已冻结活动的引用原文找到目标句子，使用对应序号打开来源。
找不到该引用时前置断言明确失败；保留原有固定句子、关闭后焦点恢复和后续教学流程断言。
验收要求覆盖定义位于第 1、2 条引用及缺少定义三种情况。不改产品排序、来源 API、历史资料或 fixture 正文。

## 验证结果

基线 `d591bcc`，2026-09-20 执行：

| 检查 | 结果 |
| --- | --- |
| Node 最小复现，使用真实 `workspaceModel` | 修改前交换引用顺序后失败 |
| Node 提取修改后的定位表达式 | 两种顺序均选中定义；缺少定义返回 `-1`，由前置断言拒绝 |
| Chromium 定向场景，旧定位、定义在第 2 条 | 真实来源对话框原文断言在 5 秒后失败 |
| Chromium 定向场景，新定位、定义分别在第 1、2 条 | 2 passed；原文与焦点恢复均通过 |
| `go build -C server -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过 |
| `git diff --check` | 通过 |
| `npm run check` | 无法启动：`tsc: not found` |
| `npm run test:browser -- workspace.spec.ts --project=chromium --reporter=line` | 无法启动：`playwright: not found` |

定向浏览器验证复用本机已有的 Playwright 1.62.1 与同版本容器，不安装依赖。
Web 资源来自现有本地构建，已比较其 `clients/web/src`、`package-lock.json` 和 `vite.config.ts` 与当前工作树完全一致；Go 服务从当前源码重新构建。
PostgreSQL 17 使用本任务专用容器及两个独立空数据库，服务和浏览器位于隔离网络，不复用其他任务的服务。
临时定向场景复用原 `legacySession` 建立流程，仅在提交提案前控制两份真实命中的顺序；通过真实配对、导入、冻结、内容适配和来源请求验证，没有 mock 来源响应。
修复验证直接读取当前测试文件中的定位表达式。

当前工作树缺少 Web 依赖，未获得安装授权。因此未运行完整 workspace 浏览器套件、Web 类型检查、单测及重新打包；上述定向通过不代表这些检查通过。
