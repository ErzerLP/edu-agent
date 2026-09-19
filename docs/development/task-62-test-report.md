# 任务 62：AI 配置与服务端、客户端测试

## 范围与验收标准

基于提交 `4913be9` 检查。项目由 Go 服务端及 PostgreSQL、Go CLI、React Web、共享 `packages/agentcore` 组成，Nocturne 和 NoteSync 为外部集成。

本次只配置测试模型、执行已有测试及真实模型验收、登记缺陷，不分析深层原因或修复代码。

1. 使用指定兼容端点的模型列表确认模型 ID，配置客户端和独立服务端验收实例，密钥不得进入仓库、日志或 issue。
2. 执行三个主要 Go 模块的测试、Race 和 vet；数据库使用独立本地容器与数据库，包之间串行。
3. 执行现有 Web 检查、浏览器测试和 CLI 黑盒入口；缺依赖、缺授权、跳过和失败分别记录，不视为通过。
4. 用公开样本验证真实模型连接、客户端流式交互及服务端导师链路。发现可复现问题后创建中文 GitHub issue，不修改实现或依赖文件。

## AI 配置

- 模型列表返回的正式 ID 为 `gpt-5.6-terra`。
- 当前用户的 CLI 模型配置已更新，模型 Key 存入系统钥匙串；配置文件不保存 Key。
- CLI 测试配置采用 128000 上下文窗口、2048 最大输出、90 秒超时、自动推理强度；服务端另用 16384 的上下文预算。这些是本次配置值，不代表上游模型容量的认证结果。
- 独立服务端使用本机 `127.0.0.1:32862`，数据库为专用容器 `edu-agent-task62-postgres` 内的独立验收库；现有 `8080` 部署保持原样。
- 服务端环境及设置放在仓库外的受保护目录，目录权限 `0700`，秘密文件权限 `0600`；Web 导师配置槽显式复用教学模型。真实模型验收通过正式 CLI/API 路径，浏览器矩阵使用确定性本地模型 fixture，不消耗测试 Key。
- 验收后已停止独立 HTTP 实例并释放宿主测试锁；保留本地配置、启动脚本和独立数据库供后续 Web 验收复用。

## 已完成的检查

