# Issue #66：补偿版本按钮的异步布局竞态

## 架构与问题确认

基线 `dc03439`，初始工作树干净；相关生产代码及浏览器用例与报告基线
`364fbe1` 一致。项目由 React/TypeScript Web、Go 服务、PostgreSQL、Go CLI
及共享 agentcore 组成。learningcontent 保存加密正文和不可变修订，
learning/tutoring 保存原活动与学习事实，mentorrun 保存选段运行；Web 通过
React Query 读取正文、目标和运行，设备阅读偏好与正式内容版本分别保存。

先检查原矩阵遗留的浏览器错误现场和测试数据库：当时页面停留在第 1 版，
没有错误提示；对应 artifact 只有第 1、2 版，未保存第 3 版。
这些证据本身不能判定服务端恢复命令失败，也不能定位原矩阵的具体时序。

在独立 PostgreSQL 17、Playwright 1.62.1 WebKit 和同基线生产资产下，
先重放原场景：恢复请求返回 201，第 3 版正常显示，随后在独立的收藏筛选
问题处失败。再仅控制旧版页面的 `content-edits` 响应在鼠标按下后到达，
不修改响应内容或服务端逻辑，复现了 issue 的五秒标题等待失败：

- 恢复按钮原坐标为 `y=509.609375`，回执到达后变为 `y=617.390625`，
  向下移动约 108px，按钮高度为 44px。
- 鼠标松开时已不在按钮上，补偿 POST 请求数为 0，仍停留在第 1 版。
- `clients/web/src/teaching-page.tsx` 原第 1215–1238 行把 ContentEditor
  放在 ContentTools 前；前者异步增高会移动后面的恢复按钮。
- `clients/web/src/components/content-editor.tsx:105` 异步读取选段回执，
  `:129` 将其写入状态，`:411` 起追加结果、输出和版本链接。
- `clients/web/src/components/content-tools.tsx:195` 依赖完整 click 事件调用
  `:113` 的恢复命令；没有 click 就不会提交或显示错误。

因此已确认当前代码存在能产生所报现象的布局竞态；未声称已从缺少网络跟踪
的原矩阵记录反推出唯一根因。收藏更新及 Studio 目标下拉框问题不属于本次修复。

## 开发方案与验收标准（实施前）

单一 Web 布局修复，无需拆分，不改数据库、协议或补偿事务。
把内容管理与版本操作移到异步选段面板之前，使后者的挂载及运行结果更新
不会移动已经可操作的按钮。在既有完整浏览器场景中控制真实回执交付时序，
于鼠标按下和松开之间展示选段结果，不添加重试或延长五秒断言超时。

验收要求：

1. 修复前回归失败，修复后同一迟到回执不移动恢复按钮，补偿只提交一次。
2. 页面显示正式第 3 版，保留固定阅读第 2 版及原教学活动。
3. 同一场景继续检查来源、Studio 和深浅主题的四种视口。
4. 运行前端类型检查、单测、生产构建及三引擎定向回归；依赖或独立缺陷
   限制导致未完成的检查分别记录，不将局部通过写成完整矩阵通过。

## 验证记录

2026-09-21，Linux amd64，Go 1.26.6。复用本机已有的 PostgreSQL 17 和
`mcr.microsoft.com/playwright:v1.62.1-noble` 镜像，没有访问真实模型。
每次浏览器场景使用任务专用容器中的独立新数据库，串行执行。
用户授权后执行 `npm ci --no-fund`，安装 271 个锁定依赖，锁文件未修改。

问题确认阶段的证据：

| 检查 | 结果 |
| --- | --- |
| 原场景、原生产资产、独立空库 | 补偿返回 201，显示第 3 版；之后因收藏筛选为空失败，未归因于 #66 |
| 鼠标按下期间交付选段回执 | 旧布局向下移动约 108px，恢复请求数为 0，五秒后第 3 版标题断言失败 |
| 仓库新增确定性回归对旧资产 | 同样在第 3 版标题断言失败；不依赖固定等待时间或重试 |
| WebKit DOM 顺序对照实验 | 仅在点击前将 ContentTools 移到 ContentEditor 之前，完整场景通过：按钮坐标不变、恢复 POST 仅一次、第 3 版、固定第 2 版、原活动、Studio、深浅主题及四种视口 |

DOM 对照实验验证布局变化与丢失点击的因果关系，不替代源码构建和修改后资产验收。
安装依赖前的 `npm run build` 曾因 `tsc: not found` 退出 127；该环境缺口已解决。

