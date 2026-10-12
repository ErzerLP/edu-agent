# Studio 测验与正式作答实施计划

**Goal:** 完成 G5，生成、保存和重开测验，逐题进入既有正式作答及进度。

**Architecture:** 按已确认方案 A，每题一个专用单题会话。learningcontent 拥有加密题集与映射，learningstart 编排 knowledge 的冻结参考及 learning/tutoring 的正式 proposal/事务。复用 TeachingPage 的帮助、Attempt、评估和未知操作核对，不新增评分或教学状态。

**Tech Stack:** Go、PostgreSQL、OpenAPI、React/TypeScript、Vitest、Chromium。

**Spec:** [工作区设计](notebook-workspace.md)，`docs/comet/specs/notebook-workspace/spec.md`。

## 范围与约束

- 只做 3c，不提交、不推送、不改 CLI/agentcore、不操作 Docker。
- quiz 要求明确目标、冻结 D1 来源；生成沿 Studio Responses/RequireCompletion，严格验证 prompt/rubric/allowed_help/citations。
- POST quiz/start 显式携带题号、产物版本、目标版本、隐私代次和操作身份；服务端检查前题真实完成后，原子创建专用会话/活动/正文和映射。同版本同题只有一个承载。
- GET quiz 返回关联及真实会话状态，不存第二份进度。旧版本、原课堂与草稿独立。
- 追加 migration，映射随 learning owner 清除；正文复用原密钥轮换。

## 执行与验证

- [x] 生成合同：先加 `TestStudioQuizCandidate` 验证非法 rubric/help/citations/缺目标拒绝，再新增 `learningcontent/studio_quiz.go` 并扩展 Studio schema/解码/读取。
- [x] 正式承载：先加真实 DB 测试验证同题幂等、前题顺序、旧课堂保持、正式评估路径，再新增映射 migration、owner 事务端口、`learningstart/studio_quiz.go`、HTTP 注入。
- [x] Web：扩展 OpenAPI 并生成类型；新增 `studio-quiz.tsx`，复用 TeachingPage。测试生成无目标提示、帮助限制、丢响应核对及原课堂草稿。
- [x] 验证：指定 Go 包/vet，串行 PostgreSQL，前端 check/build/相关 vitest，Chromium Studio/workspace/sources/review（31 项），真实 Responses 测验 smoke（8192、实际 2 次、不记录凭据）。通过检查不重复。
- [x] 文档：记录 3c 合同、映射、错误、清除/轮换、文件及检查证据；仅实际通过后勾选设计步骤。全引擎、全量、race、Compose 与候选门禁交批次 4。

在当前会话逐项实施；现有用户授权覆盖普通编辑与局部验证，无需再次设计审批。若必须新增评分或状态语义，停止并报告。
