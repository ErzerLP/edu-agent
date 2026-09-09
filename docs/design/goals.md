# 多目标管理（Issue #4）开发方案与验收标准

## 已确认的问题

当前基线为 `68d507d`。该版本的 `clients/cli-go/internal/command/goal.go:30` 读取当前教学会话，`:60` 在没有会话时创建会话，`:78` 在已有会话时调用 `switch_goal`。因此新增目标会改变独立的教学状态。

`server/internal/learning/domain.go:110` 的 GoalRevision 只有目标文本、来源及版本链；`server/internal/learning/command.go:152` 已支持文本修订及版本校验，应保留这些能力。当前没有结构化草稿、目标生命周期、按区目标列表或目标详情管理入口。现有测试 `TestGoalSetCreatesSessionOrConfirmsActiveSwitch` 明确断言上述会话副作用；改为保持会话的验收断言后，用 `TestGoalSetPreservesTeachingSession` 验证问题及修复。

## 交付方案

1. 旧命令 `goal set <text>` 只保存目标，不读取、创建或切换教学会话；无模型、无资料同样可用。教学开始与续学仍由教学入口负责。
2. 目标稳定归属于创建时的学习区，旧目标固定属于 #2 默认区，保留原 goal/revision ID 和事件。名称、预期结果、范围及排除项、自述基础、用途、可选完成标准、优先级、截止日期、时区和时间预算随修订保存。自述不进入 mastery。
3. 状态采用 draft/active/paused/completed/archived，允许同区多个 active。暂停、恢复、完成与归档只改变目标；恢复归档返回归档前状态。手动完成记录操作人与依据，不写 Evidence，不代表标准已经验证。
4. 内容和状态变更追加版本，以 expected_version 拒绝并发覆盖；operation_id 支持同请求重试。旧 Activity 和评估保留原修订引用。资料范围、范围说明或完成标准变化在详情中展示，并提示路线可能需要调整。
5. HTTP、CLI、交互页面和既有 MCP 创建入口调用相同应用规则，沿用权限及隐私读写门。目标模块提供是否允许新学习的查询接口，不直接写教学表。
6. 提供按区搜索、状态过滤和分页、详情、创建、多行编辑及状态操作；重名项显示可理解的辅助信息，技术 ID 放详情。保存失败保留输入与操作身份，版本冲突要求用户重新查看差异。
7. 新增个人数据纳入隐私清除与残留验证，同步 OpenAPI、帮助和使用文档。

资料选择复用已合并的 #3 冻结范围契约：`scope_snapshot_id` 归属于学习区，支持多个集合及确定版本。2026-09-09 按用户补充要求将远端已合并的 #2/#3 接入 task/14，保留已接受的 CLI 修复。

剩余批次为一个完整管理链路：复用 learning 的事务、事件、幂等回执及隐私门，追加目标结构化元数据和学习区归属；HTTP 查询及修订、CLI/TUI 编辑、资料选择与生命周期一并交付。状态变更也追加修订，不修改旧行；draft 和 active 可发起新学习，paused/completed/archived 不可发起。归档恢复回到归档前状态，暂停恢复为 active。手动完成记录依据和操作时间，学习标准验证仍标记为未验证。旧行按默认区和 active 解释以保持原教学入口兼容。只有资料范围、范围/排除项、预期结果或完成标准的变化提示路线可能需调整。

详情历史按版本分页；列表用绑定区、搜索和状态的稳定游标。完整内容替换和状态动作分开，均复用原 GoalCommand 版本与 operation_id。未提供截止日期和预算即未设定；提供时区使用 IANA 名称，截止时间带明确偏移并与该时区核对。读写目标均按上下文学习区校验，非默认目标事件不能进入默认区旧时间线。

## 验收标准

- 创建 B 后 A 的状态、焦点、历史不变；无会话时保存目标也不创建会话。
- 无模型和资料可保存一句话草稿；两个目标与结构化信息重启后仍存在。
- 同区多个 active，暂停、归档、恢复及手动完成不修改教学事实或掌握度。
- 修订可追溯，旧题目引用不变；并发修订明确冲突，同操作重试不重复创建。
- 服务端拒绝跨区目标和资料范围引用；空资料草稿可用并明确提示缺口。
- 实际操作分页、搜索、筛选、详情、多行编辑和失败后重试；错误不伪装为空列表。
- 真实 PostgreSQL 验证迁移、归属、持久化、幂等、并发和隐私清除。
- 受影响单测、HTTP/MCP 契约及客户端测试通过，最终运行 build/vet 和必要项目检查；未运行项单独记录。

## 实施状态

已完成问题确认、方案及旧命令与教学会话解耦。`goal help`、TUI 文案、教学完成提示和现有 CLI 规格同步新语义；黑盒教学夹具改为显式调用会话创建 API。

修改前 `go test ./internal/command -run '^TestGoalSetPreservesTeachingSession$' -count=1 -v` 两个子测试均失败：无会话时从 false 变为 true；已有会话时发生一次 switch。修改后两个子测试通过，并断言教学会话查询次数为零。

上一批客户端受影响的 command/dashboard 测试与 vet 通过，生产 CLI 构建通过。当时黑盒夹具仅做编译和 vet 检查，未进行真实 PostgreSQL 与完整 Issue 验收。

