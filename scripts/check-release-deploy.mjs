// Linux/Docker acceptance gate. Uses only a unique database in the designated
// PostgreSQL container and loopback ports; never runs a provider or browser matrix.
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createHash, randomUUID } from 'node:crypto'
import { createServer as httpServer, request as httpRequest } from 'node:http'
import { request as httpsRequest } from 'node:https'
import { createServer as tcpServer, connect } from 'node:net'
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir, platform, release, cpus } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'
import { performance } from 'node:perf_hooks'
import { createRequire } from 'node:module'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const web = join(root, 'clients/web')
const require = createRequire(join(web, 'package.json'))
const nginxImage = 'nginx@sha256:8f84ed99befc3891b8f329c5c202785278a2cfb7c25107d57fb2a134a3117433'
const port = Number(process.env.RELEASE_DEPLOY_PORT ?? 33000)
assert(Number.isInteger(port) && port >= 33000 && port <= 33094, '端口必须在 33000–33094')
assert.equal(platform(), 'linux', '本入口需要 Linux host networking')
assert.equal(process.argv.length, 2, '配置通过 RELEASE_DEPLOY_PORT 环境变量提供')
const output = await mkdtemp(join(tmpdir(), 'edu-release-deploy-'))
await chmod(output, 0o700)
console.log(`证据目录：${output}`)
const id = randomUUID().replaceAll('-', '').slice(0, 12)
const database = `issue37_release_${id}`
const postgres = 'edu-agent-acc-postgres'
const image = `edu-agent-issue37-release:${id}`
const builder = `edu-agent-issue37-build:${id}`
const service = `edu-issue37-${id}-server`
const proxy = `edu-issue37-${id}-nginx`
const probeProxy = `edu-issue37-${id}-idle-nginx`
const origin = `https://localhost:${port}`
const backend = `http://127.0.0.1:${port + 4}`
const report = { schema: 1, started: new Date().toISOString(), output, status: 'running',
  environment: { platform: platform(), kernel: release(), cpu: cpus()[0]?.model, node: process.version },
  resources: { database, service, proxy, probeProxy, image, builder, ports: [port, port + 1, port + 2, port + 4, port + 5] },
  steps: [], commands: [] }
let ca, databaseCreated = false, commandNumber = 0
const containers = []
const localServers = []
const sockets = new Set()
const streams = new Set()
const observations = new Map()
const requestsSeen = []
const hash = data => createHash('sha256').update(data).digest('hex')
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))

async function command(program, args, { cwd = root, allowFailure = false, timeout = 600000 } = {}) {
  const log = join(output, `${++commandNumber}-${basename(program)}.log`)
  report.commands.push({ program, args, log })
  let text = '', stdout = ''
  const child = spawn(program, args, { cwd, stdio: ['ignore', 'pipe', 'pipe'] })
  const timer = setTimeout(() => child.kill('SIGKILL'), timeout)
  child.stdout.on('data', data => { stdout += data; text += data })
  child.stderr.on('data', data => { text += data })
  try {
    const status = await new Promise((resolve, reject) => {
      child.on('error', reject)
      child.on('close', resolve)
    })
    if (!allowFailure && status !== 0) throw new Error(`${program} 失败（${status}），日志：${log}\n${text.slice(-2500)}`)
    return { status, text, stdout, log }
  } finally {
    clearTimeout(timer)
    await writeFile(log, text, { mode: 0o600 })
  }
}
const docker = (args, options) => command('docker', args, options)
const psql = sql => docker(['exec', postgres, 'psql', '-U', 'postgres', '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-Atc', sql])

