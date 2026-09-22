# Task 91：AI 配置与服务端、客户端验收

## 范围与验收方案

基线提交：`169f8f70898b077787b2abb3dcda86e0107a406f`。本次只配置隔离测试环境、执行测试和报告可复现缺陷，不修改业务实现，不合并或推送基础分支。

项目架构：Go 服务端通过 HTTP/MCP 提供身份配对、知识、教学、导师运行、记忆和隐私服务，PostgreSQL 保存权威状态；Go CLI 拥有独立模型配置与本地 Agent，React Web 使用同源 API；共享模型循环位于 `packages/agentcore`。Nocturne、NoteSync 和搜索为外部集成。

验收顺序与标准：

1. 在受保护的本地环境文件和系统钥匙串配置测试模型；核对提供商模型 ID，并执行真实连接检查。秘密不进入仓库、日志或 issue。
2. 执行三个主 Go 模块的完整测试及静态检查；另用独立 PostgreSQL 串行执行服务端持久化测试。
3. 按锁文件准备已授权的 Web 依赖，执行类型检查、单测、生产构建及独立数据库上的浏览器测试。
4. 验证服务端模型、Web 导师和 CLI 的真实调用，控制测试输出预算；确定性回归测试使用夹具，避免耗尽测试 key。
5. 对失败记录命令、触发步骤、预期与实际现象；排除依赖缺失和外部服务限制后，为可复现产品 bug 创建中文 GitHub issue，不分析修复方案。
6. 明确区分通过、失败、跳过和未执行；不把 Linux 自动化结果描述为所有平台及外部服务全部通过。

## 执行结果

### AI 配置与真实调用

- 提供商 `/v1/models` 返回 HTTP 200；用户描述的模型对应正式 ID `gpt-5.6-terra`，按实际 ID 配置。
- CLI 使用独立配置目录，Key 已写入系统钥匙串，`model test` 成功。
- 服务端教学模型的真实探测通过：普通 JSON 和原生 JSON Schema 均可用。
- 共享模型客户端的真实 SSE 调用成功，收到最终文本。
- 在 `http://127.0.0.1:32992` 启动独立服务，生产 CLI 使用 agent 档案配对成功；真实 TUI Agent 收到“测试成功”并正常退出，没有调用工具。`/livez` 返回 200。
- 本地验收将上下文预算设为 32768、CLI/导师输出预算设为 1024；这只是测试配置，不是对模型真实上下文能力的声明。
- 服务端设置为 Web 导师复用教学配置。仅保存和验证配置不表示已完成浏览器聊天验收。
- 配置、启动脚本和日志保存在本机私有目录 `/tmp/edu-task91.qwc7JY`；CLI 配置目录为其下 `client-config`。该临时目录不会提交，重启或系统清理后可能消失。

### 已完成的自动检查

| 检查 | 结果 |
| --- | --- |
| `make test` | 通过，共 70 个有测试的包；未提供数据库时的 skip 不计数据库证据 |
| 三个主 Go 模块各自 `go test -race ./...` | 通过：agentcore 4 包、CLI 25 包、server 41 包；不含数据库 race |
| 三个主 Go 模块各自 `go vet ./...` | 通过 |
| `contracttests/fakellm` 的测试与 vet | 通过 |
| `contracttests/operations` 的测试与 vet | 通过 |
| `contracttests/cli-m1` 的 responseproxy、cmd/response-loss-proxy 测试及模块 vet | 通过 |
| `node --test scripts/web-release-results.test.mjs` | 2 项通过 |
| CLI 与普通服务端二进制构建 | 通过；普通服务端构建不包含 Web 发布资产 |

### 真实 PostgreSQL 与跨端验收

独立容器 `edu-agent-task91-postgres` 绑定 `127.0.0.1:32991`，PostgreSQL 镜像摘要为 `sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`。未复用现有部署数据库。测试包用 `-p=1` 串行执行。

`server` 中配置 `TEST_DATABASE_URL` 后运行 `go test -p=1 -count=1 -json ./...`：1,817 个测试节点通过、1 个失败、5 个跳过（统计包含子测试，不把包终态重复计为测试）。