上述记录保留为上一批局部修复的证据。2026-09-09 继续完成完整管理链路，结果如下。

## 最终实现与使用

- `learning/goals.go` 拥有结构化信息、状态转换和是否允许开始新学习的窄查询；原 CreateGoal 统一规范命令、版本链和幂等身份。修复旧入口省略 goal_id 时回执查找与提交哈希不一致的问题。
- 迁移 14 为旧版本追加默认区归属及可空管理快照，不改旧 ID、事件或冻结引用。列表与详情从不可变版本读取最新状态，无额外单 active 限制；隐私清除同时清空管理快照并验证无残留。
- 目标 store 通过 knowledge owner 的同事务窄接口验证冻结范围，不直接读取知识表。生命周期不写教学表；教学事务按目标锁检查是否允许新学习，不终止既有会话。默认区旧时间线排除其他区目标，原非目标事件不受影响。
- HTTP 列表/详情/历史使用 learning:read，创建/修订/生命周期使用 learning:write。POST 保持旧入口，PUT 使用明确目标路径。MCP 只为既有创建入口增加显式学习区参数，不新增生命周期权限。
- `goal browse` 和主菜单 `o` 打开列表及详情；`e` 进入多行编辑，`m` 选择资料集合/文档/章节，`v` 查看冻结资料与更新。`g` 和 `goal set` 继续一句话保存，不操作教学。

最小命令示例（省略 `--space` 固定默认区；其他区每次显式传入）：

```sh
edu-agent goal create --name 并发基础 掌握并发编程
edu-agent goal list --search 并发 --status draft --limit 20
edu-agent goal browse
edu-agent goal help
```

修订完整保留结构化字段；清空可选 deadline/weekly-minutes 即取消约束。手动完成必须填写依据，完成状态与标准验证分开显示。新草稿不自动进入 active，也不自动开始教学。非默认区并行教学、会话选择/续学、模型规划、自动验标和跨目标证据继承仍属于独立 Issue，不包含在本次交付。

## 最终验收记录

环境为 Go 1.26.6、PostgreSQL 17/pgvector；复用现有测试容器，新建专用 `edu_agent_task14` 数据库，各测试隔离 schema 并清理。未触碰生产数据库或修改部署配置。本机没有 psql，黑盒测试通过仓库外的临时包装调用同一测试容器内的 psql，没有安装工具或将密码写入文件。

| 验收点 | 已通过的证据 |
| --- | --- |
| 同区双目标、重试、并发、分页、搜索、状态过滤、重启 | `TestPostgreSQLGoalManagementLifecycleScopeRetriesAndHistory`，生产二进制 `TestBlackBoxGoalsWithoutModelPersistenceAndIndependentLifecycle` |
| 无模型、空资料草稿，旧 set 保留已有会话 | 黑盒服务完全移除模型配置；实际配对 CLI 创建、修订、重启及原会话行前后对比；保留上一批 set 回归 |
| 资料冻结、无效/跨区引用、暂停禁止新学习 | `TestPostgreSQLGoalFrozenMaterialsValidation`，真实知识导入和冻结范围，CLI 黑盒关联范围 |
| 暂停/归档/恢复/完成与修订不改历史 | `TestPostgreSQLGoalRevisionsPreserveLearningFactsAndFocus` 比较路线、题目、作答、评估、证据、会话、焦点帧完整行；掌握度不变，旧引用仍保留 |
| 多行输入、失败后保留、确认并发冲突、章节选择重试 | command 的 GoalEditor、GoalBrowser 系列测试；失败重试复用内容与操作身份，远端版本需确认 |
| HTTP/MCP 同一规则、权限及 OpenAPI | `TestPostgreSQLGoalHTTPManagementContract` 验证真实数据库响应符合 schema、跨区拒绝和写权限；MCP 参数归属及非法生命周期参数测试 |
| 历史升级、隐私清除 | 真实数据库 `./migrations`、`./internal/privacy/postgresstore` 全包通过，旧字段不变及新增结构化个人数据清空断言 |

执行结果：

- server 和 clients/cli-go 各自 `go test ./... && go vet ./... && go build ./...` 通过。普通全仓测试中未配置数据库而跳过的场景不计为数据库证据。
- 真实数据库串行 `go test -p=1 -count=1 ./migrations ./internal/learning/postgresstore ./internal/privacy/postgresstore` 中迁移与隐私全包通过；学习全包最初发现时间线过滤误排非目标事件，修复后精确回归和学习 store 全包重新通过。
- 真实数据库目标测试再次以 `-race` 通过；客户端目标/学习区相关 api、command 测试以 `-race` 通过。dashboard 在该筛选下没有匹配测试，使用普通全包测试证据。
- 真实 HTTP 目标契约、生产服务/CLI 黑盒测试通过。`contracttests/cli-m1` 的编译、普通测试和 vet 通过；其他无关黑盒场景未全跑。
- 后补的旧长文本目标兼容测试、资料章节选择失败重试测试均通过。未进行桌面终端人工验收、跨平台原生验收或真实外部模型调用，不将这些写成已验证。

开发方案和验收记录均随代码提交 task/14；不推送或合并 main，由用户审核落地。