async function step(name, run) {
  console.log(`检查：${name}`)
  const item = { name, status: 'running', started: new Date().toISOString() }
  report.steps.push(item)
  const started = performance.now()
  try { item.evidence = await run(); item.status = 'passed' }
  catch (error) { item.status = 'failed'; item.reason = error.message; throw error }
  finally { item.duration_ms = Math.round(performance.now() - started) }
}
async function until(predicate, timeout = 10000, description = '条件未满足') {
  const deadline = performance.now() + timeout
  do { if (await predicate()) return; await delay(100) } while (performance.now() < deadline)
  throw new Error(description)
}
function options(path, actor, target = origin, method = 'GET', body) {
  const url = new URL(path, target)
  const headers = { Host: new URL(origin).host, Origin: origin }
  if (actor) { headers.Cookie = actor.cookie; headers['X-CSRF-Token'] = actor.csrf }
  if (actor?.space) headers['X-Learning-Space-ID'] = actor.space
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  return { hostname: '127.0.0.1', port: url.port, path: url.pathname + url.search, method, headers,
    agent: false, ca, servername: 'localhost' }
}
function request(path, { actor, target = origin, method = 'GET', body } = {}) {
  return new Promise((resolve, reject) => {
    const send = target.startsWith('https:') ? httpsRequest : httpRequest
    const req = send(options(path, actor, target, method, body), response => {
      const chunks = []
      response.on('data', chunk => chunks.push(chunk))
      response.on('error', reject)
      response.on('end', () => {
        const data = Buffer.concat(chunks)
        resolve({ status: response.statusCode, headers: response.headers, data,
          json: () => JSON.parse(data.toString('utf8')) })
      })
    })
    req.on('error', reject)
    req.setTimeout(15000, () => req.destroy(new Error(`请求超时：${path}`)))
    req.end(body === undefined ? undefined : JSON.stringify(body))
  })
}
async function api(path, actor, method = 'GET', body, status = 200) {
  const response = await request(path, { actor, method, body })
  assert.equal(response.status, status, `${method} ${path}: ${response.data}`)
  noStore(response, path)
  return response.json()
}
function noStore(response, path = '响应') {
  assert.match(response.headers['cache-control'] ?? '', /(?:^|,)\s*no-store\s*(?:,|$)/i, `${path} 应禁止缓存`)
}
async function paired(profile) {
  const code = (await docker(['exec', service, 'edu-agentd', 'pairing-code', 'create', '--profile', profile])).stdout.trim()
  const response = await request('/v1/web/pairings', { method: 'POST', body: { code, display_name: `Issue37 ${profile}` } })
  assert.equal(response.status, 201, `配对失败：${response.data}`)
  noStore(response, '/v1/web/pairings')
  const session = response.json()
  const cookie = response.headers['set-cookie'][0]
  assert.match(cookie, /^__Host-edu_web=/)
  assert.match(cookie, /; Secure/)
  assert.match(cookie, /; HttpOnly/)
  assert.match(cookie, /; SameSite=Strict/)
  assert.equal(session.server_id, origin)
  return { cookie: cookie.split(';')[0], csrf: session.csrf_token, device: session.device.id, capabilities: session.capabilities }
}
function track(socket) {
  sockets.add(socket)
  socket.on('close', () => sockets.delete(socket))
  socket.on('error', () => socket.destroy())
  return socket
}
async function listen(server, number) {
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(number, '127.0.0.1', resolve) })
  localServers.push(server)
}
function stream(path, actor, { name = randomUUID(), target = origin } = {}) {
  return new Promise((resolve, reject) => {
    const config = options(path, actor, target)
    config.headers['X-Acceptance-Stream'] = name
    const state = { name, heartbeats: [], ended: false, started: performance.now() }
    const send = target.startsWith('https:') ? httpsRequest : httpRequest
    const req = send(config, response => {
      state.status = response.statusCode
      state.headers = response.headers
      let pending = ''
      response.setEncoding('utf8')
      response.on('data', chunk => {
        pending += chunk
        let end
        while ((end = pending.indexOf('\n\n')) !== -1) {
          if (pending.slice(0, end) === ': heartbeat') state.heartbeats.push(performance.now())
          pending = pending.slice(end + 2)
        }
      })
      response.on('end', () => { state.ended = true; state.endedAt = performance.now() })
      response.on('close', () => { state.ended = true; state.endedAt ??= performance.now() })
      response.on('error', () => { state.ended = true; state.endedAt ??= performance.now() })
      state.close = () => { req.destroy(); response.destroy(); streams.delete(state) }
      streams.add(state)
      resolve(state)
    })
    req.on('error', reject)
    req.setTimeout(95000, () => req.destroy(new Error('SSE 没有数据超过 95 秒')))
    req.end()
  })
}
async function nginxConfig(number, upstream, filename) {
  const source = await readFile(join(root, 'deploy/web/nginx.conf'), 'utf8')
  const block = source.replace('listen 443 ssl;', `listen 127.0.0.1:${number} ssl;`)
    .replace('server_name learn.example;', 'server_name localhost;')
    .replaceAll('http://127.0.0.1:8080', `http://127.0.0.1:${upstream}`)
  const path = join(output, filename)
  await writeFile(path, `worker_processes 1;\nevents { worker_connections 128; }\nhttp {\n${block}\n}\n`)
  return path
}
async function runProxy(name, config) {
  const mounts = ['--network', 'host', '-v', `${config}:/etc/nginx/nginx.conf:ro`, '-v', `${output}/certs:/etc/nginx/certs:ro`]
  await docker(['run', '--rm', '--name', `${name}-syntax`, ...mounts, nginxImage, 'nginx', '-t'])
  await docker(['run', '-d', '--name', name, ...mounts, nginxImage])
  containers.push(name)
}
async function backendConnections() {
  const raw = (await docker(['exec', service, 'cat', '/proc/net/tcp'])).text
  const suffix = `:${(port + 4).toString(16).toUpperCase().padStart(4, '0')}`
  return raw.split('\n').filter(line => { const fields = line.trim().split(/\s+/); return fields[1]?.endsWith(suffix) && fields[3] === '01' }).length
}

