# Notebook Studio 3b 实施计划

**目标：** 当前所选来源可生成、取消、恢复和重新生成有核验引用的指南／简报。

**依据：** [Notebook 设计](notebook-workspace.md) 的 G3/G4、D1/D4、批次 3；[正式合同](../comet/specs/notebook-workspace/spec.md)。本任务已授权实施，串行执行，不暂存／提交。

**架构：** 新增 `studio` 受限 mentorrun kind，复用加密 checkpoint、预算 transport、租约和 command。服务端 settings 提供 llm 结构化客户端，Responses strict 或 Chat json_object 均做同样的服务端严格校验。learningcontent 在运行结算事务内验证来源并追加不可变 revision。

**约束：** 不修改 agentcore/CLI；不交付测验；不改变课堂冻结参考；零来源拒绝生成；goal 可空；只发送已选择的核验片段；不自动重试外部模型；真实请求不超过 10 次，输出预算 8192，凭据不落盘／日志。

- [x] 领域与模型合同：`learningcontent/studio_generation.go` 定义分型正文和逐条引用；`mentorrun/studio.go` 定义生成请求／状态和 strict schema。先测试未选来源不能进入请求、未知字段／缺引用拒绝、guide/brief 不可互换，再实现。
- [x] 发布与恢复：新增 migration 000036 扩展 run kind；`StudioPublishTx(ctx, tx, actor, request, body)` 与 run 完成同事务；来源 reader 经组合根注入。真实 DB 测试覆盖成功／新版本、并发冲突、取消、重启 queued／unknown、partial、预算、撤权与清除、轮换。
- [x] HTTP 合同：Studio generation 创建，运行状态／取消复用 runs；artifact 历史和引用端点逐次核验来源；生成操作身份复用 mentor operation。更新 OpenAPI、客户端生成类型、路由分类和 nginx 白名单，拒绝缺字段／null／重复 JSON 键。
- [x] 前端：StudioPanel 接当前选择和可选目标；独立生成控制和分型结果查看器，真实运行列表恢复、取消、原 operation 核对、历史版本、引用定位。quiz 保持尚未交付。同步 Studio/citation 单测。
- [x] 验收：先局部失败测试再最小实现；稳定后各门禁通过一次即停。前端 check/build 和局部 vitest；指定 Go 包 test/vet；32995 PostgreSQL 串行；Chromium Studio/workspace/sources；Responses deepseek-v4-flash 各产物一次 smoke（8192）记录延迟／tokens。
- [x] 文档：设计新增 3b 实施记录，列合同、错误分类、文件映射、实际证据、未覆盖项和 3c 衔接；仅完成后勾选 3b。
