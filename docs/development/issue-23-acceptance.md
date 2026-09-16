# Issue #23 验收记录

## 先确认缺失，再实施

起点 `e0dd3bd`，工作树初始干净，已包含 #18、#21、#22。实施前读取 learning、tutoring、knowledge、identity、privacy、Web 与 CLI 边界，并完成[开发方案与验收标准](../design/learning-content-workspace.md)。

缺失由基线代码确认，不是缺配置或既有功能改名：

- `clients/web/src/main.tsx:69` 的路由树没有教学工作区或版本内容页面。
- `server/internal/transport/httpapi/web.go:236` 的 SPA 回退白名单不接受内容地址。
- `server/internal/learning/domain.go:179` 的 Activity 在 `:190` 保存 prompt，没有独立正文身份、版本或块定位；迁移截至 `000022_goal_research.sql`，没有内容表。
- `server/internal/learning/store.go:220` 已有正规 SessionWorkItem，CLI 和 tutoring 动作已能作答、反馈、自由问答与续学。缺的是内容 owner 和 Web 入口，不是需要重写教学状态机。

## 交付内容

新增 learningcontent owner 和追加迁移 23。正文 Artifact/Revision 绑定学习区、目标修订、原会话/活动、资料、生成模型、输入与语义指纹；正规旧活动确定性生成首版，引用和正文使用 AES-256-GCM 加密，版本只追加。权限、隐私代次、提交幂等与答案版本检查都在数据库事务内执行；清除纳入原 learning owner 回执，未新增旁路状态机。

新增协议协商、内容详情/指定版本/历史、授权提交、版本化正式答案与原操作查询接口，更新 OpenAPI 和部署白名单。旧 Activity 三种类型、字符串答案、活动身份、历史事件和 rubric 保持原合同。

新增中文教学工作区与内容页面，包含真实会话选择、阅读、来源、正式答案、帮助、必要反馈、自由问答/返回原焦点及续学。九种最小块可组合，未知展示使用文本回退；草稿、失败输出、未知交互和过期版本不能正式作答。聊天与答案使用独立控件，答案丢响应后查询原操作及原会话，不自动换 ID 补发。草稿只在本标签页内存按身份、学习区、会话和活动修订隔离。

单选仅适配原题独立行的显式选项；完整集合、答案值和标签对应关系必须与原题一致。任意旧题仍可用字符串文本答案，不从 rubric 猜选项。新增失败测试曾实际证明“标签和值分别出现在原题中”不足以防止交换 A/B 的含义，现已修正并保留回归。

## 已执行验证（2026-09-16）