try {
  report.commit = (await command('git', ['rev-parse', 'HEAD'])).text.trim()
  report.environment.docker = (await docker(['version', '--format', '{{.Server.Version}}'])).text.trim()
  report.input_sha256 = {}
  for (const file of ['server/Dockerfile', '.dockerignore', 'deploy/web/nginx.conf', 'clients/web/package-lock.json', 'server/api/openapi.yaml', 'clients/web/src/api/schema.d.ts', 'clients/web/src/api/settings.ts']) {
    report.input_sha256[file] = hash(await readFile(join(root, file)))
  }
  await step('真实 Dockerfile 构建发行镜像', async () => {
    const result = await docker(['build', '--progress=plain', '-f', 'server/Dockerfile', '-t', image, '.'])
    const inspection = JSON.parse((await docker(['image', 'inspect', image])).text)[0]
    report.image = { id: inspection.Id, repo_digests: inspection.RepoDigests, user: inspection.Config.User,
      manifest: result.text.match(/exporting manifest (sha256:[a-f0-9]+)/)?.[1],
      index: result.text.match(/exporting manifest list (sha256:[a-f0-9]+)/)?.[1],
      bases: [...result.text.matchAll(/FROM ([^\s]+@sha256:[a-f0-9]+)/g)].map(match => match[1]), log: result.log }
    assert.equal(inspection.Config.User, '10001:10001')
    return report.image
  })
  await step('运行镜像无 Node/npm', async () => {
    return (await docker(['run', '--rm', '--name', `edu-issue37-${id}-runtime`, '--network', 'none', '--entrypoint', 'sh', image, '-ec',
      'if command -v node || command -v npm || apk info -e nodejs || apk info -e npm || test -e /usr/local/lib/node_modules; then echo "运行镜像包含 Node/npm" >&2; exit 1; fi; id; apk info'])).text
  })
  await step('容器内缺 embed 资产构建失败', async () => {
    await docker(['build', '--progress=plain', '--target', 'build', '-f', 'server/Dockerfile', '-t', builder, '.'])
    const extract = `edu-issue37-${id}-extract`
    await docker(['create', '--name', extract, builder, 'true'])
    containers.push(extract)
    await docker(['cp', `${extract}:/src/server/internal/webassets/dist`, join(output, 'assets')])
    const negative = join(output, 'missing-assets.Dockerfile')
    await writeFile(negative, `FROM ${builder}\nRUN rm -rf /src/server/internal/webassets/dist && cd /src/server && go build -tags web_release -o /tmp/missing-assets ./cmd/edu-agentd\n`)
    const result = await docker(['build', '--progress=plain', '-f', negative, output], { allowFailure: true })
    assert.notEqual(result.status, 0)
    assert.match(result.text, /pattern dist\/(?:assets|index\.html|offline-sw\.js): no matching files found/)
    return { exit: result.status, diagnostic: result.text.match(/internal\/webassets\/release.go[^\n]+/)?.[0], log: result.log }
  })
  await step('OpenAPI 类型生成与正式 Web capability 校验器', async () => {
    const generated = join(output, 'schema.d.ts')
    await command(join(web, 'node_modules/.bin/openapi-typescript'), ['../../server/api/openapi.yaml', '-o', generated], { cwd: web })
    assert.deepEqual(await readFile(generated), await readFile(join(web, 'src/api/schema.d.ts')), 'OpenAPI 生成漂移')
    return { schema_sha256: report.input_sha256['server/api/openapi.yaml'], types_sha256: hash(await readFile(generated)) }
  })
  await step('独立数据库、真实服务容器、TLS Nginx 和本地确定性模型', async () => {
    // Reserve every required loopback port before creating external resources.
    for (const number of report.resources.ports) {
      const reservation = tcpServer()
      await listen(reservation, number)
      await new Promise(resolve => reservation.close(resolve))
      localServers.pop()
    }
    const certs = join(output, 'certs')
    await mkdir(certs)
    await command('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=localhost',
      '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1', '-keyout', join(certs, 'learn.example.key'), '-out', join(certs, 'learn.example.crt')])
    ca = await readFile(join(certs, 'learn.example.crt'))
    let calls = 0
    const model = httpServer(async (req, res) => {
      if (req.url === '/v1/learning/runs/idle-probe/events?after=0') {
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', 'X-Accel-Buffering': 'no' })
        res.write(': heartbeat\n\n')
        return // Intentionally silent source for the separate 75-second idle probe.
      }
      const chunks = []
      for await (const chunk of req) chunks.push(chunk)
      try {
        const payload = JSON.parse(Buffer.concat(chunks))
        assert.equal(req.url, '/v1/chat/completions')
        assert.equal(payload.stream, true)
        assert(payload.messages.some(message => String(message.content).includes('发布代理验收目标')))
        calls++
        const delta = { role: 'assistant', tool_calls: [{ index: 0, id: 'release-question', type: 'function',
          function: { name: 'ask_user', arguments: JSON.stringify({ question: '选择验收方向', choices: ['心跳', '连接回收'] }) } }] }
        res.writeHead(200, { 'Content-Type': 'text/event-stream' })
        res.end(`data: ${JSON.stringify({ choices: [{ index: 0, delta }] })}\n\ndata: ${JSON.stringify({ choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] })}\n\ndata: [DONE]\n\n`)
      } catch (error) { report.fixture_error = error.message; res.writeHead(400); res.end() }
    })
    model.on('connection', track)
    await listen(model, port + 2)
    report.modelCalls = () => calls
    // Passive TCP observation: preserve all bytes, pair identical heartbeat
    // frames by stream header and ordinal, and use one monotonic clock.
    const tap = tcpServer(client => {
      track(client).setNoDelay(true)
      const upstream = track(connect(port + 4, '127.0.0.1'))
      upstream.setNoDelay(true)
      let header = '', pending = '', record
      client.on('data', chunk => {
        if (record) return
        header += chunk.toString('latin1')
        if (!header.includes('\r\n\r\n')) return
        const path = header.split('\r\n')[0].split(' ')[1]
        const streamName = header.match(/\r\nx-acceptance-stream: ([^\r]+)/i)?.[1]
        requestsSeen.push(path)
        record = { path, heartbeats: [], opened: performance.now() }
        if (streamName) observations.set(streamName, record)
        header = ''
      })
      upstream.on('data', chunk => {
        pending += chunk.toString('latin1')
        let found
        while ((found = pending.indexOf(': heartbeat\n\n')) !== -1) {
          record?.heartbeats.push(performance.now())
          pending = pending.slice(found + ': heartbeat\n\n'.length)
        }
        pending = pending.slice(-64)
      })
      upstream.on('close', () => { if (record) record.closed = performance.now(); client.destroy() })
      client.on('close', () => upstream.destroy())
      client.pipe(upstream); upstream.pipe(client)
    })
    await listen(tap, port + 1)
    await psql(`CREATE DATABASE ${database}`)
    databaseCreated = true
    const envFile = join(output, 'server.env')
    await writeFile(envFile, [
      `DATABASE_URL=postgres://postgres:postgres@127.0.0.1:32995/${database}?sslmode=disable&application_name=issue37_release`,
      `LISTEN_ADDR=127.0.0.1:${port + 4}`, `PUBLIC_BASE_URL=${origin}`, 'MIGRATE_ON_START=true', 'WEB_UI_ENABLED=true',
      'ADMIN_UI_ENABLED=false', 'LEARNING_SETTINGS_FILE=/var/lib/edu-agent/learning-settings/settings.json',
      `MODEL_ENDPOINT_ALLOWLIST=["http://127.0.0.1:${port + 2}/v1"]`, 'DEVICE_RATE_LIMIT_PER_MINUTE=10000',
    ].join('\n') + '\n', { mode: 0o600 })
    await docker(['run', '-d', '--name', service, '--network', 'host', '--env-file', envFile, image])
    containers.push(service)
    await until(async () => { try { return (await request('/readyz', { target: backend })).status === 200 } catch { return false } }, 30000, '服务没有就绪')
    await runProxy(proxy, await nginxConfig(port, port + 1, 'nginx.conf'))
    await runProxy(probeProxy, await nginxConfig(port + 5, port + 2, 'idle-nginx.conf'))
    await until(async () => { try { return (await request('/app/')).status === 200 } catch { return false } }, 10000, 'Nginx 没有就绪')
    const loaded = await docker(['exec', proxy, 'nginx', '-T'])
    assert.match(loaded.text, /proxy_buffering off;/)
    assert.match(loaded.text, /proxy_cache off;/)
    assert.match(loaded.text, /proxy_read_timeout 75s;/)
    assert.match(await readFile(join(web, 'README.md'), 'utf8'), /读取超时 75 秒/)
    return { origin, backend, nginx_image: nginxImage, nginx_version: (await docker(['exec', proxy, 'nginx', '-v'])).text.trim(),
      postgres_version: (await psql('SHOW server_version')).text.trim(), loaded_config_sha256: hash(loaded.text),
      heartbeat: '服务默认 10s；写超时默认 5s（未覆盖）', tls: '临时本地证书；客户端显式信任此证书' }
  })
  await step('容器资产与 HTTP 内容逐字节一致、入口缓存及 404 边界', async () => {
    const files = []
    async function walk(directory, prefix = '') {
      for (const entry of await readdir(directory, { withFileTypes: true })) {
        const relative = prefix + entry.name
        if (entry.isDirectory()) await walk(join(directory, entry.name), relative + '/')
        // This tracked directory instruction is not a frontend build asset.
        else if (relative !== 'README.txt') files.push(relative)
      }
    }
    await walk(join(output, 'assets'))
    assert(files.includes('index.html') && files.includes('offline-sw.js') && files.some(file => file.endsWith('.css')) && files.some(file => /^assets\/.*\.js$/.test(file)))
    const manifest = []
    for (const file of files.sort()) {
      const data = await readFile(join(output, 'assets', file))
      for (const target of [backend, origin]) {
        const response = await request(`/app/${file}`, { target })
        assert.equal(response.status, 200, `${target}/app/${file}`)
        noStore(response, file)
        assert.deepEqual(response.data, data, `资产不一致：${file}`)
      }
      manifest.push({ file, bytes: data.length, sha256: hash(data) })
    }
    await writeFile(join(output, 'assets.json'), JSON.stringify(manifest, null, 2) + '\n')
    const missing = ['/v1/issue37-missing', '/v1/learning/issue37-missing', '/admin/issue37-missing',
      '/app/assets/issue37-missing.js', '/app/issue37-missing.css', '/app/settings/issue37-missing', '/app/api/issue37-missing', '/app/README.txt']
    for (const target of [backend, origin]) for (const path of missing) {
      const response = await request(path, { target })
      assert.equal(response.status, 404, `${target}${path}: ${response.data}`)
      assert(!response.data.includes(Buffer.from('type="module"')), '404 返回了 SPA')
    }
    for (const path of ['/app/', '/app/settings', '/app/progress', '/app/runs']) {
      const response = await request(path)
      assert.equal(response.status, 200)
      noStore(response, path)
      assert.deepEqual(response.data, await readFile(join(output, 'assets/index.html')))
    }
    return { assets: manifest.length, manifest_sha256: hash(JSON.stringify(manifest)), routes_404: missing, targets: [backend, origin] }
  })
  const actor = await paired('settings')
  const manager = await paired('memory')
  let runPath, watermark
  await step('敏感 API no-store、capability/schema 一致及正式导师运行', async () => {
    const ts = require('typescript')
    const source = ts.transpileModule(await readFile(join(web, 'src/api/settings.ts'), 'utf8'), { compilerOptions: { module: ts.ModuleKind.ESNext } }).outputText
      .replace(/from ['"]zod['"]/, `from ${JSON.stringify(pathToFileURL(require.resolve('zod')).href)}`)
    const validators = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
    const before = validators.capabilitiesSchema.parse(await api('/v1/capabilities', actor))
    assert.equal(before.schema_version, 1)
    assert.equal(before.web_mentor.available, false)
    assert.equal(before.rendering.available, true)
    assert.equal(before.persistence.available, true)
    assert.equal(before.research.available, false)
    const settings = validators.settingsSchema.parse(await api('/v1/settings', actor))
    await api('/v1/settings', actor, 'PUT', { expected_revision: settings.revision, target: 'mentor',
      connection: { enabled: true, provider: 'openai_compatible', endpoint: `http://127.0.0.1:${port + 2}/v1`, model: 'release-sse-fixture', auth_mode: 'none' },
      mentor_uses_teaching: false, limits: { ...settings.limits, output_tokens: 64 } })
    const after = validators.capabilitiesSchema.parse(await api('/v1/capabilities', actor))
    assert.equal(after.web_mentor.available, true)
    assert.equal(after.research.available, false)
    const space = await api('/v1/learning-spaces', actor, 'POST', { operation_id: randomUUID(), expected_version: 0, name: '发布代理验收', description: '', status: 'active' })
    actor.space = space.id
    const goal = randomUUID()
    await api('/v1/learning/goals', actor, 'POST', { operation_id: randomUUID(), payload_schema_version: 1, aggregate_type: 'goal', aggregate_id: goal,
      expected_version: 0, text: '发布代理验收目标', source: 'web' }, 201)
    const receipt = await api(`/v1/learning/goals/${goal}/runs`, actor, 'POST', { operation_id: randomUUID(), session_id: randomUUID(),
      expected_version: 1, prompt: '结构化询问验收方向', save: false, request_budget: 2, token_budget: 10000 }, 202)
    runPath = `/v1/learning/runs/${receipt.run_id}`
    let snapshot
    await until(async () => { snapshot = await api(runPath, actor); return snapshot.status === 'waiting_input' }, 30000, '导师没有进入等待状态')
    watermark = snapshot.watermark
    const paths = ['/v1/web/session', '/v1/settings', '/v1/capabilities', '/v1/learning-spaces', '/v1/learning/goals', '/v1/learning/runs', runPath]
    for (const target of [backend, origin]) for (const path of paths) {
      const response = await request(path, { actor, target })
      assert.equal(response.status, 200, `${path}: ${response.data}`)
      noStore(response, path)
    }
    await api('/v1/devices', manager)
    const invalid = await request('/v1/settings', { actor, method: 'PUT', body: { expected_revision: settings.revision, target: 'mentor' } })
    assert.equal(invalid.status, 409)
    assert.equal(invalid.json().error.code, 'settings_conflict')
    noStore(invalid, '过期设置版本')
    assert.equal(report.modelCalls(), 1)
    assert(actor.capabilities.runs && actor.capabilities.spaces && actor.capabilities.goals)
    return { schema_version: after.schema_version, capability_before: before, capability_after: after, paths_no_store: paths,
      error_no_store: invalid.status, run: receipt.run_id, state: snapshot.status, fixture_calls: report.modelCalls() }
  })
  await step('管理/内部端点在真实代理保持隔离', async () => {
    const paths = ['/admin', '/admin/', '/admin/api/pairing-codes', '/admin/api/settings', '/internal/privacy/migrations/acquire',
      '/internal/nocturne/capabilities', '/mcp', '/livez', '/readyz', '/v1/pairings/exchange', '/v1/knowledge/imports', '/v1/model/capabilities']
    const before = requestsSeen.length
    for (const actorOption of [undefined, actor]) for (const path of paths) {
      const response = await request(path, { actor: actorOption })
      assert.equal(response.status, 404, path)
      assert(!response.data.includes(Buffer.from('type="module"')))
    }
    assert.equal(requestsSeen.length, before, '隔离路径到达了上游')
    return { requests: paths.length * 2, paths, upstream_requests: 0 }
  })
  const eventsPath = `${runPath}/events?after=${watermark}`
  await step('真实导师 SSE 经 TLS Nginx 即时到达且活跃连接超过 75s；静默上游 75s 关闭', async () => {
    const active = await stream(eventsPath, actor, { name: 'timing' })
    assert.equal(active.status, 200)
    noStore(active, eventsPath)
    assert.match(active.headers['content-type'], /^text\/event-stream/)
    const direct = await stream(eventsPath, actor, { name: 'direct', target: backend })
    assert.equal(direct.headers['x-accel-buffering'], 'no')
    direct.close()
    const idle = await stream('/v1/learning/runs/idle-probe/events?after=0', undefined, { target: `https://localhost:${port + 5}` })
    assert.equal(idle.status, 200)
    await until(() => active.heartbeats.length >= 9 || active.ended, 90000, '未收到九次默认十秒心跳')
    assert.equal(active.ended, false, '活跃 SSE 被代理关闭')
    await until(() => idle.ended, 10000, '静默 SSE 未按 75 秒超时')
    const upstream = observations.get('timing')
    assert.equal(upstream.heartbeats.length, active.heartbeats.length)
    const samples = active.heartbeats.map((arrival, i) => ({ ordinal: i + 1,
      upstream_ms: Number((upstream.heartbeats[i] - active.started).toFixed(3)),
      client_ms: Number((arrival - active.started).toFixed(3)),
      proxy_delay_ms: Number((arrival - upstream.heartbeats[i]).toFixed(3)) }))
    const gaps = active.heartbeats.slice(1).map((value, i) => Math.round(value - active.heartbeats[i]))
    assert(samples.every(sample => sample.proxy_delay_ms >= 0 && sample.proxy_delay_ms < 1000), '心跳被缓冲超过一秒')
    assert(gaps.every(gap => gap >= 9500 && gap <= 11000), `心跳间隔不符：${gaps}`)
    assert(samples.at(-1).client_ms > 75000)
    const idleMs = Math.round(idle.endedAt - idle.heartbeats[0])
    assert(idleMs >= 74000 && idleMs <= 79000, `静默超时不符：${idleMs}`)
    active.close(); idle.close()
    await until(async () => await backendConnections() === 0, 5000, '长流关闭后后端连接未回收')
    assert.equal(report.modelCalls(), 1, 'SSE 观察触发了额外模型调用')
    return { samples, gaps_ms: gaps, max_proxy_delay_ms: Math.max(...samples.map(item => item.proxy_delay_ms)),
      idle_timeout_ms: idleMs, active_elapsed_ms: samples.at(-1).client_ms,
      direct_x_accel_buffering: direct.headers['x-accel-buffering'], fixture_calls: report.modelCalls() }
  })
  await step('客户端断开后服务端 socket 与每设备订阅额度回收', async () => {
    const four = []
    for (let i = 0; i < 4; i++) {
      const item = await stream(eventsPath, actor, { name: `capacity-${i}` })
      assert.equal(item.status, 200)
      four.push(item)
    }
    assert.equal(await backendConnections(), 4)
    const refused = await request(eventsPath, { actor })
    assert.equal(refused.status, 429)
    noStore(refused, '第五条 SSE')
    const started = performance.now()
    four[0].close()
    await until(async () => await backendConnections() === 3, 5000, '断开后后端 socket 没有回收')
    const recoveredMs = Math.round(performance.now() - started)
    const replacement = await stream(eventsPath, actor, { name: 'replacement' })
    assert.equal(replacement.status, 200, '断开后订阅额度未释放')
    for (const item of [...four, replacement]) item.close()
    await until(async () => await backendConnections() === 0, 5000, '全部断开后后端 socket 没有回收')
    return { four_streams_backend_connections: 4, fifth_status: refused.status, replacement_status: replacement.status,
      disconnect_recovery_ms: recoveredMs, final_backend_connections: 0 }
  })
  await step('正式设备撤销后已有 SSE、后端 socket 及读事务回收', async () => {
    const open = []
    for (let i = 0; i < 4; i++) {
      const item = await stream(eventsPath, actor, { name: `revocation-${i}` })
      assert.equal(item.status, 200)
      open.push(item)
    }
    const started = performance.now()
    const revoke = await request(`/v1/devices/${actor.device}`, { actor: manager, method: 'DELETE' })
    assert.equal(revoke.status, 204, `正式撤销失败：${revoke.data}`)
    noStore(revoke, '设备撤销')
    await until(() => open.every(item => item.ended), 5000, '撤销后客户端 SSE 未关闭')
    await until(async () => await backendConnections() === 0, 5000, '撤销后后端连接未回收')
    const recoveredMs = Math.round(performance.now() - started)
    for (const item of open) item.close()
    for (const path of [eventsPath, '/v1/web/session', runPath]) {
      const response = await request(path, { actor })
      assert.equal(response.status, 401, '被撤销设备仍可读取')
      noStore(response, path)
    }
    const transactions = Number((await psql(`SELECT count(*) FROM pg_stat_activity WHERE datname='${database}' AND state LIKE 'idle in transaction%'`)).text.trim())
    assert.equal(transactions, 0)
    return { revoke_status: revoke.status, streams_closed: open.length, revocation_recovery_ms: recoveredMs,
      final_backend_connections: 0, idle_read_transactions: transactions, revoked_status: 401 }
  })
  report.status = 'passed'
} catch (error) {
  report.status = 'failed'
  report.reason = error.message
  console.error(error.message)
  process.exitCode = 1
} finally {
  // Evidence contains no persistent credentials. Capture logs before removing
  // our exact names; never use prune, Compose down, or remove the PostgreSQL container.
  const cleanupErrors = []
  for (const item of streams) item.close()
  for (const name of containers.reverse()) {
    try {
      if (name === service || name === proxy || name === probeProxy) await docker(['logs', name], { allowFailure: true })
      await docker(['rm', '-f', name])
    } catch (error) { cleanupErrors.push(error.message) }
  }
  for (const socket of sockets) socket.destroy()
  for (const server of localServers) await new Promise(resolve => server.close(resolve))
  if (databaseCreated) {
    try { await psql(`DROP DATABASE ${database} WITH (FORCE)`) }
    catch (error) { cleanupErrors.push(error.message) }
  }
  for (const file of ['server.env', 'certs/learn.example.key']) await rm(join(output, file), { force: true })
  report.fixture_calls = report.modelCalls?.()
  delete report.modelCalls
  report.cleanup = { status: cleanupErrors.length ? 'failed' : 'passed', errors: cleanupErrors,
    postgres_preserved: (await docker(['inspect', '--format', '{{.State.Running}}', postgres], { allowFailure: true })).text.trim() === 'true' }
  if (cleanupErrors.length || !report.cleanup.postgres_preserved) { report.status = 'failed'; process.exitCode = 1 }
  report.finished = new Date().toISOString()
  await writeFile(join(output, 'report.json'), JSON.stringify(report, null, 2) + '\n', { mode: 0o600 })
  console.log(`结果：${report.status}；证据：${join(output, 'report.json')}`)
}
