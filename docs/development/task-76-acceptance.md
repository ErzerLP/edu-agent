# Task 76：AI 配置与服务端、客户端测试

## 范围与验收方案

基线提交：`364fbe1`，分支：`task/76`。本任务只配置、测试和登记缺陷，不实施缺陷修复。

项目由 Go 服务端、PostgreSQL、Go CLI/TUI、React Web 客户端及共享 `agentcore` 模块组成。
教学模型、Web 导师及 CLI 本地助手独立配置。Nocturne、NoteSync 和 Brave 搜索为外部集成。

1. 使用独立测试环境，设置用户指定的兼容 API；通过模型列表核对实际模型标识为 `gpt-5.6-terra`。
2. 测试 Key 只写入忽略的本地环境配置、受保护的服务端设置或系统钥匙串，不写入提交、报告和 issue。
3. 运行三个 Go 模块的普通测试、race、vet、构建；真实数据库采用仓库串行候选分片。
4. Web 验证生成类型、类型检查、单测、发行构建和 Chromium、Firefox、WebKit 浏览器测试。
5. 对教学、Web 导师、CLI 助手执行真实提供商验证，限制输出和调用次数，记录实际结果。
6. 对失败保留命令、前置条件、触发步骤和可观察现象；核对现有 issue 后登记，不推断根因或设计修复方案。

验收标准：每项有明确的通过、失败或未覆盖结论；测试环境与用户已有数据隔离；
真实提供商调用与 fixture 证据分开；发现的可复现缺陷有 GitHub issue 链接。
未配置的外部集成和非 Linux 平台不能算作已通过。

## 实际结果

环境：Linux amd64、Go 1.26.6、Node 24.20.0；固定 PostgreSQL 17 Alpine 镜像。
已完成当前可用环境中的配置、测试与缺陷登记，共创建 #54–#67（14 个 issue）。
这不是缺陷修复或发布认证：真实教学与部分候选测试仍失败，未覆盖项见文末。

| 检查 | 结果 |
| --- | --- |
| 提供商模型列表与真实 Chat Completions | 通过，实际模型为 `gpt-5.6-terra`；最小请求使用 18 tokens |
| 生产 CLI `model test` 与真实 Agent TUI | 通过；自定义兼容端点，Key 使用系统钥匙串；TUI 完整显示一轮流式回答并正常退出 |
| 真实 SSE 工具调用 | 通过；接收完整 `report_test(ok=true)` 和结束标记；该次请求使用 83 tokens |
| 服务端教学/导师正式连接探测 | 均通过；结构化 JSON 和原生 JSON Schema 能力为 true |
| 服务端真实导师运行 | `succeeded`，1 次模型请求，正确解释偶数和奇数 |
| 生产 Web + 真实模型 | 配对、设置回读、Key 隐藏、教学连接探测、导师流式问答、刷新恢复、390px 视口及无页面异常检查通过；回答为“偶数是能被 2 整除、除后没有余数的整数。” |
| 真实教学路线及继续学习 | 路线生成与采用通过；讲解/练习请求返回 422，生产 CLI 复现，见 #57 |
| `make test` | 三个 Go 模块普通测试通过；无数据库环境的 skip 不算数据库证据 |
| `make test-race` | 三个 Go 模块 race 测试通过 |
| `make vet` | 三个 Go 模块静态检查通过 |
| Go 服务端、CLI、companion、agentcore 构建 | 通过；授权补测后服务端 Web 发行构建、Docker 镜像构建也通过 |
| fake LLM、response proxy、operations 模块测试 | 通过 |
| 发布结果解析器测试 | 通过 |
| 数据库 `db-core` | 首轮 149 项（含子例）通过，0 失败，1 项真实 NoteSync 条件测试跳过；该项随后在真实 NoteSync 环境定向补测通过。首轮候选命令仍记录失败，不改写其结果 |
| 数据库六个分片 | `learning-core`、`learning-offline`、`learning-fault`、`memory`、`privacy-core`、`privacy-fault` 全部通过 |
| 生产模型对照黑盒 | baseline/candidate 均启动失败；baseline 定向复现，见 #55 |
| 离线准备黑盒 | 初次与定向复现均失败，见 #56 |
| 其余 CLI 黑盒 | 5 项（含子例）通过，10 项失败，无 skip；教学相关失败统一见 #56 |
| 真实 NoteSync 固定版本候选 | 服务 3.6.1、适配器合同、数据库接受远端并回写流程通过 |
| 学习区/导师/HTTP 数据库集成 | 409 项（含子例）通过，导师停止场景 1 个子例失败（并导致其父测试失败），0 skip；相同代码下单独复现该子例通过，见 #58 |
| Web 生成、类型、单测、发行资产 | `make web-release-check` 通过；21 个单测文件、52 项测试通过；缺资产构建按预期失败 |
| 三浏览器完整矩阵 | 39 个文件、94 项用例：78 通过、16 失败，0 skip、0 retry；详见下表及 #59–#66 |
| 浏览器干净库定向复测 | 8 项，5 通过、3 失败；保留原矩阵失败，不以复测结果覆盖 |
| Nocturne Python 契约 | 按哈希锁文件安装后 38 项通过；4 条依赖弃用警告，无失败 |
| Nocturne 真实 Compose 完整场景 | 健康/鉴权、交付、真实 CRUD 清除、过期核对、账号隔离、故障恢复、dead 交付重放与失败升级回滚已完成；备份清除核对超时失败，见 #67。该完整场景未到达后续保留期清理和最终 SIGTERM 检查 |
| Nocturne 独立 `backup` 场景 | 新建隔离环境定向通过：真实失败升级回滚、隐私清除后密钥销毁、旧备份恢复拒绝与保留期清理；不覆盖完整场景前面的交付/过期/重放序列，不替代其失败 |

