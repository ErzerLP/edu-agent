# Issue #1：本地工具候选交接

这是Builder交接记录，不是Comet Runtime生成的`verification.md`，也不表示GitHub Issue或Runtime已接受验收。

## 候选与范围

- 最终代码候选：`a74dbbc`，在`main`本地提交；C10为`561a090`，恢复一致性修正为`c1eb418`。未推送、未关闭或修改远端Issue。
- 采用已确认的[轻量化交付方案v2](client-local-tools-delivery.md)，C1–C10生产路径均已落地。没有新增daemon、通用上传协议、流式大文件编辑引擎、服务端权限或数据库变更。
- `comet resume-probe`返回native/no-active-native-changes；没有自行创建或归档workflow。
- 当前状态：C1–C10开发与Linux候选检查完成；清理安全、不重放及正文隐私关键链路的独立只读审查已收齐，确认的缓存撤销P1已修复，未留确认的生产阻断问题。macOS原生、真实跨挂载攻击及Runtime验收未运行。

## 用户结果核对

下表的“通过”只指本候选在Linux上的实际测试，不把跨平台编译、mock provider或静态审查说成macOS/真实服务端验收。

| 单元 | 可观察结果与范围 | 主要证据入口 | 结论 |
| --- | --- | --- | --- |
| [C1](../comet/specs/client-local-execution/spec.md) | OS用户完整Shell权限、管道/重定向、cwd/env/Shell选择、独立任务、查询/等待/停止、stdin累计输入 | localexec、agentloop、agentcontroller、agentui的`TestLocalExecution`/`TestLocalSessionLease`与真实子进程测试 | Linux通过；无daemon/跨重启进程重附着 |
| [C2](../comet/specs/client-local-output/spec.md) | 超内存输出的独立加密留存、分页/检索、保存缺口、恢复只读、no-save内存 | `TestPersistentOutput`、`TestArtifact`、`TestLocalOutput` | Linux通过；任务终态与输出可用性分开 |
| [C3](../comet/specs/client-local-pty/spec.md) | 控制终端、合并输出、输入/中断/EOF/resize及F5人工入口 | 各层`TestPTY` | Linux通过；不是全屏终端模拟器 |
| [C4](../comet/specs/client-large-file-read/spec.md) | 超过1MiB文本范围读取、长行续读、完整hash、投影后真实游标 | 各层`TestLargeFileRead` | Linux通过；仍全文有界读取，不承诺GB级固定内存 |
| [C5](../comet/specs/client-large-file-edit/spec.md) | 大文本局部精确edit、版本/授权/原子发布、独立预算 | 各层`TestLargeFileEdit` | Linux通过；原文及候选分别有界，超限用Shell/调预算 |
| [C6](../comet/specs/client-file-patch/spec.md) | 多hunk/多文件patch、完整diff与逐文件receipt、一次授权、F6独立查看 | `TestCompleteDiff`、`TestPatchPlan`、`TestFileArtifact`、`TestFilePatch` | Linux通过；无跨文件事务或自动回滚 |
| [C7](../comet/specs/client-file-query-pagination/spec.md) | 超过2000项的list/find/search续扫、匹配续页、完整性/未返回/失效区别 | `TestDirectoryScan`/`TestDirectoryGuard`、`TestQuery` | Linux通过；游标仅当前实例有效，不是持久索引 |
| [C8](../comet/specs/client-recursive-copy/spec.md) | 超过32MiB二进制和递归目录复制、冻结完整计划、逐项日志和部分结果 | `TestLargeCopy`、`TestCopyTree`、`TestFileBatch`、`TestRecursiveCopy` | Linux通过；不覆盖/合并/跟随源链接 |
| [C9](../comet/specs/client-archive-restore/spec.md) | 旧归档分页定位、显式source/destination/version恢复、真实位置/冲突 | 各层`TestArchiveRestore` | Linux通过；不猜原路径、不清空容器、不重放 |
| [C10](../comet/specs/client-archive-purge/spec.md) | 明确范围永久清理、YOLO仍确认、完整计划/逐项结算、逻辑与物理字节区分 | `TestArchivePurge`、`TestFileBatchPurge` | Linux通过；只清理归档，不自动过期/后台清理 |

大于64KiB完整写入不是用短命令生成大文件代替验收：`agentloop.TestLocalExecutionCumulativeInputAndReadback`通过模型工具先启动`cat > accumulated.txt`，两次stdin分别传38000字节，再关闭输入/等待退出，最终读取并逐字节核对76000字节完整文件；原始输入与命令不进入checkpoint。localexec内核另覆盖累计160KiB输入。

## 候选检查与证据复用

工作目录：`clients/cli-go`。环境：`go1.26.6 linux/amd64`。

### 完整CLI基线：c1eb418

以下完整模块检查在缓存撤销修正之前执行；修正后只重验受影响路径，不把该基线误称为最终commit重新运行的全模块结果。

```sh
go test -count=1 -timeout=180s ./...
go vet ./...
go build -o /tmp/edu-agent-issue1-candidate.0AHm7J/edu-agent ./cmd/edu-agent
/tmp/edu-agent-issue1-candidate.0AHm7J/edu-agent version
GOOS=darwin GOARCH=arm64 go build -o /tmp/edu-agent-issue1-candidate.0AHm7J/edu-agent-darwin-arm64 ./cmd/edu-agent
```

