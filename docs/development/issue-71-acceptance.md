# Issue #71：知识结构浏览器用例收集失败

## 问题确认与架构

基线 `2ca85f5`，分支 `task/94`，初始工作树干净。项目由 Go 模块化服务、
PostgreSQL、React Web、独立 Go CLI 和共享 agentcore 模块组成。Web 生产资产由
Go 服务同域提供；Playwright 在 Node.js 中收集用例，再启动真实服务和浏览器。
本次只涉及浏览器测试的运行环境边界，不改变 knowledge owner 的产品行为。

以下行号对应修复前基线，导入链确认报告中的缺陷仍然存在：

- `clients/web/tests/browser/knowledge-structure.spec.ts:5` 为构造历史概念夹具，
  运行时导入 `emptyConcept`；第 30 行是该函数唯一的调用处。
- `clients/web/src/api/structure.ts:2` 运行时导入浏览器 HTTP 客户端。
- `clients/web/src/api/client.ts:34–38` 在模块初始化时创建 `publicClient`，
  第 35 行直接读取 `window.location.origin`。Node.js 没有 `window`，
  因此测试注册前就会抛错，是否启动数据库、服务和浏览器与此无关。
- `clients/web/src/api/structure.test.ts:2` 在导入前模拟了 `window`，
  所以既有协议单测通过不能证明 Playwright 用例可在 Node.js 中收集。

## 开发方案与验收标准

一个批次完成测试边界修复：只通过 `import type` 引用生产协议类型，历史概念
夹具在测试内显式定义并使用 `ConceptContent` 约束字段，切断收集阶段对浏览器
客户端的运行时依赖。保留现有业务交互和断言，不给 Node.js 模拟浏览器全局，
也不修改生产客户端的同源校验、凭据或初始化方式。

验收标准：

1. 不配置数据库、不启动服务和浏览器时，单文件 `--list` 能列出原用例，退出 0。
2. 全部浏览器文件可以收集；三引擎配置下该文件均可收集。
3. Web 类型检查、单测和生产构建通过，`git diff --check` 通过。
4. 本机现有浏览器及独立数据库可用时，执行原真实浏览器场景，核对审阅、
   未知结果恢复、列表/图、键盘、移动宽度和无障碍断言；缺少环境时明确记录未运行。

## 验收记录

初次执行 `npx --no-install playwright test knowledge-structure.spec.ts --list`
因工作树没有 `node_modules`，在工具检查阶段退出 1（缺少 Playwright），
尚未进入测试收集。Node.js 为 `24.20.0`。按用户提供的全局规范，安装依赖需要
明确授权，此次未安装依赖。

已完成纯类型导入和本地历史夹具修复。`node --check
clients/web/tests/browser/knowledge-structure.spec.ts` 与 `git diff --check`
通过；前者只检查语法，不代表 TypeScript 类型检查或 Playwright 收集通过。
修复后的收集、Web 单测、类型检查、构建和真实浏览器场景尚未执行；现有检查
不能替代这些运行验证。未修改生产代码、锁文件或全局依赖。

用户已接受本次修复并明确要求落入 `main`，合并不代表上述未运行检查已经通过。
