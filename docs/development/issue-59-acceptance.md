# Issue #59：NoteSync 任务中心可见性验收

## 问题确认与架构

基线 `dc03439`，工作树初始干净。React/TanStack 页面通过 NoteSync 专用任务适配器
读取 Go HTTP 审阅接口，复用知识域、PostgreSQL 审阅及操作收据；本问题位于浏览器
验收定位器，不需要改变同步解决、持久化或权限合同。

- `clients/web/tests/browser/notesync.spec.ts:119` 在全页按“已解决（resolved）”
  查找并取第一个匹配项。
- `clients/web/src/notesync-page.tsx:77–78` 先渲染包含同名文本的状态下拉选项；
  `85–88` 行随后才渲染各审阅卡片、状态和同一审阅链接。
- 因此即使真实卡片已显示已解决状态，原定位器仍会选中隐藏的 `option`。
  下一行同样使用首个链接，不能保证核对的是本次解决的审阅。

## 开发方案

1. 只修改 NoteSync 浏览器验收：在同步审阅列表内按本次 `reviewID` 的链接定位卡片，
   在该卡片内断言状态，避免筛选选项、旧审阅或其他任务满足断言。
2. 点击该卡片的“查看同一同步审阅”，核对完整 URL，继续原有正文及解除引用检查。
3. 保留原超时、失败策略和全部后续断言；不修改产品代码，不处理另行登记的 #61。

## 验收标准

- 用原定位器复现隐藏 `option` 失败，确认同页实际卡片的已解决状态可见。
- 修改后完整用例通过，包括返回原审阅、解除集合引用、正文销毁、访问拒绝、刷新后
  无正文恢复和浏览器存储不含正文；不以单独通过可见性断言替代完整验收。
- Chromium、Firefox 串行回归；WebKit 使用全新独立库定向检查，并如实记录独立失败。
- 运行 `make web-release-check`，覆盖生成类型一致性、Web 类型和单测、相关 Go 契约与
  静态检查、生产构建；最后检查 `git diff --check`。

## 实际结果

### 最小浏览器复现与回归

使用本机已缓存的 Playwright `1.62.1` 和 Chromium `151.0.7922.34`，按
`SyncReviewList` 的真实元素、文本及顺序构造最小页面（下拉筛选在审阅卡片之前）。
运行 `node /tmp/edu-agent-issue59.uWN5eI/locator-repro.mjs`，得到：

```text
Locator: getByText('已解决（resolved）').first()
Expected: visible
Received: hidden
locator resolved to <option value="resolved">已解决（resolved）</option>
```

同页卡片内的状态断言通过；该 `option` 的布局尺寸为 `0×0`，卡片状态段落可见。
换用本次修复的区域／卡片／审阅 ID 定位器后，状态可见及原审阅链接断言通过。
再加入同名的其他已解决卡片，并把目标审阅改为 `open`：只定位到目标卡片，
不会因其他卡片或筛选选项中的“已解决”误判成功。
最小复现文件为仓库外临时证据，不纳入提交，也不替代真实服务端端到端验收。

### 验证边界

- `git diff --check` 通过。
- 当前工作树缺少 `clients/web/node_modules` 和 `server/edu-agentd`。已经按用户全局
  规范请求锁定依赖安装和本任务临时容器授权，尚待回复。
- 完整 `notesync.spec.ts`（Chromium／Firefox／WebKit）及
  `make web-release-check` 尚未执行，后续解除引用与正文清理不能计为验收通过。
- 未修改产品代码、API、锁文件、超时或失败重试策略；#61 的候选数据状态问题不在本次范围。
