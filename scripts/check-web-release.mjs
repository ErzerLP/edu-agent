import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { copyFileSync, existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir, platform, arch, cpus, release } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { checkGoResults, checkBrowserResults } from './web-release-results.mjs'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const web = join(root, 'clients/web')
const candidate = process.argv.includes('--candidate')
if (process.argv.slice(2).some(arg => arg !== '--candidate')) throw new Error('仅支持 --candidate 参数')
const output = mkdtempSync(join(tmpdir(), 'edu-web-release-'))
const report = { schema: 1, scope: candidate ? '自动化候选（不含外部及人工验收）' : '局部发行检查', started: new Date().toISOString(), environment: { platform: platform(), arch: arch(), kernel: release(), cpu: cpus()[0]?.model, node: process.version }, steps: [], status: 'running' }
const hash = value => createHash('sha256').update(value).digest('hex')

function command(program, args, cwd = root, env = {}, timeout = 600000) {
  const result = spawnSync(program, args, { cwd, env: { ...process.env, ...env }, encoding: 'utf8', timeout, maxBuffer: 64 * 1024 * 1024 })
  if (result.error) throw result.error
  if (result.status !== 0) throw new Error(`${program} ${args.join(' ')} 失败（${result.status}）\n${result.stdout}\n${result.stderr}`)
  return result.stdout
}
function fingerprint() {
  const files = command('git', ['ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', 'server', 'clients', 'packages', 'scripts', 'deploy', 'Makefile']).split('\0').filter(Boolean)
  const input = createHash('sha256')
  for (const path of [...new Set(files)].sort()) {
    input.update(path + '\0')
    input.update(existsSync(join(root, path)) ? readFileSync(join(root, path)) : '<deleted>')
    input.update('\0')
  }
  return input.digest('hex')
}
function step(name, run) {
  console.log(`检查：${name}`)
  const started = Date.now()
  const item = { name, status: 'running' }
  report.steps.push(item)
  try {
    const result = run() ?? ''
    const log = join(output, `${report.steps.length}.log`)
    writeFileSync(log, typeof result === 'string' ? result : JSON.stringify(result), { mode: 0o600 })
    Object.assign(item, { status: 'passed', log, sha256: hash(readFileSync(log)) })
  } catch (error) {
    item.status = 'failed'
    throw error
  } finally { item.duration_ms = Date.now() - started }
}

