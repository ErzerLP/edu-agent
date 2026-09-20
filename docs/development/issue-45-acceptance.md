# Issue #45：目标进度标题链接可访问性

## 确认与原因

基线为 `d591bcc`，初始工作树干净。项目由 Go 服务端与 PostgreSQL、React Web、
Go CLI 和共享 `packages/agentcore` 组成，Nocturne、NoteSync 为外部集成。
Web 通过正式 HTTP API 获取学习事实，Vite 生产资产嵌入 `web_release` 服务端。
本次问题仅涉及 Web 展示及浏览器回归，无需改变 API、数据库或其他客户端。

`clients/web/src/pages.tsx:602` 在目标详情内复用 `ProgressPanel`。
`clients/web/src/progress-page.tsx:78` 的目标进度标题在同一个 `h3` 中排列目标链接及
学习区、状态文字。当前路由使链接带上 `active`、`data-status="active"` 和
`aria-current="page"`，与工单的 axe 目标吻合；这些路由属性本身不是错误。
`clients/web/src/styles.css:382–387` 仅给链接颜色及悬停下划线，Tailwind 的基础样式
重置了默认文字装饰，导致静止状态的标题链接只靠颜色区分。

修复前使用本机 Chromium 1234 渲染上述最小 DOM 和已有生产 CSS。
该资产所对应的 `styles.css` 与当前工作树的 SHA-256 均为
`a89351772ff0cd6bb1d90a9d13963f11f1e865f6b45602a6c92d324a92179bec`。
实测结果如下，证明问题在当前样式中仍存在：

| 主题 | 链接色 | 相邻文字色 | 文字间对比度 | 默认下划线 |
| --- | --- | --- | --- | --- |
| 浅色 | `#17644e` | `#20342e` | 1.87:1 | 无 |
| 深色 | `#8ad5b6` | `#edf4f0` | 1.53:1 | 无 |

两者字重均为 600。依据 [axe 的 link-in-text-block 规则](https://dequeuniversity.com/rules/axe/4.10/link-in-text-block)，
文本块中的链接缺少独立视觉标记且与相邻文字的对比度低于 3:1 时构成违规。
这里比较的是链接与相邻文字，不是链接与背景。

## 开发方案与验收标准

保持单个局部修复批次，为共用 `GoalItem` 的标题链接添加现有 Tailwind `underline`
类，使首页、目标详情和进度列表复用的标题均具有常驻下划线。
增强现有 `learning.spec.ts` 的真实数据场景：先等待进度标题出现，移开鼠标后检查
非聚焦状态的下划线，再执行原有 axe 扫描。

验收标准：

1. 修复前的最小案例可确认缺失视觉标记；修复后默认状态的标题有下划线。
2. 浅色、深色各 390、768、1280、1440px 下，标题保持可辨识且原 axe 扫描无违规。
3. 原 `learning.spec.ts` 的配对、目标保存、生命周期、草稿隔离及恢复场景通过。
4. Web 单测、类型检查与生产构建分别记录真实结果；无关基线错误单独列明。
5. 不降低 axe 检查范围，不修改路由语义、主题配色或全站链接样式。

## 验证记录

- Chromium 1234 最小案例：已确认修复前的默认文字装饰和双主题对比度，见上表。
- `git diff --check`：通过。
- 当前工作树没有 `clients/web/node_modules`。按照用户提供的全局工作规范，安装依赖
  需要明确授权；已申请按现有锁文件执行 `npm ci`，尚未收到答复。
- Web 单测、类型检查、Vite 构建、`web_release` 构建及原 `learning.spec.ts`：未运行。
  不能将修复前的已有资产用于声明当前修复已通过，新增的 Tailwind 类须重新构建生成。
- 仓库历史验收记录还列有独立的类型检查问题 #43；本轮尚未执行类型检查，
  不将历史失败或未经运行的命令记作当前候选的结果。

当前仅完成问题确认和局部修复，浏览器验收仍待依赖安装授权后补齐。
