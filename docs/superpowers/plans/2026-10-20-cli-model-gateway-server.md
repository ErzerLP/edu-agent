# CLI 模型网关服务端实施计划

**目标：** 实现 Build #122 G-A 服务端，接口遵循 [设计 §3](../../design/cli-model-gateway.md)。

**架构：** settings 冻结按 target 选择的连接；llm 转换无状态文本/function 协议；modelgateway 编排身份、去重、租约、流输出和真实上游计量。PostgreSQL 门禁负责跨实例一致性，通知仅用于及时取消资源。

**技术栈：** Go、pgx/PostgreSQL、React/TypeScript、nginx。

**约束：** 不改 clients/cli-go 或 packages/agentcore；不提交；不做兼容；G-B 工具限制及预算清理不在本次范围。默认并发 10，范围 1–16，CLI 容量 C−1，每设备 1；正文只在有界内存中出现。

- [x] 身份与连接：新增 cli profile/model:invoke；settings 按 target 冻结连接和不透明版本；测试权限投影、连接隔离及版本变化。
- [x] 上游转换：在 integrations/llm 新增网关请求验证、Chat/Responses 流和非流式适配；用 httptest 验证文字、多工具、推理、usage、截断和有限回退，先运行失败测试再实现。
- [x] 共享准入：新增 modeladmission 端口，提取 claim 的 210021 锁和计数；000049 建立元数据表及通知；真实数据库验证总容量、网页预留和设备串行。
- [x] 网关生命周期：新增 modelgateway 和 httpapi 接口；同事务准入/去重/BeginTx，外发前 Dispatch，真实 transport Observe，结束 Finish，恢复 RecoverTx；验证重复、取消、撤销、清除、断线和正文不落库。
- [x] 现有出口：同步 modelusage transport 申请短租约；父 mentor run 保持现有租约；隐私清除加入新表，数据库通知取消所有实例活跃流。
- [x] 用量与网页：Query 增加来源/设备/会话筛选及 cursor 绑定；Task 增加设备/会话/轮次；用量页增加筛选和归属显示。
- [x] 合同与部署：OpenAPI、nginx 精确路由和流设置；设计文档记录 G-A 服务端覆盖及接口差异。
- [x] 验证：umask 022；真实 TEST_DATABASE_URL 下受影响 Go 包 go test -p=1 / go vet -p=1，mentorrun -timeout 40m；前端 npm run check。仅执行相关检查，不运行全量或三浏览器矩阵。

本会话直接执行；沿用用户给定 worktree 和已批准设计，不额外建立工作树或审批门禁。
