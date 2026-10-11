import { defineConfig } from '@playwright/test'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import baseline from './playwright.config'

if (process.env.WEB_LIVE_SEARCH !== '1')
  throw new Error('真实 Brave 验收必须显式设置 WEB_LIVE_SEARCH=1')
if (process.env.PWDEBUG || (process.env.DEBUG && process.env.DEBUG !== 'pw:webserver'))
  throw new Error('含凭据的 live 验收只允许 DEBUG=pw:webserver，禁止浏览器 API 调试录制')

process.env.WEB_LIVE_EVIDENCE_DIR ??= mkdtempSync(join(tmpdir(), 'edu-research-live-'))
process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1'

export default defineConfig({
  ...baseline,
  testDir: './tests/live',
  outputDir: join(process.env.WEB_LIVE_EVIDENCE_DIR, 'artifacts'),
  timeout: 180_000,
  expect: { timeout: 15_000 },
  workers: 1,
  retries: 0,
  projects: [
    {
      name: 'chromium',
      use: {
        browserName: 'chromium',
        launchOptions: { executablePath: process.env.WEB_CHROMIUM_PATH },
      },
    },
  ],
  use: {
    ...baseline.use,
    actionTimeout: 15_000,
    navigationTimeout: 15_000,
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
  webServer: {
    command: 'node scripts/test-server.mjs',
    url: 'http://127.0.0.1:32929/livez',
    timeout: 60_000,
    reuseExistingServer: false,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      WEB_WORKSPACE_FIXTURE: '1',
      WEB_MENTOR_FIXTURE: '0',
      WEB_NOTESYNC_FIXTURE: '0',
      WEB_NOCTURNE_FIXTURE: '0',
      WEB_OFFLINE_FIXTURE: '0',
    },
  },
})