try {
  report.commit = command('git', ['rev-parse', 'HEAD']).trim()
  report.input_sha256 = fingerprint()
  report.environment.go = command('go', ['version']).trim()
  step('依赖与测试环境', () => {
    if (!existsSync(join(web, 'node_modules/.bin/vite'))) throw new Error('缺少 Web 锁定依赖；取得安装授权后先执行 npm ci，不自动安装')
    if (candidate && !process.env.TEST_DATABASE_URL) throw new Error('缺少独立 TEST_DATABASE_URL，数据库检查不能计为通过')
    report.environment.playwright = JSON.parse(readFileSync(join(web, 'node_modules/@playwright/test/package.json'), 'utf8')).version
    report.environment.lock_sha256 = hash(readFileSync(join(web, 'package-lock.json')))
  })
  step('OpenAPI 生成一致性', () => {
    const generated = join(output, 'schema.d.ts')
    const log = command(join(web, 'node_modules/.bin/openapi-typescript'), ['../../server/api/openapi.yaml', '-o', generated], web)
    if (!readFileSync(generated).equals(readFileSync(join(web, 'src/api/schema.d.ts')))) throw new Error('OpenAPI 生成类型有漂移；请重新生成并审阅，不自动覆盖')
    return log
  })
  step('前端类型和单测', () => command('make', ['web-check']))
  step('非数据库页面、代理、身份及安全契约（数据库 skip 不计证据）', () => command('go', ['test', './api', './internal/transport/httpapi', './internal/research', '-count=1'], join(root, 'server'), { TEST_DATABASE_URL: '' }))
  step('Go 静态检查', () => command('go', ['vet', './api', './internal/transport/httpapi'], join(root, 'server')))
  step('生产前端资产及 Go 发行', () => {
    const log = command('npm', ['run', 'build'], web)
    return log + command('go', ['build', '-tags', 'web_release', '-o', 'edu-agentd', './cmd/edu-agentd'], join(root, 'server'))
  })
  step('缺少资产必须构建失败', () => {
    const dir = mkdtempSync(join(tmpdir(), 'edu-web-missing-assets-'))
    try {
      copyFileSync(join(root, 'server/internal/webassets/release.go'), join(dir, 'release.go'))
      const result = spawnSync('go', ['build', '-tags', 'web_release', '.'], { cwd: dir, env: { ...process.env, GOWORK: 'off', GO111MODULE: 'off' }, encoding: 'utf8', timeout: 60000 })
      if (result.error || result.status === 0 || !/no matching files found/.test(result.stderr)) throw new Error('缺资产构建没有按 embed 契约失败')
      return result.stderr
    } finally { rmSync(dir, { recursive: true, force: true }) }
  })
  if (candidate) {
    step('生产 CLI 构建', () => command('make', ['cli-build']))
    step('真实 PostgreSQL、空库研究、CLI 同一教学、竞态和 SSE', () => {
      const log = command('go', ['test', '-json', '-p=1', '-count=1', '-timeout=10m', './internal/transport/httpapi', './internal/mentorrun', '-run', '^TestPostgreSQL(Web|StudyCLI|StartLearning|Research|Adaptive|MentorCookieHTTPAndSSERecovery)'], join(root, 'server'), { EDU_AGENT_INTEGRATION_CLI: join(root, 'clients/cli-go/bin/edu-agent') }, 1500000)
      checkGoResults(log, ['TestPostgreSQLStudyCLIResearchAndWebContent', 'TestPostgreSQLStartLearningEmptyLibrary', 'TestPostgreSQLAdaptiveQueueImmediateAndCompensation', 'TestPostgreSQLResearchGlobalErasureCannotReviveSources', 'TestPostgreSQLMentorCookieHTTPAndSSERecovery'])
      return log
    })
    // 每个文件重启夹具：设置探测、全局清除与导师端点变更不可污染下一套用例。
    for (const project of ['chromium', 'firefox', 'webkit']) {
      for (const file of readdirSync(join(web, 'tests/browser')).filter(file => file.endsWith('.spec.ts')).sort()) {
        step(`${project} / ${file}`, () => {
          const json = join(output, `${project}-${file}.json`)
          const fixture = !['learning.spec.ts', 'settings.spec.ts'].includes(file)
          const browserCommand = file === 'offline.spec.ts'
            ? ['run', 'test:offline', '--']
            : ['run', 'test:browser', '--', file]
          const log = command('npm', [...browserCommand, `--project=${project}`, '--reporter=json'], web, {
            WEB_RELEASE_MATRIX: '1', WEB_WORKSPACE_FIXTURE: fixture ? '1' : '0', WEB_MENTOR_FIXTURE: fixture ? '1' : '0', WEB_NOTESYNC_FIXTURE: file === 'notesync.spec.ts' ? '1' : '0', PLAYWRIGHT_JSON_OUTPUT_FILE: json,
          }, 1200000)
          checkBrowserResults(JSON.parse(readFileSync(json, 'utf8')), project)
          return log + `\n浏览器报告 SHA256：${hash(readFileSync(json))}`
        })
      }
    }
  }
  if (fingerprint() !== report.input_sha256) throw new Error('检查期间源码输入改变，证据失效')
  report.status = 'passed'
} catch (error) {
  report.status = 'failed'
  report.reason = error.message
  console.error(error.message)
  process.exitCode = 1
} finally {
  report.finished = new Date().toISOString()
  writeFileSync(join(output, 'report.json'), JSON.stringify(report, null, 2) + '\n', { mode: 0o600 })
  console.log(`证据：${join(output, 'report.json')}；完整发布仍须核对 docs/development/issue-37-acceptance.md 的未通过项。`)
}
