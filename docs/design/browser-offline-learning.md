# 浏览器加密离线学习（Issue #39）

## 问题确认与架构

本分支基线 a5bf595。`clients/web/src/main.tsx:23` 的路由树统一使用在线 SessionProvider，没有离线入口；
`clients/web/src/lib/session.tsx:128` 的 SessionProvider 在启动时必须调用 readSession（142 行），
无身份时只显示配对；`clients/web/src/lib/drafts.ts:1` 明确草稿仅在标签页内存。
`clients/web/vite.config.ts:7` 只生成静态资产，没有 Service Worker 或离线存储。
检索 Web 源码的 offline、indexedDB、serviceWorker 无实现命中，功能没有其他名称或开关。

服务端 learning owner 已实现签名包、独立 submission/operation、Inbox 去重、
数据库时间裁决、冻结内容、评估及隐私 possession/purge/ack。CLI 已有原生加密队列。
本专项只增加浏览器适配，不更改历史签名、迁移或在线教学 reducer。
Web 身份默认 12 小时且全局清除会删除会话，不能直接承担最长 37 天的归属核对；
必须提供仅用于离线合同的原设备恢复身份。

## 开发方案

这是一个从显式下载到原操作同步的垂直结果，在同一任务内顺序交付三个批次。

1. 浏览器协议与身份：独立 `WEB_OFFLINE_ENABLED` gate，默认关闭。用户明确保存时，
   使用当前配对身份创建最多 37 天的 HttpOnly 离线 Cookie，仅明确下载可续期，只允许版本化浏览器离线
   API，持续核对设备撤销与原 generation。同步、状态及 purge 复用原 owner。
   跨 generation 仅允许查清除任务及 ack；身份记录不含正文，保留供清除回执。
   首次信任根由已认证同源 HTTPS 返回；loopback HTTP 仅为开发例外。
2. 浏览器密封库：独立 `/app/offline` 入口。显式同意、口令确认、持久化许可和能力
   探测全部成功才创建库。随机 DEK 用口令 PBKDF2-SHA256 派生的 KEK 包装，正文、
   原响应字节、答案、请求及回执用 AES-256-GCM 加密；明文密钥只在解锁内存。
   有界完整快照与完整性标记在 strict IndexedDB 事务中发布并回读核对，Web Locks
   串行协调所有标签页。锁定广播清除其他标签页内存，刷新必须重新解锁。
   口令或密钥丢失不能恢复；整库丢失或部分损坏明确告知，绝无明文降级。
   采用丢失提示路径，不提供跨设备导入恢复，以免复活已经清除的历史包。
3. 签名读取、作答和同步：保存原始响应字符串并验证完整信任链、签名、摘要及绑定。
   浏览器适配 v1 仅支持既有 objective/open 文本作答及无帮助协议，未知内容只读，
   未知授权/作答协议拒绝提交。下载请求先持久化，丢响应重试原请求；答案持久化后
   不可修改；发请求前先持久化 unknown，重连先核对所有 unknown，再发送允许的队列。
   正式回执决定已确认、拒绝、冲突、待评估和待人工复核，离线不评分或写 Evidence。
   Service Worker 只缓存构建白名单资产和离线页壳，逐文件核对构建摘要，版本与适配协议绑定，绝不缓存 API。
   本地删除需要包含未同步数量的明确确认；隐私 purge 清除密封库、缓存和内存密钥，
   核对后提交正式 ack，失败保留待处理状态。ack 响应丢失后，只读查询原设备、原代次
   的已完成回执，不保留明文 challenge 或凭空推断清除成功。

## 验收标准

- 真实服务签发的包断网可读、支持的作答可保存；刷新及重启后解锁恢复。
- 验证响应、包、授权、Activity 摘要和信任链；篡改及未知协议拒绝。
- 原目标/学习区/会话/设备不因导航变更；签名原文和原操作不改写。
- 库创建必须明确授权；错误口令、缺失密钥、配额、权限、部分损坏不显示已保存。
- 两标签页串行写入/同步；保存只在事务提交并回读后显示，未保存输入明确提示。
- 服务端接纳后丢响应先查 operation，只产生一份 Attempt/Evidence；部分成功分别显示。
- 服务端使用原双期限、授权及归属；不推进在线 Session；隐私清除不复活旧正文。
- API Cookie/CSRF、scope、禁用 gate、撤销、过期、generation/purge 在真实 PostgreSQL 验证。
- Service Worker 白名单不含登录、凭据或 API，应用壳不覆盖其他学习区内容。
- 本地清除覆盖密钥/包/答案/队列/缓存；离线无法即时远程擦除，ack 仅在核对后成功。
- 运行 Web 类型、单测、构建、真实 Chromium 断网/存储故障验收，受影响 Go 测试和
  PostgreSQL 离线/隐私合同以及原 CLI 离线回归；未运行项目明确记录。

## 安全与使用边界

只承诺经过验收的 Chromium 能力组合；其他浏览器能力不足时拒绝保存。浏览器持久化
仍可能被用户清理或操作系统删除，未同步答案不在服务器备份。WebCrypto 是加密原语，
不是系统钥匙串或端到端加密；无法防止同源恶意脚本在解锁时读取内容，也不承诺取证
擦除、OS 快照或用户副本。口令不发送服务器。关闭页面丢失未明确保存的输入。

参考：[WebCrypto](https://www.w3.org/TR/WebCryptoAPI/)、
[IndexedDB strict 事务](https://www.w3.org/TR/IndexedDB/)。
