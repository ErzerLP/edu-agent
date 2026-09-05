# CLI Agent 能力扩展：分批交付计划

状态：用户已确认实施；基线 `40da3e1`。此计划不代表各批已经完成，不创建或伪造 Comet Runtime 状态。

## 用户结果和范围

Go CLI Agent 默认总上下文 272000 tokens、最大输出 128000 tokens，长输出可接收、展示、保存与续聊；随后串行补齐 stat、find、mkdir、copy、move，增强 write/edit 参数容量和 search。最终文件工具为 list/read/search/stat/find/write/edit/mkdir/copy/move/archive。沿用工作区 confinement、冻结授权、归档保护和不可自动重放的副作用记录。

用户批准了拆为八个可独立验证的垂直批次；不得把多个工具包装为一个巨型 change，也不建立 basic/full/auto 工具分组。

## 批次与正式合同

| 批次 | 结果 | 正式合同 | 状态 |
| --- | --- | --- | --- |
| C1 | 272k/128k 配置与长输出接收、保存、续聊 | [large-context](../comet/specs/client-agent-large-context/spec.md) | 主体及JSON显式值复核测试/vet通过 |
| C2 | write/edit 大参数 | [large-arguments](../comet/specs/client-file-large-arguments/spec.md) | 已实现，边界/HTTP授权恢复/相关包测试及vet通过 |
| C3 | stat 安全元数据 | [stat](../comet/specs/client-file-stat/spec.md) | 已实现，安全/注册/4096回归及相关包测试、vet通过 |
| C4 | find 路径发现 | [find](../comet/specs/client-file-find/spec.md) | 已实现，相关包测试、4096回归和vet通过 |
| C5 | search 输出、上下文与忽略规则 | [search](../comet/specs/client-file-search-enhancement/spec.md) | 已实现，各单元具名/相关包测试和vet通过 |
| C6 | mkdir 与目录副作用恢复 | [mkdir](../comet/specs/client-file-mkdir/spec.md) | 已实现，相关包/迁移/定向race/vet通过，原生非Linux待验证 |
| C7 | 普通文件流式 copy | [copy](../comet/specs/client-file-copy/spec.md) | 已实现，相关包/迁移/定向race/vet通过，原生非Linux待验证 |
| C8 | 文件、目录安全 move | [move](../comet/specs/client-file-move/spec.md) | 已实现，相关包/迁移/定向race/vet通过，原生非Linux待验证 |

每批都包含 schema、执行、投影/UI、必要 Session 处理、具名测试及受影响 package 检查。共享代码串行写入；独立只读审查可并发。只有该批可运行且验证完成后更新状态。

## 候选复核收尾

C1–C8生产入口及候选复核已完成。跨批复核发现并修复了两项问题：

- 连续文件变更覆盖单一WAL：改为[文件效果有界日志](client-file-effect-journal.md)，执行前保存计划、执行后可信结算；后续变更/偏好标记/取消/崩溃不丢先前效果，容量或I/O失败停止后续副作用。
- mkdir长路径确认：与copy/move共用完整分页，重要路径、父锚点与创建范围显示完后才可批准；拒绝/取消始终可用。

最终record/dirty payload均为v6，容器与checkpoint均为v1。各批文档中的版本升级记录描述引入时的历史版本，以journal文档为最终恢复合同。

最终验证复用各批未失效的具名/整包证据，并补齐：journal三包具名及定向race、agentui完整测试、command/dashboard完整集成、受影响包vet、会话缓存诊断无阻塞、Linux CLI构建与Darwin/Windows amd64 CLI交叉构建。`git diff --check`通过。构建产物：`/tmp/edu-agent-expansion-build.pXkRm7/`。

没有运行真实provider/人工TUI、macOS/Windows原生执行、全仓测试、数据库或Compose矩阵；跨设备move为错误注入证据，未做特权挂载。没有提交或推送。

## 边界

- 不引入 Shell、进程执行、永久删除、归档自动恢复/过期/清理、跨设备复制后删除。
- 不实现目录递归复制、分块写入、完整多文件 patch、图片/PDF/Office、AST/LSP 或跨文件事务。
- 用户既有显式模型配置不自动覆盖；新默认不能保证 provider 支持对应容量。
- 小窗口（当前实现不超过8192）仅压缩非约束工具说明；工具数量、名称、完整参数约束不变，问询描述及终端宽度规则保留。默认272k请求不压缩工具说明；没有 basic/full/auto 分组。
- 默认 5% 安全余量；预留完整 128000 输出时输入总预算为 130400，包含系统提示、工具 schema、记忆、历史及当前输入。
- 最近原文优先但不再无条件强留两轮；必要时允许整轮带来源投影，保持 tool call/result 协议及副作用事实，当前不可压缩轮次超限仍明确失败。
- 当前归档合同继续有效，新增工具不能绕过：普通发现跳过归档，显式只读允许；copy/move 首版源与目标均禁止归档。

## 验证与交付纪律

仅临时工作区和 fake provider。具名检查先于 package；批次稳定后运行一次受影响 package/vet 和必要跨层场景。原生平台证据与交叉编译分别记录。不得默认启动数据库、Compose、全仓 race 或真实付费模型。复用未失效结果。独立失败最多一个有依据的修复后重新分类，不扩张测试支线。

本任务不自动 commit/push，不更改依赖或锁文件。下文各批结果仅记录真实实现与验证，不手工写 Runtime 的 verification.md。
