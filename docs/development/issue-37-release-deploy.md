# Issue #37：容器发行与真实 Nginx SSE 部署验收

基线为 `048ec11`，在独立 `issue-37-deploy` worktree 执行。范围仅为 Issue #37 的
“容器/版本/缓存”和“代理 SSE”，不修改统一验收表，不连接外部模型或搜索。

## 执行方案

1. 使用 `server/Dockerfile` 构建发行镜像及其 Go build stage；在容器内删除 embed
   资产后重编译，要求构建失败。检查运行镜像没有 Node/npm，记录镜像摘要和构建日志。
2. 新建唯一临时 PostgreSQL 数据库，运行发行容器。机械替换 Nginx 模板的监听地址、
   域名和上游地址，保持 location、TLS、缓冲及超时规则；使用临时本地证书。
3. 对照容器 build stage 中的资产逐字节检查 HTTP 响应；重新生成 OpenAPI 类型到
   临时目录并比较；用正式 Web capability 校验器检查实际服务响应。检查缓存与 404。
4. 通过正式配对、设置、学习区、目标和运行 API 创建确定性导师运行。透明 TCP
   观察器只转发字节并记录同一心跳进入 Nginx 和到达 HTTPS 客户端的单调时间。
   检查默认十秒心跳、跨越 75 秒的活跃连接、订阅额度回收、设备撤销及后端 socket。
5. 自动化入口输出结构化报告、配置和日志；失败退出非零，finally 清理本任务容器、
   唯一数据库及本地夹具，保留已有 PostgreSQL 容器。若发现部署缺陷，先保留失败证据，
   再最小修复和定向复验；涉及公共 API、迁移或用户语义时停止报告。

## 环境与可重复入口

成功运行时间：2026-10-11 11:15:56–11:17:30 UTC，退出码 `0`，11 项检查通过。
工作树基线：`048ec1102dfa7f199fce89f5cdaa9517d889ca47`；只有本任务脚本和报告为新增文件。

| 项目 | 实际环境 |
| --- | --- |
| 平台 | Linux amd64；kernel `6.8.0-71-generic`；AMD EPYC 9275F 24-Core Processor |
| Docker | Engine `29.8.2`，Linux host networking；没有创建 Docker 网络 |
| 构建 | Dockerfile 内 Node `24-alpine`、Go `1.26.6-alpine`；最终 Alpine `3.22` |
| 入口 / 模型夹具 | 主机 Node `v26.10.0`；模型只监听本机，鉴权模式 `none` |
| PostgreSQL | 既有 `edu-agent-acc-postgres`，`17.11`，host `127.0.0.1:32995` |
| 临时数据库 | `issue37_release_7fadca8ab6ef`，运行结束后已删除 |
| Nginx | 官方 `nginx/1.30.5`，固定 linux/amd64 manifest digest，见下表 |
| TLS | 临时自签证书，SAN `localhost` / `127.0.0.1`；HTTPS 客户端显式信任该证书 |
| 端口 | 33000：HTTPS；33001：透明 TCP 观察；33002：模型/静默夹具；33004：Go；33005：独立静默探针 Nginx |

从 worktree 根目录运行：

```bash
node scripts/check-release-deploy.mjs
# 需要避开同一任务的默认端口时：
RELEASE_DEPLOY_PORT=33010 node scripts/check-release-deploy.mjs
```

入口只使用指定的验收 PostgreSQL 容器，每次生成唯一库名和容器名。
需要 Docker 权限、OpenSSL，以及现有 `clients/web/node_modules` 中的锁定依赖；
构建镜像需要官方基础镜像与 npm/Go 构建依赖。没有安装主机依赖，没有执行浏览器矩阵。
脚本结束或捕获检查失败时，会删除自身容器、唯一数据库、临时 server.env 和 TLS 私钥。
完整命令、输出、派生配置和结构化结果保留在权限为 `0700` 的任务临时目录。

本次成功输出：

```text
结果：passed；证据：/tmp/codeg-acp/1364849-6e053df6/edu-release-deploy-UQ0a5g/report.json
```

`report.json` SHA-256：`dfc09862b911778a2c0f84ca64c0a786b4333ecd6533a58eb8638bff41d5e841`。
该报告记录每条实际 argv、日志路径、生产输入摘要和逐项证据；临时目录不提交。
脚本 SHA-256：`d9ffcde51ce12980cd23004ed399e5682f3708094dd5cb0f2797d45dc3aec57d`。

## 镜像与输入摘要

