import { spawn } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
if (!process.env.TEST_DATABASE_URL) throw new Error('浏览器验收必须配置独立 TEST_DATABASE_URL')
let child
let restarting = false
const settingsFile = join(mkdtempSync(join(tmpdir(), 'edu-web-settings-test-')), 'settings.json')
function start() {
  child = spawn('../../server/edu-agentd', ['serve'], {
    stdio: 'inherit',
    env: {
      PATH: process.env.PATH,
      DATABASE_URL: process.env.TEST_DATABASE_URL,
      LISTEN_ADDR: '127.0.0.1:32929',
      PUBLIC_BASE_URL: 'http://127.0.0.1:32929',
      MIGRATE_ON_START: 'true',
      WEB_UI_ENABLED: 'true',
      WEB_UI_ALLOW_LOOPBACK_HTTP: 'true',
      ADMIN_UI_ENABLED: 'false',
      LEARNING_SETTINGS_FILE: settingsFile,
      MODEL_ENDPOINT_ALLOWLIST: '["http://127.0.0.1:1/v1"]',
      DEVICE_RATE_LIMIT_PER_MINUTE: '10000',
      PAIRING_RATE_LIMIT_PER_MINUTE: '1000',
    },
  })
  writeFileSync(
    '/tmp/edu-web-test-server-32929.json',
    JSON.stringify({ supervisor: process.pid, child: child.pid }),
  )
  child.on('exit', (code) => {
    if (restarting) {
      restarting = false
      start()
    } else process.exit(code ?? 1)
  })
}
start()
process.on('SIGUSR2', () => {
  restarting = true
  child.kill('SIGTERM')
})
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => child.kill('SIGTERM'))