| 检查 | 结果与覆盖 |
| --- | --- |
| `cd server && go test ./... && go vet ./... && go build ./...` | 通过；无数据库环境的 skip 不计作数据库验收 |
| `go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过，嵌入本次真实 Vite 资产；浏览器运行该发行二进制 |
| `cd clients/web && npm run generate` | 已用项目标准生成器从 OpenAPI 重新生成类型，不手工拼接新协议到旧 DTO |
| `npm run check && npm test` | 通过，5 个测试文件、11 项单测，包含草稿/未知交互和 Markdown/链接/公式安全边界 |
| `npm run build` | 通过；仍有 Zod PURE 注释与大于 500 KiB 的 bundle 提示，不将其描述为无警告构建 |
| `cd clients/cli-go && go test ./internal/api` | 通过，旧严格 DTO 解码兼容回归 |
| 内容领域与 HTTP/OpenAPI 单测 | 通过：稳定 ID、自由组合、原题/选项语义、草稿/失败/未知交互、加密绑定；协议/区头、严格 JSON、越权、体积限制、短链接查询及 Nginx 白名单 |
| 真实 PostgreSQL 内容生命周期 `-race` | 通过：确定性适配与重启、并发首版/CAS、权限实时撤回、跨区/跨会话拒绝、静态密文与不可变历史、幂等、正式答案与原操作、完整隐私屏障/清除/禁止复活 |
| 真实 PostgreSQL Web 身份定向 `-race` | 通过：浏览器继承已有 knowledge:read，不自动获得写入、审批或管理权限，旧身份不自动升级 |
| 工作区 Chromium 端到端 | 三项通过；服务、Cookie、资料导入/冻结、提案、教学事件和 PostgreSQL 均真实，模型使用本地 HTTP 夹具 |
| 原 Web 浏览器回归 `learning.spec.ts` | 五项通过；保存目标不自动开学、身份/区/草稿与已有入口保持兼容 |
| 原导师浏览器回归 `mentor.spec.ts` | 一项通过；结构化等待、断线/服务重启恢复、预算与两标签页清除 |

真实 PostgreSQL 五包最终整包复验全部通过：迁移 6.790 秒、learning 存储 170.902 秒、identity 存储 9.077 秒、privacy 存储 118.874 秒、HTTP 9.589 秒。首轮曾发现跨区错误未统一映射与旧 Web 权限断言需更新，修复后重跑五包，不将首轮失败计作通过。

### 浏览器验收细节

工作区场景通过旧公开 API 建立正规会话，并在发行活动后才打开 Web，没有把静态样例渲染当成真实教学：

1. 选择旧会话，读取原题与冻结资料，来源弹层关闭后恢复焦点；进入内容版本页并返回，主题切换保留未提交答案。IME 组合态不发答案，讨论和帮助不误交答案，帮助等级保留，未发送聊天不被帮助按钮清空。
2. 在服务器已提交答案后中断响应，确认只发一次 POST，随后查询原 operation/session，恢复必要反馈，再完成确认并生成下一活动。刷新后内容仍是同一正式版本，原 Activity/rubric 不变。
3. draft 版本不替换当前正式版，不完整 committed 被拒绝；指定草稿页可读且不执行 HTML/外链图片/公式资源。未知交互前后端都禁止提交，支持的单选提交仍发送原字符串答案。
4. 两标签页独立草稿；答案已提交但响应延迟时，切到另一个真实学习区与会话，新草稿不被迟到响应覆盖。此测试实际发现并修复了固定教学栏遮挡切区菜单的问题，未使用强制点击绕过。
5. 390、768、1280、1440 宽度，深浅主题，共八组布局/可访问性检查及截图；无整页横向溢出，长代码局部滚动，辅栏尺寸可用方向键调整。截图保存在忽略的 `clients/web/test-results/`，不提交临时产物。

### 可重复命令

使用独立数据库设置 `TEST_DATABASE_URL`，不得指向生产库。数据库用例创建随机 schema 并自行清理；浏览器使用该独立库的正式迁移。

```bash
cd server
go test -p=1 -count=1 ./migrations ./internal/learning/postgresstore \
  ./internal/identity/postgresstore ./internal/privacy/postgresstore ./internal/transport/httpapi
go test -race -count=1 ./internal/learning/postgresstore \
  -run '^TestPostgreSQLLearningContentVersionsAnswersAndErasure$'
go test -race -count=1 ./internal/identity/postgresstore -run TestPostgreSQLWeb
```

构建前端和发行二进制后，串行运行浏览器场景：

```bash
cd clients/web
WEB_WORKSPACE_FIXTURE=1 npm run test:browser -- workspace.spec.ts
npm run test:browser -- learning.spec.ts --output=test-results/learning-regression
WEB_MENTOR_FIXTURE=1 npm run test:browser -- mentor.spec.ts --output=test-results/mentor-regression
```

本次宿主 Chromium 缺少 `libasound.so.2`，未安装系统库；使用已有且与锁文件匹配的 `mcr.microsoft.com/playwright:v1.62.1-noble` 容器执行同样命令，挂载工作树并传入独立数据库地址。所有模型请求只到本地 HTTP 夹具，不产生真实提供商费用。

## 兼容、部署及审查注意

- 新正文必须配置独立的原始 32 字节 `MENTOR_KEY_FILE`；缺密钥时明确受限，不生成临时替代密钥或回退明文。复用原密钥加载、备份和清除边界，具体配置见 [Web README](../../clients/web/README.md#版本化教学工作区)。
- 普通新浏览器仅继承原配对档案已有的 `knowledge:read`。旧浏览器没有此权限时重新配对，不迁移或隐式扩权。
- 历史列表展示最近 100 个版本；指定版本接口仍可读取更旧版本。刷新不声称保存未提交答案，备份/WAL 不承诺即时物理擦除。
- 类型生成差异较大，是因为基线 #21/#22 的手工附加声明被标准生成结果替代。生成后发现 `research-page.tsx` 现有 UUID 推断过窄导致 TypeScript 构建失败，仅显式标为 `useRef<string>`，未改变研究行为。
- 未运行真实模型提供商 smoke、原生手机软键盘/输入法、Safari/Firefox、TLS/Nginx 实机部署和全仓 `-race`；不将 Chromium 合成 IME、容器浏览器或代理正则测试冒充这些验收。已实现安全区、动态视口及焦点处理，但仍建议移动设备人工走查。
- 按独立工作树交付约定仅提交当前任务分支，不推送、合并或变更基础分支；由用户审阅后落地。