| 对象 | digest / SHA-256 |
| --- | --- |
| 服务镜像 `edu-agent-issue37-release:7fadca8ab6ef`，本地 image ID / OCI index | `sha256:50f8c799c34c39be2d721d00a3c542673ef7572573549cffb37911ce189b3641` |
| 服务 linux/amd64 manifest | `sha256:37536dfab8abc2fd8a88a0d26319695998f7977cecca4aa6e751132c824a1684` |
| 官方 Nginx manifest，实际使用 `nginx@sha256:…` | `sha256:8f84ed99befc3891b8f329c5c202785278a2cfb7c25107d57fb2a134a3117433` |
| Node 构建基础镜像 index | `sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1` |
| Go 构建基础镜像 index | `sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83` |
| Alpine 运行基础镜像 index | `sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8` |
| `deploy/web/nginx.conf` | `a91a73d4da8e3818bb92166a75cffe5746cdceddccdd7323ce5469193d92acbf` |
| `clients/web/package-lock.json` | `88c4c869505729bacd21503174cd4e508e9115a27e06aaee19f4223677c7f732` |
| `server/api/openapi.yaml` | `d9791336c3b1f7a25fb3ddaa631aadd337ea5921fe0fab4ae8c81fe42da87d13` |
| 重新生成且与仓库完全相同的 `schema.d.ts` | `0ce4674c50b6bee5e63feeff2e9580e253a6a64457d99de87082f71b523d5535` |

服务镜像是本地构建结果，未推送注册表。Dockerfile 现有基础镜像引用为 tag；
上表记录本次解析结果，不表示已将 Dockerfile 改为 digest 锁定。构建 provenance
可使重建后的 index digest 改变；资产清单和平台 manifest 单独记录。

## 容器、版本、缓存与路由结果

实际构建入口：

```bash
docker build --progress=plain -f server/Dockerfile -t edu-agent-issue37-release:7fadca8ab6ef .
docker build --progress=plain --target build -f server/Dockerfile -t edu-agent-issue37-build:7fadca8ab6ef .
```

| 检查 | 结论与证据 |
| --- | --- |
| 携带真实前端的发行构建 | 通过；真实 Dockerfile 成功构建并启动 `web_release` 服务，readyz 为 200；最终用户为 `10001:10001` |
| 运行镜像无 Node | 通过；在该镜像 `--network none --entrypoint sh` 下检查 node/npm 命令、nodejs/npm 安装包和 `/usr/local/lib/node_modules`，全部不存在；包清单仅 24 项，实际 `uid=10001(eduagent)` |
| 缺前端资产必须构建失败 | 通过；从真实 Go build stage 派生负例 Dockerfile，仅删除 `/src/server/internal/webassets/dist` 后执行正式 `go build -tags web_release ./cmd/edu-agentd`；Docker build 退出 1，报告 `internal/webassets/release.go:9:28: pattern dist/assets: no matching files found` |
| 资产一致性 | 通过；从 build stage 复制 embed 目录，对全部 63 个公开 HTML/JS/CSS/字体/worker 资源逐字节比较直连和 HTTPS 代理响应，126 次资源请求全部一致；目录说明 `README.txt` 返回 404 |
| schema / capability 一致性 | 通过；`openapi-typescript ../../server/api/openapi.yaml -o 临时/schema.d.ts` 与已提交类型逐字节一致；实际响应通过正式 `clients/web/src/api/settings.ts` 的 Zod 校验器，`schema_version=1` |
| capability 与实际配置相符 | 通过；未配置时 `web_mentor.available=false`，正式 PUT 配置本机夹具后为 true；rendering/persistence 为 true，未配置 search 的 research 始终 false；正式运行进入 `waiting_input`，本地模型恰好调用 1 次 |
| 敏感 API no-store | 通过；session/settings/capabilities/spaces/goals/runs/运行快照在直连和代理均含 no-store；另外验证设备列表、SSE、409 过期设置版本、429 订阅超额、204 撤销和撤销后的 401。部分错误响应含重复 `no-store`，按缓存指令语义通过 |
| 404 不回落到 SPA | 通过；下列 8 个路径在直连和代理均为 404，响应不含 `type="module"`；4 个正式页面入口为 200，返回同一 index 且 no-store |

8 个拒绝路径为 `/v1/issue37-missing`、`/v1/learning/issue37-missing`、
`/admin/issue37-missing`、`/app/assets/issue37-missing.js`、`/app/issue37-missing.css`、
`/app/settings/issue37-missing`、`/app/api/issue37-missing`、`/app/README.txt`。
正式页面取样为 `/app/`、`/app/settings`、`/app/progress`、`/app/runs`。

