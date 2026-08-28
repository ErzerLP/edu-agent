# 开发规范入口

本目录是项目开发流程的统一入口。开始需求设计、实现、测试、验收或工作流调整前，按以下顺序阅读：

1. `development-workflow.md`：需求拆分、垂直交付、工作流状态、WIP 和完成定义。
2. `testing-strategy.md`：从编辑反馈到候选验收的分层测试与证据复用。
3. 当前 Comet change 的 `brief.md` 和完整目标 `spec.md`：本次用户可见结果、范围和验收合同。
4. 当前 capability 的 `docs/design/` 文档：协议、数据结构、状态机和实施顺序。

发生冲突时按以下优先级处理：

1. 用户最新明确决定和 Comet Runtime continuation。
2. 当前 change 已确认的正式 brief/spec/children。
3. `development-workflow.md` 与 `testing-strategy.md`。
4. capability 设计参考、历史验收报告和实现注释。

设计文档不能扩大已确认需求。确需改变用户可见行为、范围或验收标准时，必须返回 Shape 更新正式产物并重新确认。

当前 `offline-sync` 使用分阶段垂直交付计划；详细顺序位于其工作树的 `docs/design/offline-sync-delivery-plan.md`。