修改后生产资产的实际验收结果：

| 检查 | 结果 |
| --- | --- |
| `npm run build` | 通过，包含 TypeScript 检查、Vite 生产资产及复制到 Go 嵌入目录；保留既有的大包体积提示 |
| `GOPROXY=off go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过，嵌入本次修改后的生产资产，供以下浏览器测试使用 |
| `npm run check && npm test` | 通过，包含最终浏览器测试代码的类型检查；21 个文件、52 项单测全部通过 |
| WebKit 定向完整场景 | 1/1 通过，13.8 秒，独立空库 `issue66_after_webkit_ready` |
| Chromium 定向完整场景 | 1/1 通过，14.7 秒，独立空库 `issue66_after_chromium` |
| Firefox 定向完整场景 | 1/1 通过，15.8 秒，独立空库 `issue66_after_firefox` |
| `git diff --check` | 通过 |

三个浏览器均使用仓库原配置和本次增强后的 `workspace.spec.ts`，不再使用临时副本
或 DOM 修改；没有跳过、重试或增加原断言超时。每个场景都验证选段加工、答案保留、
来源、Markdown 导出、迟到选段回执下按钮坐标不变、恢复 POST 仅一次、第 3 版、
固定阅读第 2 版、原活动不变、Studio 筛选，以及两种主题 × 四种视口。
这是目标场景的三引擎回归，不是全仓或完整发布候选矩阵。

首次修改后 WebKit 运行在补偿之前的下载步骤超时，跟踪中没有导出请求。
固定版本偏好的回显会增加页面提示；补充“取消固定阅读版本”出现的断言后再导出，
该前置问题消失。新增回归同时等待“取消收藏”反馈后再固定版本，确保下一操作
使用已经回显的偏好。没有修改偏好更新实现，也不表示已修复独立的 #62。

复现使用当前用例的准备函数及完整目标场景，临时副本仅改为导入本机已有的
`playwright/test`、去掉目标场景未使用的 Axe 导入。场景和前后对照分开保留。
问题确认阶段的原生产资产来自基线 `364fbe1` 的既有构建，已核对修改前源码与其一致。
原始复现、日志及跟踪保存在本机 `/tmp/edu-agent-issue66.711mnq/`，
不随提交交付；测试数据库容器 `edu-agent-task89-postgres` 停止并保留供复核。

验收命令：

```sh
cd clients/web
npm ci --no-fund
npm run check
npm test
npm run build
cd ../../server
GOPROXY=off go build -tags web_release -o edu-agentd ./cmd/edu-agentd
cd ../clients/web
WEB_RELEASE_MATRIX=1 WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 \
  TEST_DATABASE_URL='专用空测试库 URL' \
  npm run test:browser -- workspace.spec.ts --project=webkit --grep '选段模型加工' --trace=on
```

Chromium 和 Firefox 各使用独立空库，将 `--project` 替换为对应引擎。
本次浏览器命令在上述既有 Playwright 镜像中执行，挂载当前工作树及证据目录，
使用 host 网络连接任务专用 PostgreSQL；未安装或升级浏览器。
首次交付仅提交 `task/89` 分支，交由用户审查。

## 验收后合并记录

用户验收并明确授权落地后，将 `main` 的 `e9a2cfb` 合入任务分支。
手动解决两处冲突：

- `teaching-page.tsx`：保留 #62 的服务端偏好回写、取消旧查询和缓存更新，
  同时把完整的 ContentTools 放到异步选段面板之前。
- `workspace.spec.ts`：保留 #62 对连续收藏/固定及迟到偏好查询的确定性回归，
  其完成状态断言覆盖原前置等待；同时保留 #66 的迟到选段回执、按钮位置和
  单次补偿请求回归。#64 的正文加载及反馈回归也完整保留。

合并后重新运行 `npm run build`（含类型检查）、生产 Go 嵌入构建和 `npm test`，
全部通过，单测仍为 21 个文件、52 项。使用独立空库 `issue66_merge_webkit`
执行合并后的 WebKit 完整目标场景，1/1 通过（12.8 秒），同时验证 #62 和 #66。
合并后证据位于原证据目录的 `merge-build.log`、`merge-webkit.log` 和
`merge-webkit/`。随后按用户指定方式在主工作树 squash 提交，保留任务分支及工作树。