资产版本取实际构建的内容散列文件名；主要资源如下。全部资产摘要保存在 `assets.json`，
规范化清单 SHA-256 为 `5a5a45155b6792ff2b9170192b6518c5986381ebea50bd11266296aee97808a5`。

| 资源 | 字节 | SHA-256 |
| --- | ---: | --- |
| `index.html` | 531 | `940148312ba53c5c732ddfb2eba9f8cc6d981684d3c8f1a193134bfbff23dbfc` |
| `assets/index-CYN16iFM.js` | 1336945 | `832857446c339aab5d3dd6d343d4840ec42bdea95f33f512fc0284c987b2fcfe` |
| `assets/index-D5BUw1u7.css` | 51085 | `a790f614a32284ae947b0d6c91642357fcfc6b7a8a797b5c87e2f24dbe0fcb08` |
| `offline-sw.js` | 12085 | `546a6f6bf846bcf4e5d9f296a3c388427967dc560c466153f1a48e79f1e2302d` |

OpenAPI `info.version=0.2.0`、Web package `version=0.1.0` 和 capability 协议
`schema_version=1` 是不同版本域。本次通过的“一致”指同一候选的真实资产、无漂移
生成类型和可由客户端解析的实际能力响应；没有新增统一版本字段或公共 API。

## 真实 Nginx、SSE 时间与连接回收

主代理使用 `deploy/web/nginx.conf` 原 location 内容，补外层 events/http，机械替换
监听为 `127.0.0.1:33000 ssl`、server_name 为 localhost、上游为透明观察端口 33001。
观察器直连发行容器的 33004，双向 pipe 原始 TCP 字节，不产生 SSE 或改写响应。
导师 SSE、学习 API 和公开资产检查经过真实服务容器与官方 Nginx；服务连接独立 PostgreSQL。
TLS/Host/Origin/安全 Cookie 的正式配对也已通过。

脚本实际执行两套配置的 `docker run --rm … nginx -t`，输出均为：

```text
nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
nginx: configuration file /etc/nginx/nginx.conf test is successful
```

随后执行 `docker exec edu-issue37-7fadca8ab6ef-nginx nginx -T`，确认实际加载：

```nginx
proxy_buffering off;
proxy_cache off;
proxy_read_timeout 75s;
```

读取超时与 Web README 的 75 秒一致。Nginx 的读取超时约束相邻上游读取间隔，
持续有心跳的流可以超过 75 秒；参见 [Nginx 官方代理文档](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_read_timeout)。
Go 原始 SSE 响应同时有 `X-Accel-Buffering: no`，代理响应保持 `text/event-stream` 和 no-store。

时间测量使用同一进程的单调时钟，按请求标识与心跳序号匹配同一帧；上游时刻为观察器
收到完整心跳的时刻，下游为 HTTPS 客户端收到完整 SSE 注释帧的时刻。延迟包含观察器
转发、真实 Nginx、TLS 与本机客户端调度，不是模型 Token 延迟或公网性能。

| 心跳序号 | 上游到达（距请求开始 ms） | HTTPS 客户端到达 ms | 差值 ms |
| --- | ---: | ---: | ---: |
| 1 | 7.899 | 9.033 | 1.134 |
| 2 | 10009.718 | 10010.046 | 0.329 |
| 3 | 20510.220 | 20510.648 | 0.428 |
| 4 | 31010.270 | 31010.556 | 0.286 |
| 5 | 41508.325 | 41508.623 | 0.299 |
| 6 | 51509.438 | 51509.780 | 0.342 |
| 7 | 62009.181 | 62009.450 | 0.269 |
| 8 | 72509.700 | 72510.013 | 0.313 |
| 9 | 83009.150 | 83009.489 | 0.338 |

默认十秒心跳的实测间隔为 `10001, 10501, 10500, 10498, 10001, 10500, 10501, 10499 ms`。
handler 以 500 ms 周期检查心跳到期；脚本允许 9.5–11 秒间隔和每帧小于 1000 ms 的代理延迟。
九帧延迟为 0.269–1.134 ms，活跃真实导师流在第九帧到达的 83.009 秒仍未结束。

