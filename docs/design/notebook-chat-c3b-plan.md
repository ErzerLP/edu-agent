# C3b 对话练习实施计划

目标：在已交付 C3a 内核上完成 study 的出题、作答、自动评估、反馈及恢复。
设计依据：[Notebook workspace](notebook-workspace.md) C3 与已确认 Q2/Q4；样式依据 `notebook-chat-prototype.html`。

沿用 study 的持久运行和有界结构化生成。单轮返回至多一个已核验候选，与 learning 单题和对话引用一起提交。卡片读取原 content/session/feedback；所有提交继续经过 C3a 的帮助下限和评估任务登记。端点只公开界面需要的 DTO，不公开内部任务凭据。

约束：不改 CLI/agentcore；migration 只追加；不暂存或提交；真实模型 responses/deepseek-v4-flash/8192，最多 15 次，不记录凭据。

- [x] 服务端发布：先增加 study 候选的真实数据库测试；增加 turn 到 exercise 的唯一映射及收起状态；生成复用来源候选、两次上限和现有候选校验；保存确切方向、来源与评估策略；零来源请求返回选择来源入口。
- [x] 卡片接口：测试同对话待答限制、收起恢复与重放；增加受权限/来源保护的读取、帮助、收起/恢复与评估状态/续接；作答与反馈复用 learning 端口；提示先提交帮助记录再披露。
- [x] 公共合同：同批更新 `server/api/openapi.yaml`、生成 `clients/web/src/api/schema.d.ts`、HTTP 合同/路由分类、`deploy/web/nginx.conf`。
- [x] 前端：新增 `api/exercises.ts`、共享 `inline-exercise-card.tsx`/`feedback-card.tsx`；ChatPanel 插入卡片，原 operation 核对、帮助下限、短文案、引用导航、手机布局；相关 vitest 和 Chromium 业务断言。
- [x] 验证：指定局部 Go test/vet、真实 PostgreSQL 矩阵；npm check/build/相关 vitest；Chromium notebook-* 与 learner；真实模型出题→提交→反馈并记录耗时/token；通过一次即停止。
- [x] 文档：记录实际证据后更新 C3b/C3 状态与 H3/H4/H5/H7，写明 C4 共享组件与单题桥接用法、未覆盖范围和风险。

执行按上述依赖顺序在当前会话内进行。检查采用真实行为断言；数据库夹具只替换外部模型，保留生产事务、权限、状态机和投影。
