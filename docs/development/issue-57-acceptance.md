# Issue #57：教学模型严格 schema 导致续学提案被拒绝

## 缺陷确认与架构

工作树基线为 `dc03439`，其前一提交为 issue 使用的 `364fbe1`；二者之间仅有验收文档变化。
Go 服务通过 app 组合知识、学习/教学、身份、记忆与隐私模块，PostgreSQL 保存正式状态；
CLI 和 Web 使用 HTTP 接口，MCP 复用应用服务。教学提案使用
`learning.Service → tutormodel.Adapter → llm.Client`，导师和客户端助手使用独立模型路径，
所以导师成功不能验证教学提案 schema。

只读检查原验收环境的 `tutoring_proposal_requests`：route 为 `ready/{success}`，
两次 activity 和一次 explanation 均为 `failed/incompatible/{incompatible}`。
保留的 HTTP 结果为 422 / `proposal_rejected`。未读取或输出模型 Key，也未改变原会话。

基线原因链：

- `server/internal/integrations/tutormodel/adapter.go:39` 对所有教学提案启用原生严格 schema。
- 同文件 `:68` 的资料引用、`:76–78` 的评分项与客观题规则、`:82` 的误解候选，
  在 `properties` 定义字段，却没有全部列入 `required`。
- `server/internal/integrations/llm/client.go:288–302` 的探测只有一个必填布尔字段；
  `adapter.go:74` 的路线也满足严格合同，因此都能通过。
- `client.go:334–344` 把上游普通 4xx 分类为 `incompatible`，
  `server/internal/learning/proposal.go:155` 将其返回为 `proposal_rejected`；
  `server/internal/transport/problem/problem.go:156–157` 映射为通用 HTTP 422。

