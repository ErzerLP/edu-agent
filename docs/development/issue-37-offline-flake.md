# Issue #37 Chromium 离线核对用例修复

基线 `main@048ec11`，工作分支 `issue-37-offline-flake`。
使用 Google Chrome for Testing `155.0.8059.12`。

## 根因与取舍

这是测试对只读查询次数的假设过严。夹具在
`clients/web/tests/browser/offline-fixture.ts:171` 临时注册一次性拦截器，
恢复网络后又在第 180 行派发合成 `online` 事件。当前 Chromium 中，
先前注册的应用监听器已经开始处理原生事件，临时拦截器才执行，
因此一次夹具重连会触发两次应用重连。
应用在 `clients/web/src/offline-page.tsx:248` 对每次联网事件调用重连；
同步在 `clients/web/src/offline/sync.ts:338` 通过 Web Lock 串行化，
第 357 行先核对未知操作，第 358 行还会查询已确认项以刷新评估/证据结果。
这是两个同步调用的查询，不是同一次调用将未知项重复查询。

临时观测两次均复现 `sync,status,status`，且两个 status 指向同一 operation。
第二次的页面时间序列（毫秒）为：应用身份查询 482.4、原生
`online(isTrusted=true)` 484.2、夹具拦截 484.3、合成
`online(isTrusted=false)` 496.5；写请求 601，随后同一 operation
的两次查询 1284.1 和 1309.5。原断言失败，但页面已确认，未重复写入。
日志保存在 `/tmp/edu-issue37-offline-evidence/diagnostic-order.log`；
临时观测代码已全部移除。没有比较 Chromium 151，不能归因于版本升级。

重复联网事件后查询已有回执属于可接受的幂等只读刷新。
本次只修改 `clients/web/tests/browser/offline.spec.ts` 的目标场景：
允许至少一次 status，并逐条断言为原 sync operation 的 GET；
保留首个写请求的序列检查，确认后和场景结束前均断言 sync 恰好一次。
原操作确认、学习证据接纳、教学会话未改变、锁定和错误口令断言均保留。
没有修改夹具、产品同步逻辑、其他 spec 或 `issue-37-acceptance.md`。

## 验证命令

先在 `clients/web` 执行 `npm run build`，然后在 `server` 执行
`go build -tags web_release -o edu-agentd ./cmd/edu-agentd`，均通过。
构建包含必要的 TypeScript 检查；因未改 src，没有额外执行
`npm run check && npm test`。构建仅出现既有第三方注释和大 bundle 提示。

在 `clients/web` 连续执行下列命令三次，每次由 webdatabase 创建并清理
独立 schema，端口为 32929；没有操作其他项目容器。
将 `N` 分别替换为 1、2、3：

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable' \
WEB_CHROMIUM_PATH='/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome' \
PLAYWRIGHT_JSON_OUTPUT_FILE=/tmp/edu-issue37-offline-evidence/run-N.json \
/tmp/edu-web-baseline-6fe9a7a.5ZH8lk/webdatabase \
npm run test:offline -- --project=chromium --reporter=line,json \
--output=/tmp/edu-issue37-offline-evidence/run-N-artifacts \
> /tmp/edu-issue37-offline-evidence/run-N.log 2>&1
```

| 连续运行 | 结果 | 耗时 | 退出码 |
| --- | --- | --- | --- |
| 1 | 7 passed | 38.7 秒 | 0 |
| 2 | 7 passed | 37.5 秒 | 0 |
| 3 | 7 passed | 32.0 秒 | 0 |

三份 JSON 均核对到 7 个场景首次通过，0 failed、0 skipped、0 retries，
没有收集错误。连续三次通过后停止浏览器验证。
测试文件的 Prettier 格式检查、`node --check` 和 `git diff --check` 通过。
浏览器日志只有既有 `NO_COLOR` / `FORCE_COLOR` 提示。
未运行全量浏览器矩阵、前端单测、Go 全量、race 或 Compose。

本次范围内无未解决问题；没有据此声称完整浏览器矩阵已通过。
日志、JSON、构建产物和临时观测不纳入提交；不推送、不创建 PR。