| 检查 | 结论与证据 |
| --- | --- |
| 禁缓冲、即时心跳 | 通过；实际加载 off/off，九次同帧比较最大差 1.134 ms；观察 SSE 没有增加模型调用 |
| 75 秒超时 | 通过；真实导师流活跃超过 75 秒。第二个真实 Nginx 用相同 location/75s 配置代理本机静默源，该源只发初始心跳，客户端在 75072 ms 关闭；此静默探针单列，不冒充导师运行 |
| 客户端断开回收 | 通过；四个真实订阅对应服务端 4 个 ESTABLISHED socket，第五条为 429；断开一条后 45 ms 降为 3，新订阅返回 200；全部关闭后降为 0 |
| 设备撤销回收 | 通过；独立 memory 档案配对的设备经正式 DELETE 返回 204，604 ms 内四条已有 SSE 全关闭，服务端 ESTABLISHED 为 0；被撤销 cookie 的 SSE/session/快照请求均为 401 |
| 读事务释放 | 通过；撤销后 `pg_stat_activity` 中仅本任务库的 idle-in-transaction 数量为 0 |
| 管理/内部隔离 | 通过；无 cookie / 学习 cookie 两组，共 24 次请求均为 Nginx 404；透明观察器记录对应上游请求数为 0 |

隔离取样覆盖 `/admin`、`/admin/`、`/admin/api/pairing-codes`、`/admin/api/settings`、
`/internal/privacy/migrations/acquire`、`/internal/nocturne/capabilities`、`/mcp`、
`/livez`、`/readyz`、`/v1/pairings/exchange`、`/v1/knowledge/imports`、`/v1/model/capabilities`。

连接回收的服务端依据是容器内 `cat /proc/net/tcp`，只数本任务服务监听端口 33004 的
ESTABLISHED（状态 `01`）；同时以 429 → 200 验证 handler 的每设备订阅额度确实释放。
事务观察命令为：

```bash
docker exec edu-agent-acc-postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -Atc \
  "SELECT count(*) FROM pg_stat_activity WHERE datname='issue37_release_7fadca8ab6ef' AND state LIKE 'idle in transaction%'"
# 输出：0
```

## 缺陷、清理和未覆盖边界

未发现需要修复的产品缺陷。没有修改 Dockerfile、Nginx 模板、Go、OpenAPI 或迁移。
新增验收脚本在调试中修正了日志文件名、CLI stdout/stderr 分离及测试预期；最终完整
运行通过。目录 README 的 404、重复 no-store 和过期设置版本的 409 均符合既有合同。

暂存审阅另外捕获了新验收脚本的门禁缺陷：Shell `-e` 不会使取反的 `! command -v node`
失败时立即退出。以实际 Node 构建阶段的 `/usr/local/bin/node` 单独复制到发行镜像的
临时负例复现：旧检查退出 `0`，显示 Node 路径；改为显式 `if …; then exit 1; fi`
后，同一负例退出 `1` 并报告“运行镜像包含 Node/npm”。最终入口再次完整通过。
负例 Dockerfile 留在 `/tmp/issue37-node-gate.Dockerfile`，两次输出在
`/tmp/issue37-node-only-red.log` / `/tmp/issue37-node-only-green.log`；两张临时负例镜像已删除。

成功运行清理结果为 `cleanup.status=passed`、`errors=[]`、`postgres_preserved=true`。
本任务所有运行产生的容器和临时数据库均已清理；未创建网络，没有触碰其他项目容器。
临时 server.env 与 TLS 私钥已删除，成功发行镜像和只读证据保留本机。统一验收表未修改。

清理后的独立复核命令与输出：

```bash
docker ps -a --filter name=edu-issue37 --format '{{.Names}}\t{{.Status}}'
# 无输出
docker exec edu-agent-acc-postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -Atc \
  "SELECT datname FROM pg_database WHERE datname LIKE 'issue37_release_%'"
# 无输出
docker inspect --format '{{.State.Running}}' edu-agent-acc-postgres
# true
```

以下项目未运行，不由本次结果推断通过：

- 公网证书/域名、真实网络延迟、负载均衡、多副本粘性路由、跨区域或高并发性能。
- 慢客户端触发 Go 默认 5 秒写超时、代理崩溃/重启、滚动升级和旧资产/旧协议混用。
- 真实模型、外部搜索、浏览器对 fetch 流的消费行为及完整浏览器/原生设备矩阵。
- Nocturne / NoteSync 的完整 Compose 栈、启用管理面的本机认证和内部协议认证；
  本次隔离证据是 Nginx 根本不转发所测路径。
- 新空库迁移用于搭建验收；旧数据升级、备份恢复、长期保存正文和完整 Go/race 矩阵未运行。

构建阶段已有 Vite 的大 chunk 提示；本次未测首屏性能。Dockerfile 的基础镜像 tag 可变化，
重现时须核对新解析 digest。本次只完成这两行的声明范围，不表示 Issue #37 整体或生产发布通过。