已使用 OpenAI Docs 技能核对[官方结构化输出合同](https://developers.openai.com/api/docs/guides/structured-outputs#all-fields-must-be-required)：
严格 schema 的每个对象必须关闭额外属性，并把所有属性列入 `required`；可选值以包含 `null` 的类型联合表达。
这是可独立复现的请求合同缺陷，与历史失败类别一致；原始上游错误正文未保留，不能断言历史请求没有其他限制。

## 开发方案与验收标准（实施前）

单批次结果：严格兼容端点可以接受续学所需的教学 schema，原领域校验继续有效。

1. 先加强真实 HTTP 请求边界的回归测试：端点递归检查严格 schema，探测通过后再生成各类提案。
   修改前必须出现探测/路线成功、activity/explanation 等被拒绝的结果。
2. 只修正教学适配器中有缺陷的 schema：所有属性必填，原可选字段允许 `null`；
   不改教学状态、权限、数据库、模型预算，也不通过关闭 strict 或放松输出校验绕过问题。
3. 同步独立 fake LLM 合同与输出，覆盖开放题无客观题规则、客观题有规则、空可选引用和非空可选值。
4. 修复后原回归通过；错误类型、缺失必填字段、未知属性仍被拒绝；
   执行受影响包测试、vet、服务构建及一个可用的教学垂直场景。

真实付费提供商复测不作为本地确定性测试的替代；没有执行的检查必须单列。

## 验收结果

### 先失败后通过的回归

在 `server` 执行：

```sh
go test -count=1 ./internal/integrations/tutormodel -run '^TestAdapterSendsStrictRecursiveSchemasAndThreeRoles$' -v
```

生产代码修改前，各子例的能力探测均通过；planning/route 通过，activity、assessment、free_answer、explanation 失败。
本地严格端点明确拒绝 `$.activity.knowledge_references[].range`、
`$.assessment.items[].misconception_candidate`、`$.text.knowledge_references[].range` 未列入 `required`，
真实适配器返回 `tutor model failed: incompatible`。修复后同一测试的六类提案全部通过。

`TestAdapterValidatesNullableActivityFields` 的 14 个子例通过：覆盖空/非空可选值，
拒绝缺字段、错误类型、数组非法元素、未知嵌套属性和空必填文本。
`TestNullableActivityProposalPreservesDomainValidation` 的 3 个子例通过：
开放题接受空规则、客观题接受完整规则、客观题仍拒绝空规则；资料引用从权威解析器补齐。

### 夹具兼容性

正式服务当前默认发送 `max_tokens=2048`，但旧 fake LLM 的严格请求解码器未声明该字段。
首次定向 CLI 验收因此止于能力探测；将独立契约测试客户端设为生产默认预算后，
`TestRealTutorModelAdapterDecodesEveryProposalSchema` 的五个子例也全部以 `incompatible` 失败。

为完成本次正式链路验收，仅给 fake LLM 增加可选正整数 `max_tokens`，
不调整生产预算或超时。修复后五个子例通过；新增预算测试的六个子例通过，
零值、负数、小数、字符串仍被拒绝。

### 已通过检查

```sh
# server
go test -count=1 ./internal/integrations/tutormodel ./internal/integrations/llm ./internal/learning
go vet ./internal/integrations/tutormodel ./internal/integrations/llm ./internal/learning

# contracttests/fakellm
go test -count=1 ./...
go vet ./...

# contracttests/cli-m1
go vet ./blackbox
```

以上测试均通过，所选包没有数据库 skip；fake LLM 模块入口没有测试文件。
黑盒准备阶段已成功构建真实 Go 服务、生产 CLI、fake LLM、响应代理与投影夹具。

### 正式 CLI 与 PostgreSQL

新增 `TestBlackBoxStrictSchemaContinuation`，通过正式探测接口后使用生产
`learn --session` 生成路线、讲解、活动及评估，并检查会话完成和四类模型请求各成功一次。
运行方式：

```sh
# contracttests/cli-m1，使用独立测试数据库及可用的 psql
TEST_DATABASE_URL='<独立测试库连接>' \
  go test -p=1 -count=1 -v ./blackbox -run '^TestBlackBoxStrictSchemaContinuation$'
```

首次实际执行使用原测试容器中的随机独立 schema，止于上述 fake LLM 预算合同错误，
失败耗时 6.33 秒；测试夹具已自动清理该临时 schema，原 issue 会话及资料未改动。
修正夹具后宿主候选锁被其他任务占用，30 秒及随后 300 秒等待均未取得锁，
最终等待命令退出码为 75，未启动第二次数据库测试。没有绕过串行数据库规则。

首次交付时该完整场景仅完成编译与 vet，未计为通过。

用户接受后，落地前合入 `main`（`a083339`），没有文本冲突，也没有手动解冲突。
主分支已经包含 #56 的测试修复；新增回归同步改用按会话 ID 查询完成状态，
以适配主分支删除 `latestSession` 辅助函数的变化。
首次补测的目标文本没有英文夹具中的检索词，返回 `knowledge_not_found`（6.58 秒）；
将测试目标对齐 `Stable Concept Verification` 资料后，使用上述命令定向通过。

补测取得 `/tmp/edu-agent-operations-candidate.lock`，继续使用随机独立 schema；
结果为 **PASS，10.45 秒，0 skip**。生产 CLI 退出码为 0，出现讲解、题目和已接受评估，
原会话进入 `Completed`；route、explanation、activity、assessment 各成功调用一次。
测试夹具自动清理临时 schema，原验收数据保留。
合并后的 fake LLM 全模块测试及 vet、黑盒包 vet 也通过。

### 验证边界

未调用 issue 中的真实付费提供商，不宣称该端点的修复后在线验收已通过。
未运行全仓、完整 PostgreSQL/CLI 矩阵或 Web 发布构建；本次未修改这些模块的生产行为。

交付结论：schema 根因已通过先失败后成功的确定性回归验证，相关测试和构建通过；
落地前已补齐完整 CLI/PostgreSQL 定向场景的通过证据。
