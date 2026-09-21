# Issue #44：OpenAPI 生成类型一致性

## 架构与问题确认

核查基线为 `d591bcc`，初始工作树干净。Go 服务端组合学习、知识、身份、导师、
记忆和隐私服务，PostgreSQL 保存权威状态；Go CLI 和 React Web 消费同一 HTTP
合同，`packages/agentcore` 提供共享模型循环，Nocturne 和 NoteSync 为外部集成。
Web 使用 `openapi-fetch` 消费 `server/api/openapi.yaml` 生成的 TypeScript 类型，
Zod 负责运行时校验，Vite 产物由 Go 嵌入发布。

已从当前代码确认缺陷：

- `server/api/openapi.yaml:9、46、72` 定义了三个 companion 路径，`:924` 开始定义
  导师会话路径；基线 `clients/web/src/api/schema.d.ts:6` 的 `paths` 均未包含它们。
- `scripts/check-web-release.mjs:59–62` 使用锁定生成器重新生成，并逐字节比较
  已提交文件；因此上述缺失必然触发一致性失败。
- `docs/development/issue-38-acceptance.md:66–68` 记录 companion 类型生成未完成；
  `docs/development/issue-39-acceptance.md:133–135` 和提交 `ada8e87` 说明后续只追加
  396 行离线类型，未刷新其他历史生成物。根因是合同更新后生成物未完整同步。

## 开发方案与验收标准

本项保持一个交付批次：使用现有锁文件中的 `openapi-typescript 7.13.0` 完整重新
生成 `clients/web/src/api/schema.d.ts`。复用已有逐字节一致性门禁；不修改源合同、
生成器版本或业务逻辑，不单独手补路径，不扩大到其他已登记问题。

验收标准：

1. 修复前保存临时生成与比较结果，确认生成成功、比较失败及缺失路径。
2. 修复后重新生成临时文件，与提交文件逐字节一致。
3. `make web-release-check` 的 OpenAPI 一致性步骤通过；继续执行前端类型、单测
   和构建检查，并准确记录独立历史失败与因此未执行的步骤。
4. OpenAPI 契约测试和差异格式检查通过；锁文件与源合同不变。

已有发布门禁就是本缺陷的回归检查，不再添加重复比较逻辑。生成器既有
`ActionExposureRequest => record_exposure` 警告单独记录。

## 原任务基线的验收结果

环境为 Node `v24.20.0`、npm `11.19.0`、Go `go1.26.6 linux/amd64`。用户授权后
执行 `cd clients/web && npm ci --no-fund --no-audit`，安装 271 个锁定依赖；
锁文件和源合同均未修改。

修复前，四项缺失路径断言退出 1。随后实际执行工单中的生成与比较：

```bash
clients/web/node_modules/.bin/openapi-typescript server/api/openapi.yaml \
  -o /tmp/edu-issue44-nDclkA/generated-before.d.ts
diff -u clients/web/src/api/schema.d.ts /tmp/edu-issue44-nDclkA/generated-before.d.ts
```

生成退出 0，比较退出 1；首处差异为三个 companion 路径，还包含导师会话、NoteSync、
隐私操作及已有字段说明。完整差异保存在本机 `/tmp/edu-issue44-nDclkA/before.diff`。
修复前 `npm run check` 单独复现 `teaching-page.tsx:881` 的 `present_review` 类型
不匹配（#43），确认它不是本次重新生成引入的问题。

执行 `cd clients/web && npm run generate` 后，生成文件增加 842 行、删除 13 行；
全部来自现有合同，未手工修改生成内容。复核结果如下：

| 检查 | 结果 |
| --- | --- |
| 独立生成到 `generated-after.d.ts` 后执行 `diff -u` | 退出 0，逐字节相同 |
| 原四项缺失路径断言 | 通过 |
| `make web-release-check` | OpenAPI 一致性通过；随后类型检查仍仅报修复前相同的 #43 错误 |
| `cd clients/web && npm test` | 21 个文件、52 项单测通过 |
| `cd server && go test ./api -count=1` | 通过；复用本次修复前结果，源合同与契约测试未改变 |
| `node --test scripts/web-release-results.test.mjs` | 两项通过 |
| `cd clients/web && ./node_modules/.bin/vite build` | 2102 个模块打包成功，资产已复制到 Go embed 目录 |
| `cd server && go build -tags web_release -o /tmp/edu-issue44-nDclkA/edu-agentd ./cmd/edu-agentd` | 使用实际 Web 资产构建成功 |
| `git diff --check` | 通过 |

修复后的生成文件与独立临时文件 SHA256 均为：
`0ce4674c50b6bee5e63feeff2e9580e253a6a64457d99de87082f71b523d5535`。
发布检查报告位于本机
`/tmp/codeg-acp/159555-ab414ac3/edu-web-release-sg5sw6/report.json`。

完整 `make web-release-check` 仍未通过；其后续 Go 测试、vet、发行构建和缺资产
负向检查未由该入口执行。上表单独的 Vite/Go 构建只证明打包与资产嵌入成功，
不代表包含类型检查的 `npm run build` 或完整发布通过。本项未运行数据库及真实
浏览器矩阵；#43 留给其独立任务处理。

生成器仍输出既有 discriminator 警告，但生成成功且结果稳定。npm 的 esbuild
安装脚本授权提示、Vite 的 Zod 注释与大 chunk 警告均未阻止上述打包；未调整依赖
或放宽检查。依赖、构建产物和临时证据不纳入提交。

## 合入最新 main 后的复核（2026-09-21）

落地前将 `main`（`1a718fc`）合入 `task/66`，合并提交为 `fafd2d5`。
自动合并无冲突，没有手动冲突处理；保留主分支全部修复及本项生成类型更新。
主分支已通过 `b6a0094` 修复 #43，因此重新执行此前被阻断的 `make web-release-check`。

本轮命令退出 0，七个检查步骤全部通过：依赖、OpenAPI 生成一致性、前端类型及
52 项单测、非数据库契约测试、Go 静态检查、生产前端资产及 Go 发行构建、缺资产
必须构建失败的负向检查。发布结果判定测试两项通过。
报告为 `/tmp/codeg-acp/159555-2898270c/edu-web-release-oGFGiN/report.json`，
源码输入 SHA256 为 `64cfdc4b587bf846f3f281e6bb907aa37bc1489e896c8b7e0a19046fb51e1be4`。
后续仅补充本验收记录，不改变已验证的源码输入。完整局部发布检查现已通过；
数据库、真实浏览器及外部人工发布验收仍不属于本轮执行范围。
