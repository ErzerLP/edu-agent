import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

// 离线验收必须同时具备本地教学模型、签发器和清除密钥，不能只打开浏览器矩阵。
const result = spawnSync(
  process.execPath,
  [
    fileURLToPath(import.meta.resolve('@playwright/test/cli')),
    'test',
    'offline.spec.ts',
    ...process.argv.slice(2),
  ],
  {
    cwd: fileURLToPath(new URL('..', import.meta.url)),
    stdio: 'inherit',
    env: { ...process.env, WEB_WORKSPACE_FIXTURE: '1', WEB_OFFLINE_FIXTURE: '1' },
  },
)
if (result.error) throw result.error
process.exitCode = result.status ?? 1