浏览器矩阵使用原用例、默认超时及单 worker，不含真实模型调用：

| 引擎 | 文件 | 通过 | 失败 | 跳过 / 重试 |
| --- | --- | --- | --- | --- |
| Chromium | 13 | 30 | 4 | 0 / 0 |
| Firefox | 13 | 25 | 5 | 0 / 0 |
| WebKit | 13 | 23 | 7 | 0 / 0 |

定向复测均使用新建数据库：Chromium 调整模式仍失败，继承通过，Studio 在选到目标后因收藏筛选为空失败；
Firefox 隐私清除与评估详情通过；WebKit 知识结构通过、导师页面网络异常仍失败、完整选段/补偿/Studio 场景通过。
这只能说明具体断言在该次运行中的结果，不能把状态相关或偶发失败改写为已修复。

## 已登记缺陷

- [#54：数据库与 NoteSync 候选命令因脚本执行权限失败](https://github.com/ErzerLP/edu-agent/issues/54)。
  按文档运行 `make postgres-candidate` 或 `make notesync-candidate`，脚本启动前报
  `Permission denied`。本任务未修改脚本权限，通过显式 Bash 调用继续数据库验证。
- [#55：生产模型对照黑盒的 baseline/candidate 服务均无法就绪](https://github.com/ErzerLP/edu-agent/issues/55)。
  等待约 30 秒后报 readiness 失败；服务日志为“配置格式或范围无效”。
- [#56：CLI 教学与离线黑盒停留在目标选择并以 internal_error 退出](https://github.com/ErzerLP/edu-agent/issues/56)。
  预期教学阶段没有开始，离线用例也未进入 prepare/learn/sync。
- [#57：真实模型探测通过但继续教学被拒绝](https://github.com/ErzerLP/edu-agent/issues/57)。
  `gpt-5.6-terra` 能完成普通问答、导师交流、路线生成，但继续教学返回 `proposal_rejected`。
  1024 与默认 2048 输出预算下均观察到；生产 CLI 也失败，退出码为 4。
- [#58：导师停止状态断言偶发失败](https://github.com/ErzerLP/edu-agent/issues/58)。
  完整数据库集成中报“未显示取消中”，随后定向重跑通过；只确认测试结果不稳定，未断言取消功能失效。
- [#59：NoteSync 浏览器任务中心可见性断言失败](https://github.com/ErzerLP/edu-agent/issues/59)。
  “已解决”定位器实际匹配到隐藏的下拉选项，验收中断；没有据此认定同步解决操作失败。
- [#60：三浏览器调整模式定位失败](https://github.com/ErzerLP/edu-agent/issues/60)。
  无法通过 `getByLabel` 断言；现场可访问性树含该下拉框，干净 Chromium 数据库仍复现。
- [#61：完整候选的共享数据库状态影响场景准备与定位](https://github.com/ErzerLP/edu-agent/issues/61)。
  Chromium 继承准备读到 `content_redacted`；WebKit 知识维护存在同名历史记录且批准按钮禁用；
  WebKit NoteSync 导入要求身份审阅。继承和知识结构在干净库定向通过，未断言三处根因相同。
- [#62：Studio 关联目标与收藏筛选异常](https://github.com/ErzerLP/edu-agent/issues/62)。
  完整矩阵的 Chromium/Firefox 目标下拉框缺项并提示响应格式不支持；干净 Chromium 库可选目标，
  但收藏筛选为空。另一次干净 WebKit 单例通过，记录两个触发点而非推断同一根因。
- [#63：后续隐私清除返回 409](https://github.com/ErzerLP/edu-agent/issues/63)。
  完整矩阵 Firefox/WebKit 失败，干净 Firefox 单例通过；与 #47 的重新配对失败位置不同。
- [#64：Firefox 教学反馈等待不稳定](https://github.com/ErzerLP/edu-agent/issues/64)。
  矩阵中未显示“教学反馈”，干净库定向通过，不认定反馈永久丢失。
- [#65：WebKit 断网恢复后页面网络异常](https://github.com/ErzerLP/edu-agent/issues/65)。
  导师与导入的功能断言已完成，最终页面异常列表含 `access control checks`；导师干净库仍复现。
- [#66：WebKit 补偿版本显示不稳定](https://github.com/ErzerLP/edu-agent/issues/66)。
  矩阵未显示第 3 版，干净库同一完整场景通过；不据此断言补偿版本未保存。
- [#67：Nocturne 完整验收无法验证备份销毁](https://github.com/ErzerLP/edu-agent/issues/67)。
  隐私清除后等待 90 秒，回执仍为 `partial`，托管备份为 `pending`，远端清除未收敛。
  独立 `backup` 场景随后通过；不据此改写 full 失败，也不凭超时断言密钥永久未销毁或数据泄漏。

## 本地配置与证据

本机服务配置位于仓库忽略的 `.env`。受保护设置、导师加密密钥、独立 CLI 配置和原始测试证据
保存在 `/tmp/edu-agent-task76.VXGb02/`；该目录为本机私有状态，不随提交交付。
API Key 没有写入报告、issue 或受跟踪文件。服务模型输出上限为 2048，CLI 为 1024。

宿主缺少 `psql`，黑盒使用本机已有固定 PostgreSQL 镜像中的客户端，通过本地包装命令传递原参数。
NoteSync 候选使用仓库外脚本副本，仅将其数据库子脚本调用改为显式 Bash 并固定仓库目录，
以继续测试；仓库脚本未修改，#54 仍未修复。

本地服务地址为 `http://127.0.0.1:18076`，独立数据库容器为 `edu-agent-task76-postgres`。
授权补测后已构建 Web 资产并开启 Web UI，入口为 `http://127.0.0.1:18076/app/`。
该实例的模型和 PostgreSQL 健康；未启用的 Nocturne、NoteSync 与离线签发器使总体 `/readyz` 为 `degraded`。
真实外部集成测试使用另外的隔离环境，没有将测试配置套用到用户已有部署。
本地发行服务可以在仓库根目录加载 `.env` 后启动：

```sh
set -a
. ./.env
set +a
./server/edu-agentd serve
```

使用本任务 CLI 配置：

```sh
XDG_CONFIG_HOME=/tmp/edu-agent-task76.VXGb02/cli ./clients/cli-go/bin/edu-agent
```

浏览器首次进入需要配对；在同样加载 `.env` 的终端执行以下命令，取得短时配对码后输入页面。
`settings` 档案允许检查模型设置，不能把配对码和测试 Key 写入公共记录。

```sh
./server/edu-agentd pairing-code create --profile settings
```

数据库候选脚本自动清理了其创建的临时数据库容器，临时数据不可恢复；
真实模型测试用的独立数据库和本地配置保留，供继续验收。
Nocturne Compose 脚本清理了它创建的临时容器、数据卷及导入标签，临时测试数据不可恢复，未清理既有部署。

## 授权后补测方式

用户在恢复任务时明确“完全授权”，已补装测试依赖：Web 使用 `npm ci --no-fund`，
Python 使用独立环境及仓库的 `--require-hashes` 锁文件，未升级或改写项目依赖锁。
宿主补装 Python venv 和 skopeo，仅用于建立契约环境与验证离线 OCI 镜像。

浏览器采用本机 Playwright 1.62.1 Noble 镜像，所有仓库 `*.spec.ts` 按文件、按引擎串行执行。
每个文件重新启动生产服务和相应 fixture；离线用例通过 `npm run test:offline` 启动。
数据库为本任务容器中的独立 `edu_agent_web`，不使用真实模型验证库。
Firefox/WebKit 沿用仓库配置，不执行标记为 `@chromium-offline` 的完整离线场景，仍执行能力降级场景。
外部执行器在文件失败后继续收集矩阵证据，未修改任何产品代码或测试断言。
因此本次不是 `make web-release-candidate` 整体通过；原候选入口会在首个失败处停止。
此前同一源码已经取得的 Go/数据库证据继续使用，没有为了改变统计而重跑全量。
完成时再次核对源码输入摘要，与 Web 局部发行机器报告一致：
`0808091b72f1b7781c74f77c7f2304dde85202955c2bc96cb40deeef95b0d5a7`。

Web 局部发行机器报告：`/tmp/codeg-acp/801144-2a899955/edu-web-release-inp5DN/report.json`。
其余补测日志与浏览器 JSON 位于私有证据目录的 `browsers/`、`nocturne-contracts.log`、
`docker-build.log`。这些本机路径不随提交交付；文档结果不能替代原始机器报告。
真实 Web 模型结果保存在 `live-browser-result.json`，截图未包含 Key。
Nocturne 使用已校验的离线 OCI，服务端镜像来自当前源码，执行仓库原始
`contracttests/nocturne/run-compose-e2e.sh <OCI目录>`；日志位于 `nocturne-e2e.log` 和 `nocturne-e2e-gate.log`。
独立备份复测使用相同命令追加 `backup`，通过日志位于 `nocturne-backup.log`。

## 尚未覆盖

- 没有提供真实 Brave 搜索配置，未验证真实搜索驱动的空资料开学；已有 Go/数据库中的 fixture 结果不能替代它。
- 没有 macOS/Windows 原生客户端运行证据；Nocturne 完整场景及已失败的客户端/教学路径不能声明通过。
- 当前有已登记缺陷，不能把本次测试完成理解为完整在线发布验收通过。