- 25个包：24个测试包通过、CLI主包无测试文件。vet和Linux完整CLI实际运行通过。
- Darwin/arm64完整CLI交叉构建通过；C10未变化的securefile测试二进制交叉编译证据为`/tmp/edu-agent-c10-build.hzvtwM/securefile-darwin-arm64.test`。它们不等价于原生运行或钥匙串验证。
- 基线日志与包清单在`/tmp/edu-agent-issue1-candidate.0AHm7J/`，不提交二进制、日志、缓存或本地agent状态。
- 基线Go源码/测试及go.mod/go.sum按路径排序的SHA256清单汇总：`bb1b06ac74ed249e5b509018353209647f7fcc1f28d63ff2bcaf43805179c470`；清单为该目录的`source-manifest.sha256`。证据key为基线commit、该摘要、Go版本与Linux/amd64。

### 最终撤销修正：a74dbbc

- 真实Controller测试先复现Clear后内存前缀、EOF、空检索及detach仍可访问，修复后同一组测试通过。
- localexec、agentsession、agentcontroller、agentloop四包全量测试及vet通过；随后agentui、command两包全量测试及vet通过。生产逻辑未再变化，其余基线证据继续有效。
- 三包定向race通过：`go test -race ./internal/localexec ./internal/agentsession ./internal/agentcontroller -run 'Test(PersistentOutput|ArtifactAccess|LocalOutput|LocalSessionLease)' -count=1 -timeout=120s`。
- 最后补充的`TestLocalOutputNewGenerationNeverReusesOwner`已单独通过race及controller vet：共享manager且故意传旧owner配置，也不会让Clear后的新Session复活旧缓存。没有因新测试重新跑全模块。
- 最终代码的Linux完整CLI构建及version实际运行、Darwin/arm64完整CLI交叉构建通过，产物目录`/tmp/edu-agent-issue1-final.JrRH6f/`；`commit`文件记录`a74dbbc`完整ID。
- 最终Go输入清单摘要：`125137a293748fddbb27cf055c4c90123962f6775ce0f14a03b20f5ba241043b`，清单为最终产物目录的`source-manifest.sha256`。最后的文档提交不改变这些Go输入。
- 会话内已检查文件的诊断缓存无error；这不是全项目主动扫描。Markdown目标文件引用与diff格式检查通过。

### 复用的风险定向证据

各批确切命令、源状态和边界记录在[交付设计](client-local-tools-delivery.md)，不无输入变化重复全矩阵：

- C1–C3：真实进程/PTY、等待与取消、输出加密、Session租约和恢复的局部测试及相关race。
- C4–C7：读/edit/patch的版本/发布/投影、原生目录guard及query.Close的定向race。
- C8/C10：逐项日志、旧版本冻结、故障保存前缀、只读恢复、调用身份及Clear含EOF撤销的定向race。
- C10七包定向race：`Test(ArchivePurge|FileBatchPurge|LocalExecutionSmallContext)`通过。
- `c1eb418`恢复修正：`TestOperationErrorTranscriptPersistsWithoutBlockingContinuation`和`TestDirectoryCopyCheckpointReceiptMatchesExecutorContract`的先失败后通过证据、controller全包/vet/两项race通过。

上述完整CLI基线包含恢复一致性修正，缓存撤销修正以受影响包与定向race更新证据；其它未变化批次的适用证据继续有效。

## 独立复核与发现结算

独立审查为只读代码审查；不同单元使用新的执行实例，未把已停止审查的未读范围冒充通过，后续只补指定连接点。各次读取有明确范围与上限，不是全仓逐行审计，也没有代替父代理的运行测试。

| 范围 | 已核对的关键链路 | 结论 |
| --- | --- | --- |
| 永久清理安全 | 明确批准、冻结计划、原生归属/挂载/版本、逐项意图与删除停止条件 | 限定范围未发现确认阻断 |
| 恢复不重放 | WAL消费与身份、旧ID/新ID、只读任务与批量历史、pending/授权不复活 | 限定范围未发现确认阻断 |
| 正文留存与生产连接 | private参数、history/ledger/transcript/title、workspace聚合fallback、diff/batch含EOF读回、no-save/Session绑定、generation/AEAD | 初轮发现缓存P1后停止；后续完成剩余连接点，未发现新的生产阻断 |
| 缓存撤销修正 | 内存/EOF/Search/detach的访问检查、首次保存失败、安全Stop、新memory-only任务 | 修复后的源码复核及最小运行回归支持原P1关闭 |

复核曾指出：若内部调用者给同owner任意注入另一authority，底层Manager可能接受未曾保存metadata的旧任务。该API不是模型入口或导入接口，明确要求同owner只绑定原Session/代际；生产Start/Resume/F2从认证record派生并成组安装handle/owner。独立核查未找到CLI跨authority错绑的可达路径，真实新代际测试亦通过，因此不将该内部契约误用假设冒称生产P1。未来若开放后端导入或任意owner注入，需要单独建立相应身份合同，而不是默认继承本交接结论。

## 已明确的边界与待验收项

1. macOS原生执行尚未验证，包括PTY/job control、目录通知/挂载标识和真实钥匙串；无可用原生runner，不能标通过。
2. Linux使用真实原生目录/挂载标识路径及故障注入，但未运行需要额外挂载能力的真实跨挂载攻击场景。不能把不同mount ID的拒绝测试称为实际bind-mount攻防运行。
3. Shell命令自身以OS用户权限执行；结构化文件工具的工作区、链接、归档保护和确认规则不自动扩展到Shell。no-save限制自动历史，不阻止显式命令或明确批准的文件副作用。
4. 执行未知、输出缺口、保存失败和物理释放量unknown均为有效真实状态；无跨进程CAS、恰好一次、跨文件事务、完整ACL/xattr、安全擦除或自动回滚承诺。
5. 远端Issue、推送、Runtime验收/归档均未执行。独立只读审查不是Runtime授权，不手工生成或修改`verification.md`。