失败项 `TestPostgreSQLGoalListExcludesErasedGoalsBeforePagination` 在 `goals_test.go:52` 准备目标修订数据时触发 SQLSTATE 23514 / `learning_goal_previous_shape` 约束错误，分页断言尚未执行。全量结束后用单测试命令独占复现，现象一致。已创建 [GitHub issue #68](https://github.com/ErzerLP/edu-agent/issues/68)，说明环境、触发命令和实际表现；没有修改业务代码或提出修复方案。

随后设置生产 CLI 二进制路径 `EDU_AGENT_INTEGRATION_CLI`，定向补测原先跳过的 `TestPostgreSQLStudyCLIResearchAndWebContent` 与 `TestPostgreSQLStudyCLIAndWebShareChanges`，两项均通过。它们验证真实 CLI、HTTP Cookie/CSRF 与 PostgreSQL 共享状态，不能替代浏览器渲染测试。

CLI 全量黑盒首次因为宿主机缺少 `psql` 而在数据库初始化阶段失败，未认定为产品缺陷。随后通过本地启动包装器复用独立容器内现有 `psql`，不安装软件、不改变测试逻辑；重新运行 `go test -p=1 -count=1 -timeout=20m -json ./blackbox`，17 个顶层用例、21 个含子测试节点全部通过，无跳过。

启用 `RESEARCH_LIVE_SMOKE=1` 补测真实公开网页，成功取得 RFC 9110 正文及 14 个片段；覆盖状态为 `partial_text_limit`，不宣称抓取完整全文。原全量测试的五项跳过中，三项已补测通过，两项 NoteSync 上游测试仍未执行。

### 实际服务联调发现的产品缺陷

在独立普通用户 CLI 档案完成配对后，执行 `edu-agent device status` 连续两次得到退出码 6。接口 `GET /v1/model/capabilities` 返回 HTTP 200，但 CLI 报 `malformed_success_response`、`decode=null_field`、`field=response.incompatibility_reasons`。已创建 [GitHub issue #69](https://github.com/ErzerLP/edu-agent/issues/69)，记录同版本客户端/服务端、有效模型配置、user 档案权限要求、触发步骤及实际错误。

该现象与 Agent 档案不具备 `model:probe` 而返回 403 的预期权限行为不同。实际 Agent 对话成功也不能掩盖设备状态命令的协议失败。

初次执行时 Web 因缺少前端依赖而关闭。恢复任务后已构建生产资产并启用 Web，当前独立服务入口为 `http://127.0.0.1:32992/app/`，启动脚本为 `/tmp/edu-task91.qwc7JY/start-server.sh`。就绪接口仍可能因未配置的可选服务返回 `degraded`，不能仅凭 HTTP 200 认定完整部署健康。

### 完全授权后的 Web 续测

用户恢复任务并明确完全授权后，按锁文件安装 Web 依赖及 Playwright 浏览器，并安装 WebKit 所需系统库。未升级锁文件或修改业务代码；复用初次执行已通过的 Go、数据库及 CLI 证据。

- `npm ci --no-fund`：安装成功，审计报告 0 个漏洞。
- `npm run check`：通过。
- `npm test`：21 个文件、52 项测试通过。
- `npm run build`：通过；保留依赖注释和大包体积提示，不把提示当成构建失败。
- OpenAPI 重新生成到临时文件，与已提交 `src/api/schema.d.ts` 比较一致。
- `go build -tags web_release -o edu-agentd ./cmd/edu-agentd`：生产 Web 服务端构建通过。

浏览器版本：Playwright 1.62.1；Chromium 151.0.7922.34、Firefox 153.0、WebKit 26.5。对 `tests/browser` 的全部 13 个测试文件按浏览器、文件严格串行执行，共 39 组；每组通过仓库 `server/testutil/webdatabase` 创建并清理独立 schema。离线文件使用正式 `test:offline` 入口，其他文件按发布脚本配置导师、教学及 NoteSync 夹具。失败后继续收集其余文件的结果，没有修改测试断言或业务实现。

| 浏览器 | 通过用例 | 失败用例 | 收集失败文件 | 已选用例跳过 |
| --- | ---: | ---: | ---: | ---: |
| Chromium | 34 | 2 | 1 | 0 |
| Firefox | 31 | 1 | 1 | 0 |
| WebKit | 30 | 2 | 1 | 0 |
| 合计 | 95 | 5 | 3 | 0 |

收集失败不计入实际执行的用例数。Firefox/WebKit 按仓库配置排除四项仅适用于 Chromium 的离线用例，运行各自的能力降级验收；不是三引擎都验证完整持久离线流程。所有失败如下，不能将矩阵描述为全绿：

| 问题 | 复现与对照 | GitHub issue |
| --- | --- | --- |
| 知识结构用例收集阶段 `window is not defined` | 三引擎同样失败；独立 `playwright test knowledge-structure.spec.ts --list` 也失败 | [#71](https://github.com/ErzerLP/edu-agent/issues/71) |
| 开放评估准备课堂时 `/v1/tutoring/proposals` 返回 `proposal_rejected` | 三引擎同样失败；新 schema 单独执行该用例仍失败，尚未进入人工复核/覆盖页面断言 | [#72](https://github.com/ErzerLP/edu-agent/issues/72) |
| 内容版本用例等待“收藏内容”超时 | Chromium/WebKit 完整文件失败，现场停在“正在读取内容版本…”；Firefox 通过，Chromium 新 schema 单独运行也通过，按验收不稳定性记录 | [#73](https://github.com/ErzerLP/edu-agent/issues/73) |

### 真实模型 Web 与教学补验

继续使用受保护配置中的 `gpt-5.6-terra`，没有把真实 Key 注入确定性浏览器夹具。以下通过生产 Web、正式 API 和独立真实数据库执行：

1. 浏览器 settings 档案配对、读取教学模型配置、显式连接探测：通过，探测显示结构化 JSON 和原生 Schema 可用。公开设置响应未包含测试 Key。
2. 新建目标及导师对话、发送真实请求、接收 SSE 回答“测试成功”、刷新恢复保存的回答：通过。390px 页面无横向溢出，没有页面脚本异常。
3. 独立知识页面补验：提案响应丢失后核对原操作、审批、局部关系图、键盘打开详情、唯一概念及移动宽度均通过。此补验不加载失败的知识结构测试模块，也不代替原用例中尚未执行的全部断言；#71 仍成立。
4. 导入偶数/奇数资料，冻结范围并创建教学目标：通过。真实模型生成并采用 route、activity 成功，会话进入 `ActivityIssued`。
5. Web 打开真实生成的练习并开始活动：通过。提交正式文本答案：失败。两个新配对浏览器身份对同一课堂分别提交，均得到 `POST /v1/learning/content/<artifact_id>/answers` 的 HTTP 400 / `invalid_request`；页面保留答案并显示“请求未完成，输入已保留。请稍后重试。”，无法进入反馈阶段。已创建 [#74](https://github.com/ErzerLP/edu-agent/issues/74)，包含生成资料、目标、页面步骤、实际请求和响应。没有绕过失败继续写入教学状态；该真实课堂的模型反馈因此未能验收。

真实模型生成具有变化；#74 的证据是同一已生成课堂的两次独立浏览器提交，不宣称所有新生成练习都必然失败。初次浏览器配对后还观察到一次空资料开学能力读取返回 500，后续未复现，未据此建立稳定缺陷结论。

### 证据、交付与范围

本机证据目录仍为 `/tmp/edu-task91.qwc7JY`：`browser-matrix.json` 记录 39 组执行终态，各 `浏览器-文件.json` 保留 Playwright 结果；`repro-assessment.json`、`repro-content.json` 为独立复测；`live-web.log`、`live-knowledge.log`、`live-teaching.log`、`live-study-ui-repro.log` 记录真实业务补验。临时脚本、凭据、截图和日志未提交。测试 key 和导师加密密钥文件权限均为 0600，所在目录为 0700。

两项真实 NoteSync 上游测试缺少专用上游配置；Brave 搜索没有提供凭据。未运行外部 Nocturne/NoteSync 完整部署矩阵，也未进行 macOS、Windows 原生验收。

结论：已完成本次 Linux 环境下的服务端、CLI、Web 三浏览器矩阵及真实模型业务验收执行，结果并非全部通过。共创建 #68、#69、#71、#72、#73、#74 六个中文 issue，区分产品故障、测试阻断和不稳定场景，并给出触发步骤与现象。按任务要求未修复业务代码、未创建修复 PR，未合并或推送任何分支。