| 检查 | 结果 |
| --- | --- |
| `make test` | 共享模块、服务端、CLI 全部通过；此次未设置数据库，因此数据库结果另列 |
| `make vet` | 三个主要 Go 模块全部通过 |
| `make cli-build` | 通过 |
| 服务端 `go build -o edu-agentd ./cmd/edu-agentd` | 通过；不含 Web 生产资产的构建，不能视为 Web 发行通过 |
| `make test-race` | 共享模块通过；服务端三项 PDF 用例超时，命令未继续 CLI |
| `make cli-test-race` | 单独补齐后全部通过 |
| 三项 PDF Race 精确串行复现 | 三项均复现 `pdf_timeout`，见 #41 |
| `node --test scripts/web-release-results.test.mjs` | 2 项通过 |
| `contracttests/operations` 的 `go test ./...` | 通过 |
| `contracttests/cli-m1` 的 responseproxy 与命令包测试 | 通过 |
| `contracttests/fakellm` 的 `go test ./...` | 依赖声明阻断，见 #42；未执行 tidy |
| CLI `model test` | 指定测试模型真实连接通过 |
| CLI `agent --no-save` 真实终端交互 | 公开二分查找样本得到逐字流式中文回答、恢复就绪、正常退出 |
| PostgreSQL 串行全模块测试 | 40 个有测试的包、838 个顶层测试通过，连同子用例 1790 项通过，零失败 |
| `RESEARCH_LIVE_SMOKE=1` 的 `TestLivePublicPageSmoke` | 补测通过；真实 RFC 9110 正文、指纹和 14 个来源片段完成核对，明确为长度限制下的部分覆盖 |
| 独立服务端教学模型探测 | 真实模型兼容，结构化 JSON、原生 JSON Schema 均通过 |
| 独立服务端导师三类公开样本 | 数学概念、代码算法、文章阅读各成功一轮；每轮 1 个模型请求，约 3.2 / 4.2 / 2.2 秒 |
| 生产 CLI 读取同一导师运行 | 三轮均与 API 输出一致；历史查询返回已保存的 3 轮 |
| `npm test` | Web 21 个测试文件、52 项单元测试全部通过 |
| `npm run check` / `make web-check` | TypeScript 在 `present_review` 参数处失败，见 #43 |
| 直接 Vite 生产资产构建 | 2102 个模块构建成功；大 chunk 仅为警告 |
| `go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 使用真实生产 Web 资产构建成功 |
| OpenAPI 生成一致性 | 生成成功但与已提交 `schema.d.ts` 漂移，见 #44 |
| `make web-release-check` | 结果判定契约 2 项通过，随后在 OpenAPI 漂移处按预期失败 |
| Playwright 三引擎浏览器矩阵 | 固定 `1.62.1` 镜像、每文件独立 PostgreSQL 复核；39 个“引擎 × 文件”组合中 20 个通过、19 个失败 |

数据库检查环境为 Linux `6.8.0-117-generic`、Go `go1.26.6`、PostgreSQL `17.11`；于 2026-09-19 10:19:57–10:36:25 UTC 执行，使用宿主候选锁避免其他数据库测试并发。命令为：

```bash
cd server
TEST_DATABASE_URL='postgres://postgres@127.0.0.1:32962/edu_task62?sslmode=disable' \
EDU_AGENT_INTEGRATION_CLI=/绝对仓库路径/clients/cli-go/bin/edu-agent \
go test -json -p=1 -count=1 -timeout=45m ./...
```

数据库轮次最初跳过两项 NoteSync 真实上游检查和一项真实公共网页检查；后者已单独补测通过。没有测试文件的包不计入通过数量。真实模型测试使用独立 `edu_task62_live` 库及独立 CLI 配对配置，不重配用户现有 CLI 的服务器身份。

设置配对档案对旧 `/v1/model/capabilities` 返回 403，是缺少 `model:probe` 的预期权限隔离；改用正式用户配对档案后探测成功，不登记为 bug。

## 已登记缺陷

- [#41：Race 模式下正常 PDF 夹具超时，服务端验收失败](https://github.com/ErzerLP/edu-agent/issues/41)。包含三项用例的精确复现命令、错误现象和负载限制说明；不据此断言非 Race 生产 PDF 必然失败。
- [#42：假模型模块已提交依赖无法直接测试，阻断 CLI 黑盒前置检查](https://github.com/ErzerLP/edu-agent/issues/42)。命令提示 `go: updates to go.mod needed`，阻断 `make cli-m1-blackbox` 的第一步。
- [#43：`present_review` 类型不匹配阻断前端构建与浏览器验收](https://github.com/ErzerLP/edu-agent/issues/43)。Web 单测通过，但 TypeScript 检查失败。
- [#44：OpenAPI 生成类型与已提交 `schema.d.ts` 漂移](https://github.com/ErzerLP/edu-agent/issues/44)。局部发布检查在一致性步骤失败。
- [#45：学习页活动标题触发 `link-in-text-block` 可访问性违规](https://github.com/ErzerLP/edu-agent/issues/45)。三个引擎均复现。
- [#46：NoteSync fixture Token 过短导致浏览器服务无法启动](https://github.com/ErzerLP/edu-agent/issues/46)。三个引擎均未进入测试。
- [#47：全局隐私清除后新配对无法恢复学习首页](https://github.com/ErzerLP/edu-agent/issues/47)。三个引擎独立数据库复现。
- [#48：settings 身份直达设置页时导师配置区域缺失](https://github.com/ErzerLP/edu-agent/issues/48)。导师历史场景在三个引擎超时。
- [#49：离线学习浏览器场景超时且创建按钮持续禁用](https://github.com/ErzerLP/edu-agent/issues/49)。另记录 WebKit 缺少测试直接使用的 Storage API。
- [#50：动态进度页缺少跨学习区“范围”选择器](https://github.com/ErzerLP/edu-agent/issues/50)。三个引擎独立数据库复现。
- [#51：旧会话来源对话框缺少冻结时引用原文](https://github.com/ErzerLP/edu-agent/issues/51)。Chromium 复现。
- [#52：WebKit 旧会话工作区出现横向溢出](https://github.com/ErzerLP/edu-agent/issues/52)。四视口断言失败。
- [#53：WebKit 清除导师正文后第二标签页仍显示旧输出](https://github.com/ErzerLP/edu-agent/issues/53)。Chromium、Firefox 的同场景通过。

浏览器矩阵最初在一个数据库中串行运行全部文件，隐私清除等全局状态造成额外连锁失败。对失败文件逐一创建空数据库重跑后，导师主流程在 Chromium/Firefox 通过，WebKit 的 PDF、来源、设置与任务中心也通过；上述 issue 只保留独立数据库仍能复现的结果。矩阵完整日志保存在本机 `/tmp/task62-browser-matrix` 和 `/tmp/task62-browser-isolated`，不提交测试正文或凭据。

## 环境限制

- 已按用户完全授权执行 `npm ci --no-fund --no-audit`，安装 271 个锁定依赖；`node_modules` 与构建产物由 `.gitignore` 排除。npm 报告 esbuild 安装脚本未列入 `allowScripts`，但直接 Vite 构建成功；未修改依赖或锁文件。
- 本机 Python 无 `pytest`，Nocturne Python 契约测试无法启动；未安装依赖。
- 真实 NoteSync 上游未配置，服务端相关测试跳过；浏览器 NoteSync fixture 另因 #46 无法启动。
- 用户只提供模型凭据，真实 Brave 搜索、空资料联网开学、第三方来源质量不具备完整在线验收条件。
- 本次运行环境是 Linux；未声称覆盖 macOS、Windows 原生钥匙串、原生输入法或移动设备。
- 数据库使用已有本地 `postgres:17` 镜像及自定义串行测试命令，未将结果冒充固定 digest 分片脚本的正式候选证据。

未修复 bug，未推送或合并分支，密钥和本地验收状态不纳入提交。
