# Issue #69：模型能力原因列表的空数组协议

## 问题确认与架构

基线为 `169f8f7`，与报告提交一致，工作区初始干净。Go 服务在 app 中组合
身份、教学、知识、记忆和 HTTP/MCP，PostgreSQL 保存权威状态；Web 和独立
Go CLI 通过公开 API 访问服务，共享 agentcore 不负责该 HTTP 响应。
本问题位于服务端 LLM 适配器 → HTTP 能力接口 → CLI 严格解码的链路。

以下行号对应修复前基线：

- `server/internal/integrations/llm/client.go:289` 创建能力结果时未初始化
  `IncompatibilityReasons`，原生 schema 成功（`:307`）及 JSON 回退成功
  （`:319`）都直接返回，JSON 编码得到 `null`。
- 同文件 `:330` 的缓存复制以 nil 切片为 append 目标，即便初始化首次结果，
  空列表经过复制仍会退回 nil；`:246` 的缓存命中再次暴露该问题。
- `server/internal/transport/httpapi/api.go:753` 直接编码探测结果，
  而 `server/api/openapi.yaml:9019` 规定原因字段是必需的字符串数组。
- `clients/cli-go/internal/api/client.go:453` 把成功响应解码失败映射为
  `malformed_success_response`；`:650` 也明确拒绝 nil 原因列表。
  `clients/cli-go/internal/api/presence.go:40` 产生报告中的 `null_field` 诊断。
  因此 CLI 的协议校验符合现有合同，根因是服务端成功结果及缓存复制。

## 开发方案与验收标准（实施前）

一个局部批次修复现有协议：初始化探测结果的空原因列表，缓存复制继续保持
独立切片并保留空数组。无需修改 OpenAPI、CLI 校验、配对权限或数据库结构。

1. 先增加回归并在原生产代码上运行，验证原生 JSON Schema 和 JSON 回退的
   首次、缓存响应均错误返回 `null`。
2. 修复后上述响应均为 `[]`；失败探测仍保留实际原因，调用方修改失败结果
   不得污染缓存。
3. 使用确定性模型 HTTP 夹具、独立 PostgreSQL 和当前源码构建的实际服务端、
   CLI，以 user 档案配对，连续两次执行 `device status` 均退出 0，输出设备、
   可达性、权限及兼容模型状态。先在修复前确认退出 6 的协议错误。
4. 运行服务端 LLM、HTTP/OpenAPI 和 CLI API/命令的相关测试、vet 及构建；
   按检查实际范围记录结果，不把未运行的外部模型或其他数据库测试算作通过。

本任务只改变一个原因列表的不变量，无需拆分独立能力或扩大为发布验收。

## 验收结果

环境：Linux amd64、Go 1.26.6；黑盒使用独立临时 PostgreSQL 容器和随机隔离
schema，模型使用仓库的确定性 OpenAI 兼容 HTTP 夹具，不依赖真实模型凭据。

修复前保持生产代码不变，新增回归后运行：

```sh
cd server
go test -count=1 -run '^TestProbeReasonsJSONContract$' -v ./internal/integrations/llm
```

原生 schema 和 JSON 回退的首次、两次缓存查询共 6 个断言失败，均为
“得到 null，期望 []”；失败模型的原因和缓存隔离断言通过。

独立数据库上运行实际二进制黑盒：

```sh
cd contracttests/cli-m1
TEST_DATABASE_URL='<独立测试数据库连接>' \
  go test -p=1 -count=1 -v -run '^TestBlackBoxDeviceStatusWithCompatibleModel$' ./blackbox
```

修复前 user 档案配对成功，两次 `device status` 都退出 6，完整诊断一致：

```text
error[protocol_error]: 服务端响应不符合公开协议（malformed_success_response）
method=GET path=/v1/model/capabilities status=200 content_type=application/json
decode=null_field field=response.incompatibility_reasons
```

修复后相同单元回归的 9 个子场景全部通过；相同黑盒的两次查询均退出 0，
断言设备名称、设备 ID、可达性、readiness、`model:probe` 和
`Model: compatible` 输出通过。黑盒每次从当前源码构建服务端与 CLI。

其余检查均通过：

```sh
cd server
go test -count=1 ./internal/integrations/llm ./internal/transport/httpapi ./internal/settings ./api
go vet ./internal/integrations/llm ./internal/transport/httpapi ./internal/settings ./api
go build ./...

cd ../clients/cli-go
go test -count=1 ./internal/api ./internal/command
go vet ./internal/api ./internal/command
go build ./...

cd ../..
git diff --check
```

普通 package 测试没有设置数据库及额外集成二进制，相关数据库、生产教学
集成和人工终端测试按原条件跳过；数据库证据仅来自上面的专用黑盒。
未运行真实外部提供商、Web 发布构建、全仓测试或跨平台矩阵。本次不涉及
模型内容、数据库语义或平台行为变化，这些未运行项不作为通过证据。
