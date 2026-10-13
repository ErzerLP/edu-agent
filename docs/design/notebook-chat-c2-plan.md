# C2 Implementation Plan

**Goal:** 按已确认原型交付学习方向和默认保存的纯讲解对话中栏。

**Architecture:** learning direction owner 保存加密方向修订，并在学习写入事务中经 learning 命令维护唯一系统目标。显式 study conversation 保持空间级身份；每轮携带方向版本及 source_selection，历史仅在两个范围都一致时回放。前端复用身份、草稿、运行回执、引用定位与 Studio 笔记合同。

**Tech Stack:** Go、PostgreSQL、React、TanStack Query/Router、OpenAPI、Vitest、Playwright Chromium。

**Spec:** `docs/comet/specs/notebook-workspace/spec.md`、`docs/design/notebook-workspace.md` C2、`docs/design/notebook-chat-prototype.html`。

## Constraints

- 仅 C2；不改 CLI、agentcore 协议，不提交／暂存，不安装依赖。
- 追加 migration；旧 create/update/conversation 缺省兼容；保持权限、隐私、CSRF/Origin 和引用校验。
- 方向 1–2000 Unicode 字符，旧区区名预填并确认；不推断私人描述或旧目标。
- 纯讲解，不发布练习、Attempt 或掌握记录。
- 真实调用不超过 8 次；responses / deepseek-v4-flash / 输出预算 8192；密钥不进日志。

## 1. 方向与系统目标

- [x] 在 `server/internal/learningspace/postgresstore` 增加真实 DB 行为测试：旧请求、新建／编辑方向失败回滚与恢复、重放、版本冲突、改名及相同方向不修订。
- [x] 新增 `server/migrations/000038_study_direction.sql`；方向 revision/head/binding 属 learning owner，conversation 增 mode。
- [x] 新增 `server/internal/learningdirection`：ReadTx、SaveTx、EnsureTx、RotateKeyTx。EnsureTx 在空间锁下调用 learning.CreateGoal，binding 唯一；通用目标写入拒绝修改系统目标。
- [x] 扩展 Space/Command 可选 direction；空间写入回执只保存方向 revision 身份，正文通过 owner 解密；隐私清除、验证、轮换接入。
- [x] 用真实 DB 验证并发 ensure、方向修订、清除与轮换。

## 2. study 对话

- [x] 在 `server/internal/mentorrun/study_test.go` 验证显式 mode、方向版本、来源 A→B／方向变化输入隔离与保存重开。
- [x] NewConversation.mode 缺省 mentor；study 固定默认保存、无固定 goal/session；SubmitTurn.direction_version 与 source_selection 必填。
- [x] Create 在同一受理事务 ensure 系统目标，保存 direction revision/goal revision；历史匹配方向和来源。运行依受理快照，不追最新方向。
- [x] 使用现有 responses 结构化客户端与预算门禁，最多两次结构／引用尝试；保留服务端核验，不调用练习工具。
- [x] 更新 OpenAPI、合同测试、前端 DTO 并 `npm run generate`；使用既有路径则无需新增代理白名单。

## 3. 原型界面

- [x] 新建／设置弹窗增加方向，旧区一次确认；保留冲突时草稿。归档时方向只读，恢复二次确认可操作，恢复后可修改方向。
- [x] 主中栏替换为 ChatPanel：切换／新建、概要与真实来源片段、三条问题、开始／继续、消息、引用、保存笔记／复制、输入与快捷追问。
- [x] URL 持有 conversation 身份以支持刷新重开；草稿使用现有标签页 DraftStore。
- [x] 旧 session 深链接保留课堂；菜单可达旧目标页。删除主路径目标选择、开课流程和解释横幅。
- [x] 迁移 notebook-* 浏览器用例，保留来源、隐私、Studio 和 learner 业务断言。

## 4. 验收与记录

- [x] `npm run check`、`npm run build -- --configLoader native`、相关 Vitest。
- [x] `umask 022` 后，受影响包 Go test/vet；真实 PostgreSQL `-p=1`，仅有关矩阵。
- [x] 独立 schema、端口 32929、Chromium 确定性夹具，完整 C2 与 notebook/learner 回归，保存三张截图。
- [x] 加载外部 model.env，运行一次带核验引用的方向内真实模型 smoke，记录调用数及脱敏结果。
- [x] 设计文档 C2 下填写合同、migration、文件映射、迁移测试、检查与 C3 接口；最终 diff 检查，不暂存。

## 续做验收（2026-10-13）

保留上一轮实现并逐项审阅；本次补原子失败回滚的真实数据库测试，最小修复设置弹窗中归档方向
仍可编辑却被忽略、以及恢复确认按钮被原生 dialog 阻挡的问题，并补对应浏览器用例和实施记录。
全部验收均重新执行：前端生成／类型／构建通过，相关 Vitest 17 项；受影响 Go test／vet 通过；
真实 PG 原有定向 24 项及补充原子回滚 1 项通过；Chromium 主回归 17 项、来源回归 3 项通过；
设置弹窗修复后 C2 4 项全部通过，合计覆盖 21 个不同浏览器场景。立即添加来源用例补充关闭后
URL 清理断言，等待清理完成才刷新，不减弱“只打开一次”的业务断言。
真实 Responses smoke 调用 1 次，获得 1 个核验引用及 1 个保存轮次。数据库和浏览器验收无 skip，
最终通过的浏览器报告无自动 retry；修复前的失败记录单独保留。完整命令、日志、截图和新增／修改文件映射见
[设计文档 C2 实施记录](notebook-workspace.md#c2-实施记录2026-10-13)。
本轮不追认上一轮测试的执行顺序；以上复选框表示当前实现与本次验证已完成。
