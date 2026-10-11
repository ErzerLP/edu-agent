# Issue #82：服务端 Responses API 结构化输出验收

日期：2026-10-11（UTC）。工作树：`edu-agent-wt/responses-api`；分支：
`feat-82-responses-api`，基于 `67e37e3`。对应 [Issue #82](https://github.com/ErzerLP/edu-agent/issues/82)。
改动仅留在工作树，未暂存、提交或推送。

## 根因与设计

原服务端客户端固定请求 Chat Completions。该网关上的 `deepseek-v4-flash`
在此接口不支持教学适配器要求的原生严格 `json_schema`，而 Responses 支持。
新增 `llm.Options.WireAPI`，取值 `chat_completions`（缺省）或 `responses`。
两种接口分别使用 `openai-chat-completions-v1`、`openai-responses-v1` 能力档案，
仅在同一种接口内执行原有的原生 Schema → JSON 模式探测回退。

Responses 请求使用 `input`、`max_output_tokens` 和 `text.format`，读取
`output[].content[]` 中的 `output_text`。`ChatResult.Usage` 保存输入、输出、推理、
总 Token 数；输出数已包含推理数，不重复相加。协议参考
[OpenAI Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs?api-mode=responses)。
Chat Completions 请求和解析行为保持原样，其 usage 仍未解析。

真实活动请求另发现网关拒绝 `type: ["object", "null"]` 这样的复合类型数组。
Responses 发送前将客户端支持的递归 Schema 中的类型联合改写为等价 `anyOf`：
沿 `properties`、`items` 递归，每个分支保留原约束，仅替换该分支的单一 `type`。
该转换只作用于发送用的解析副本；原始 Schema 字节仍用于严格服务端校验。
未修改教学 Schema、提案校验、领域状态或数据库 migration。

| 响应情况 | 错误分类 | 行为 |
| --- | --- | --- |
| `incomplete` 且原因为 `max_output_tokens` | `output_budget_exceeded` | 不返回成功 JSON，不自动增大预算 |
| 其他非 `completed` 状态，含缺省状态 | `response_not_completed` | 拒绝未完成的结果 |
| 无 `output_text`、拒绝响应、无效 JSON | `invalid_response` | 不视为成功提案 |
| JSON 不符合原始 Schema | `schema_mismatch` | 沿用原校验 |

即使未完成响应中包含完整合法 JSON，也拒绝成功返回。新增分类由教学适配器继续传递，
错误不包含提供商正文、Key 或隐藏推理正文。

## 配置与兼容性

- 新环境变量 `MODEL_API_STYLE` 默认 `chat_completions`，只接受两种合法值；
  `buildModelClient` 和设置启动路径均传入所选接口。其余 MODEL 配置的成组校验保留。
- `Connection.api_style` 在旧文件、旧更新请求缺省时规范化为 Chat Completions。
  文件版本仍为 1；仅加载旧文件不会重写文件或更换凭据。
- 接口类型属于连接身份。切换时清除旧 Key、旧探测，以及显式复用教学连接的导师探测；
  新 Key 必须重新提供。保存不立即切换运行中教学，沿用重启生效语义。
- 保留原 Chat Completions 导师配置指纹，避免仅因新增缺省字段使既有运行失效。
  实际切换教学接口时，复用连接的身份仍发生变化。
- 设置页只在教学连接提供中文接口选择。Web 导师仍通过 agentcore 使用 Chat Completions，
  显式复用时只复用端点、模型和凭据，页面明确提示；导师的探测和公开有效接口也保持 Chat。
  独立导师及搜索槽位不接受 Responses。未修改 agentcore 或 CLI。
- 教学生成仍使用 `limits.output_tokens`，默认 2048 不变。Chat 设置页探测仍最多 64；
  新增的 Responses 探测使用已配置输出上限（含推理），避免固定 64 导致必然耗尽。
  确认框展示上限，每次最多两个请求，整体 15 秒超时不变。
- README 说明如何将输出上限调为 4096/8192、重新探测并重启；未新增输出预算环境变量，
  未改变默认预算。Base URL 的 `/v1` 拼接规则沿用旧客户端。

## 改动文件

| 文件 | 内容 |
| --- | --- |
| `server/internal/integrations/llm/client.go` | 接口选项、请求分支、usage、错误分类、探测 Profile |
| `server/internal/integrations/llm/responses.go` | Responses 解析及等价 Schema 转换 |
| `server/internal/integrations/llm/responses_test.go` | HTTP 请求契约、usage、失败状态、探测回退、联合类型及原始校验 |
| `server/internal/integrations/tutormodel/adapter.go` | 传递新增失败分类 |
| `server/internal/integrations/tutormodel/adapter_test.go` | 教学错误分类回归 |
| `server/internal/platform/config/config.go` | 环境变量默认值和校验 |
| `server/internal/platform/config/model_api_style_test.go` | 配置默认值及非法值回归 |
| `server/internal/app/app.go` | 两处客户端构造传参 |
| `server/internal/app/settings_test.go` | 环境接口选择到两处构造的 HTTP 回归 |
| `server/internal/settings/types.go` | 连接字段 |
| `server/internal/settings/service.go` | 默认兼容、身份及凭据绑定、探测预算、导师兼容 |
| `server/internal/settings/api_style_test.go` | 旧文件、凭据失效、探测、重启和旧指纹回归 |
| `server/api/openapi.yaml` | 设置字段和两种能力 Profile |
| `clients/web/src/api/schema.d.ts` | OpenAPI 生成类型 |
| `clients/web/src/api/settings.ts` | 运行时解析及中文错误文案 |
| `clients/web/src/api/settings.test.ts` | 新字段保留、旧响应默认值、非法值及错误文案 |
| `clients/web/src/settings-page.tsx` | 教学接口选择、凭据和探测预算提示 |
| `clients/web/README.md` | 接口配置、预算来源与调整步骤 |
| `deploy/env.example` | 环境变量示例 |
| `docs/development/issue-82-acceptance.md` | 本验收记录 |

## 检查命令与结果

新增行为先运行失败测试，再实现并验证通过。未运行全量 Go、race、浏览器矩阵或 Compose。

### Go

初次运行时默认 Go 缓存目录只读；指定构建缓存后，API/httpapi 又因模块缓存只读而未能构建。
已在 `/tmp` 创建可写模块缓存，已安装模块仅以只读链接复用，缺少模块按现有 go.mod/go.sum
版本下载；未修改依赖声明。另两个历史权限测试在沙箱 `umask 077` 下创建的 `0644`
文件实际变为 `0600`，改用测试进程 `umask 022` 后通过；未修改这些历史测试或安全校验。

在 `server` 目录执行：

```sh
GOCACHE=/tmp/edu-agent-82-go-cache go test -json \
  ./internal/integrations/llm/... ./internal/settings/... \
  ./internal/platform/config/... ./api/... ./internal/transport/httpapi/... \
  ./internal/integrations/tutormodel/... ./internal/app/...
```

该轮 llm、tutormodel、app 通过；其余包受上述缓存/umask 问题影响。
仅补跑未通过范围：

```sh
umask 022
export GOCACHE=/tmp/edu-agent-82-go-cache
export GOMODCACHE=/tmp/edu-agent-82-modcache
go test -json ./internal/settings/... ./internal/platform/config/... \
  ./api/... ./internal/transport/httpapi/...
go vet ./internal/integrations/llm/... ./internal/settings/... \
  ./internal/platform/config/... ./api/... ./internal/transport/httpapi/... \
  ./internal/integrations/tutormodel/... ./internal/app/...
```

以上全部通过。`TEST_DATABASE_URL` 未配置，httpapi 跳过 13 个 PostgreSQL 集成用例，
app 跳过 7 个测试/子测试；包括真实设置 HTTP 鉴权持久化用例，不能宣称数据库集成已验证。

真实 smoke 发现联合类型兼容问题后，只重新验证发生修改的两个包：

```sh
go test ./internal/integrations/llm/... ./internal/integrations/tutormodel/... && \
go vet ./internal/integrations/llm/... ./internal/integrations/tutormodel/...
```

通过。测试覆盖 null 和非 null 合法值，同时拒绝错误数组类型、错误元素类型，原严格校验仍有效。

### Web 与工作树

在 `clients/web` 执行 `npm run generate && npm run check && npm test -- --configLoader native`：
生成和 TypeScript 检查通过，Vitest **21 个文件、54 个测试通过**。
直接 `npm test` 因 node_modules 指向只读共享依赖、Vite 无法创建 `.vite-temp` 而启动失败；
`--configLoader native` 加载同一配置和全部测试，不写入共享依赖目录。
生成器报告既有 `ActionExposureRequest` discriminator 警告，该契约未在本任务中修改。
格式化仅涉及修改过的页面与设置测试。`git diff --check` 通过。

## 真实 smoke

模型：`deepseek-v4-flash`；端点主机：`api.yunyueoi.com`。实际调用 HTTPS `/v1/responses`，
上下文 32768，输出上限 8192，请求超时 60 秒。没有更改产品默认值。
仅从指定环境文件读取凭据；未将 Key 写入仓库、日志、文档或命令参数。

临时 Go 测试 `/tmp/issue82_smoke_test.go` 通过 `/tmp/issue82-overlay.json` 注入
tutormodel 测试包，未在仓库落地。调用生产 `llm.Client.Probe` 和
`tutormodel.Adapter.Generate`；活动使用真实 `ProposalActivity` Schema，输入仅包含
自编偶数知识和一个固定引用 ID。传输观察器仅记录延迟、状态、Token 计数，
不记录请求头、提案正文或推理正文。延迟包含网络请求和完整读取响应。

```sh
set -a
source /home/yunyue/.config/edu-agent-acceptance/model.env
set +a
GOCACHE=/tmp/edu-agent-82-go-cache GOMODCACHE=/tmp/edu-agent-82-modcache \
  go test -overlay=/tmp/issue82-overlay.json ./internal/integrations/tutormodel \
  -run '^TestIssue82RealResponsesSmoke$' -count=1 -v -timeout=130s
```

共 **5 次真实请求**（低于 10 次上限），完成后停止：

| 次序 | 内容 | HTTP / 结果 | 延迟 | input | output | reasoning¹ | total |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |
| 1 | 原生 Schema 能力探测 | 200 / completed，原生 Schema 可用 | 3709 ms | 106 | 586 | 577 | 692 |
| 2 | 原始活动 Schema | 400 / incompatible | 473 ms | 未提供 | 未提供 | 未提供 | 未提供 |
| 3 | 活动定向诊断重试 | 400 / 拒绝联合类型数组中的 object | 868 ms | 未提供 | 未提供 | 未提供 | 未提供 |
| 4 | 临时等价 anyOf 转换验证 | 200 / completed，原始 Schema 校验通过 | 3043 ms | 897 | 508 | 367 | 1405 |
| 5 | **最终生产代码活动生成** | **200 / completed，原始 Schema 校验通过** | **2517 ms** | **897** | **364** | **229** | **1261** |

¹ reasoning 已包含在 output 内。400 响应没有提供 usage，不记作零费用。

定向诊断和最终调用使用 `EDU_ACC_SKIP_PROBE=1` 跳过已经通过且请求未变化的探测。
第 4 次仅临时传输观察器启用转换用于定位问题；第 5 次关闭临时转换，由生产 Responses
客户端转换请求，返回 426 字节活动 JSON，含有效提示文本和所要求的引用。
最终命令为上述 go test 前置 `EDU_ACC_SKIP_PROBE=1`，并使用 `-timeout=75s`。

## 未覆盖项与风险

- 未运行 PostgreSQL HTTP/app 集成、浏览器交互矩阵、Compose、race 或全量 Go。
  页面保存链路由类型检查、运行时解析测试、设置服务和构造测试覆盖，未实点浏览器。
- 真实验收验证了能力探测及活动 Schema 生成，没有执行完整路线 → 活动 → 作答 → 评估流程，
  不代替 Issue #37/#74 的真实课堂闭环验收。
- 简单探测成功不能证明提供商支持所有教学 Schema。已适配本次确认的联合类型限制；
  其他网关或更多 Schema 约束仍可能返回 incompatible。
- 8192 是本次 smoke 显式预算，不能据此保证默认 2048 足够所有复杂教学请求；
  15 秒设置探测超时仍可能遇到慢推理模型，未放宽现有超时。
- Web 导师仍使用 Chat Completions，复用教学连接并不赋予导师 Responses 支持。
  凭据只在显式复用及重新提供凭据的既有授权下共享；接口切换不继承旧 Key。

## 建议提交

建议一个提交：`feat(model): 支持教学模型 Responses API 严格结构化输出`，包含上述全部
20 个文件。提交前由调用方审阅工作树；不包含 `/tmp` 测试、缓存、日志或任何密钥文件。
