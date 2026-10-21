import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

if (!process.env.TEST_DATABASE_URL) throw new Error('必须设置独立 TEST_DATABASE_URL')
process.umask(0o022)
const sourceResearch = process.argv.includes('--source-research')
const web = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const server = resolve(web, '../../server')
const staging = mkdtempSync(join(tmpdir(), 'mentor-web-search-'))
const binary = join(staging, 'server.test')
const launcher = join(staging, 'launch')
const quote = value => "'" + value.replaceAll("'", "'\"'\"'") + "'"
try {
  const build = spawnSync('go', ['test', '-p=1', '-c', '-tags', 'web_release', '-o', binary, './internal/app'], { cwd: server, stdio: 'inherit' })
  if (build.error) throw build.error
  if (build.status !== 0) throw new Error('浏览器假服务构建失败')
  writeFileSync(launcher, `#!/bin/sh
if [ "$1" = serve ]; then
  exec ${quote(binary)} -test.run '^TestMentorWebBrowserServer$' -test.timeout 10m
fi
exec ${quote(join(server, 'edu-agentd'))} "$@"
`, { mode: 0o755 })
  const browserArgs = ['run', 'test:browser', '--', sourceResearch ? 'tests/browser/source-research.spec.ts' : 'tests/browser/mentor-web-search.spec.ts', '--project=chromium']
  const result = spawnSync(sourceResearch ? 'go' : 'npm', sourceResearch ? ['run', './testutil/webdatabase', 'npm', '--prefix', web, ...browserArgs] : browserArgs, {
    cwd: sourceResearch ? server : web,
    stdio: 'inherit',
    env: { ...process.env, WEB_MENTOR_FIXTURE: '1', WEB_SOURCE_RESEARCH_FIXTURE: sourceResearch ? '1' : '0', WEB_SEARCH_BROWSER_FIXTURE: '1', WEB_NOTEBOOK_FIXTURE_BINARY: launcher },
  })
  if (result.error) throw result.error
  process.exitCode = result.status ?? 1
} finally {
  rmSync(staging, { recursive: true, force: true })
}
