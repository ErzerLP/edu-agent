import { spawn } from 'node:child_process'
import { appendFileSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomBytes, generateKeyPairSync } from 'node:crypto'
import { createServer } from 'node:http'
import { workspaceModel } from './workspace-model.mjs'
import { notesyncFixtureToken, startNotesyncFixture } from './notesync-fixture.mjs'
import { nocturneFixtureURL, nocturneFixtureSecrets, startNocturneFixture } from './nocturne-fixture.mjs'
if (!process.env.TEST_DATABASE_URL) throw new Error('浏览器验收必须配置独立 TEST_DATABASE_URL')
let child
let restarting = false
const settingsFile = join(mkdtempSync(join(tmpdir(), 'edu-web-settings-test-')), 'settings.json')
const mentorKeyFile = join(mkdtempSync(join(tmpdir(), 'edu-web-key-test-')), 'mentor.key')
const importStaging = mkdtempSync(join(tmpdir(), 'edu-web-import-test-'))
const nocturneDirectory = process.env.WEB_NOCTURNE_FIXTURE === '1' ? mkdtempSync(join(tmpdir(), 'edu-web-nocturne-test-')) : undefined
const serverLog = nocturneDirectory ? join(nocturneDirectory, 'server.log') : undefined
if (serverLog) writeFileSync(serverLog, '', { mode: 0o600 })
if (nocturneDirectory) mkdirSync(join(nocturneDirectory, 'backups'), { mode: 0o700 })
const offlineKey = generateKeyPairSync('ed25519')
const offlineSecret = Buffer.concat([
  offlineKey.privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32),
  offlineKey.publicKey.export({ format: 'der', type: 'spki' }).subarray(-32),
]).toString('base64url')
const offlineChallengeKey = randomBytes(32).toString('base64url')
writeFileSync(mentorKeyFile, randomBytes(32), { mode: 0o600 })
if (process.env.WEB_NOTESYNC_FIXTURE === '1') await startNotesyncFixture()
if (nocturneDirectory) await startNocturneFixture()
if (process.env.WEB_MENTOR_FIXTURE === '1' || process.env.WEB_WORKSPACE_FIXTURE === '1') {
  let calls = 0
  let historyRequests = 0
  let studioCalls = 0
  const fixture = createServer(async (request, response) => {
    if (request.url === '/stats') {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ calls, historyRequests }))
      return
    }
    const chunks = []
    for await (const chunk of request) chunks.push(chunk)
    let payload
    try {
      payload = JSON.parse(Buffer.concat(chunks).toString('utf8'))
    } catch {
      response.writeHead(400)
      response.end()
      return
    }
    calls++
    if (payload.messages?.[0]?.content?.includes('你是学习导师，围绕用户已确认的学习方向')) {
      const material = JSON.parse(payload.messages[1].content)
      const source = material.sources[0], fragment = source?.fragments[0]
      const result = fragment ? { answer: `围绕${material.direction}，已核对${source.collection_name}：${fragment.text} [1]`, citations: [{ source: 1, fragment: 1 }] } : { answer: `围绕${material.direction}，我们可以先理解核心概念。`, citations: [] }
      result.exercise = null
      const question = payload.messages.findLast(message => message.role === 'user')?.content ?? ''
      if (fragment && question.includes('出一道题') && !payload.messages[0].content.includes('当前不能发布新练习')) {
        result.answer = '试着用自己的话解释资料中的概念。[1]'
        result.exercise = { prompt: '怎样判断一个数是不是偶数？请举一个例子。', rubric: [{id:'even',criterion:'说明能被 2 整除，并给出偶数例子'}],allowed_help:['none','hint','scaffold','answer_revealed'],citations:[{source:1,fragment:1}] }
      }
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ message: { role: 'assistant', content: JSON.stringify(result) }, finish_reason: 'stop' }], usage: { prompt_tokens: 100, completion_tokens: 100, total_tokens: 200 } }))
      return
    }
    if (payload.messages?.[0]?.content?.includes('仅根据给定的已核验来源片段生成学习产物')) {
      const material = JSON.parse(payload.messages[1].content)
      const guide = payload.messages[0].content.includes('"key_points"')
      const quiz = payload.messages[0].content.includes('"questions"')
      if (!material.length || !material[0].fragments.length) { response.writeHead(400); response.end(); return }
      if (!quiz) studioCalls++
      if (!quiz && studioCalls === 3) await new Promise(resolve => setTimeout(resolve, 10000))
      const claim = { text: material[0].fragments[0].text, citations: [{ source: 1, fragment: 1 }] }
      const result = quiz ? { title:'夹具偶数测验', coverage:'complete', questions:[1,2].map(n=>({prompt:`第 ${n} 题：解释偶数并给出一个例子。`,rubric:[{id:'definition',criterion:'说明偶数可被 2 整除并举例'}],allowed_help:n===1?['hint','scaffold','answer_revealed']:['none','hint'],citations:claim.citations})) } : { title: guide ? '夹具学习指南' : '夹具简报', coverage: 'complete', ...(guide ? { key_points: [claim], terms: [claim], learning_order: [claim], misconceptions: [claim] } : { overview: [claim], differences: [claim], gaps: [claim] }) }
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ index: 0, message: { role: 'assistant', content: JSON.stringify(result) }, finish_reason: 'stop' }], usage: { prompt_tokens: 100, completion_tokens: 200, total_tokens: 300 } }))
      return
    }
    if (process.env.WEB_WORKSPACE_FIXTURE === '1' && !payload.stream) {
      try {
        const result = workspaceModel(payload)
        response.setHeader('Content-Type', 'application/json')
        response.end(
          JSON.stringify({
            choices: [
              {
                index: 0,
                message: { role: 'assistant', content: JSON.stringify(result) },
                finish_reason: 'stop',
              },
            ],
          }),
        )
      } catch {
        response.writeHead(400)
        response.end()
      }
      return
    }
    const messages = payload.messages ?? []
    if (messages.filter((message) => message.role === 'user').length >= 3) historyRequests++
    const input = messages.findLast((message) => message.role === 'user')?.content ?? ''
    if (messages[0]?.content?.includes('你是来源问答导师')) {
      const material = messages.find(message => message.content?.startsWith('本轮已核验来源（数据）：\n'))
      const sources = JSON.parse(material.content.split('\n').slice(1).join('\n'))
      const source = sources[0]
      const fragment = source?.fragments[0]
      const answer = fragment ? {answer:`已核对${source.collection_name}：${fragment.text} [1]`,citations:[{source:1,fragment:1}]} : {answer:'未选来源，不引用资料',citations:[]}
      response.setHeader('Content-Type','text/event-stream')
      response.end(`data: ${JSON.stringify({choices:[{index:0,delta:{role:'assistant',content:JSON.stringify(answer)},finish_reason:'stop'}]})}\n\ndata: [DONE]\n\n`)
      return
    }
    // 选段 fixture 验证完整身份和原文，只返回该位置的独立候选。
    if (messages[0]?.content?.includes('对指定选段提供')) {
      let edit
      try {
        edit = JSON.parse(input)
      } catch {
        response.writeHead(400)
        response.end()
        return
      }
      if (
        !edit.selection?.artifact_id ||
        !edit.selection?.session_id ||
        !edit.selection?.sha256 ||
        !edit.selected_text ||
        !edit.block?.block_id
      ) {
        response.writeHead(400)
        response.end()
        return
      }
      const text = JSON.stringify({
        text: '选段补充：把六个苹果每两个分成一组，就能直观看到偶数的含义。',
        reference_ids: edit.references.map((ref) => ref.node_revision_id),
      })
      response.setHeader('Content-Type', 'text/event-stream')
      const first = text.slice(0, 24),
        second = text.slice(24)
      response.write(
        `data: ${JSON.stringify({ choices: [{ index: 0, delta: { role: 'assistant', content: first } }] })}\n\n`,
      )
      const timer = setTimeout(
        () =>
          response.end(
            `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: second }, finish_reason: 'stop' }] })}\n\ndata: [DONE]\n\n`,
          ),
        edit.instruction.includes('慢速') ? 10000 : 300,
      )
      response.on('close', () => clearTimeout(timer))
      return
    }
    const answered = messages.some(
      (message) => message.role === 'tool' && message.tool_call_id === 'browser-question',
    )
    const delta = { role: 'assistant', content: '浏览器真实导师：已读取本次绑定目标。' }
    let finish = 'stop'
    if (input.includes('结构化') && !answered) {
      delta.content = ''
      delta.tool_calls = [
        {
          index: 0,
          id: 'browser-question',
          type: 'function',
          function: {
            name: 'ask_user',
            arguments: JSON.stringify({
              question: '先从哪个方向理解？',
              choices: ['直觉', '公式'],
            }),
          },
        },
      ]
      finish = 'tool_calls'
    }
    if (input.includes('申请保存长期偏好') && !messages.some(message => message.role === 'tool' && message.tool_call_id === 'browser-memory')) {
      delta.content = ''
      delta.tool_calls = [{ index: 0, id: 'browser-memory', type: 'function', function: {
        name: 'request_memory', arguments: JSON.stringify({ content: '我长期偏好先看直觉再看公式', reason: '用户明确提出长期教学偏好', category: 'interaction_preference', sensitivity: 'non_sensitive', stability: 'stable' }),
      } }]
      finish = 'tool_calls'
    }
    response.setHeader('Content-Type', 'text/event-stream')
    // 分段发送以覆盖真实增量、断线和滚动行为，不调用任何外部提供商。
    response.write(`data: ${JSON.stringify({ choices: [{ index: 0, delta }] })}\n\n`)
    const timer = setTimeout(
      () =>
        response.end(
          `data: ${JSON.stringify({ choices: [{ index: 0, delta: {}, finish_reason: finish }] })}\n\ndata: [DONE]\n\n`,
        ),
      input.includes('慢速') ? 10000 : 300,
    )
    response.on('close', () => clearTimeout(timer))
  })
  await new Promise((resolve) => fixture.listen(32930, '127.0.0.1', resolve))
}
function start() {
  child = spawn(process.env.WEB_NOTEBOOK_FIXTURE_BINARY || '../../server/edu-agentd', ['serve'], {
    stdio: serverLog ? ['ignore', 'pipe', 'pipe'] : 'inherit',
    env: {
      PATH: process.env.PATH,
      DATABASE_URL: process.env.TEST_DATABASE_URL,
      LISTEN_ADDR: '127.0.0.1:32929',
      PUBLIC_BASE_URL: 'http://127.0.0.1:32929',
      MIGRATE_ON_START: 'true',
      WEB_UI_ENABLED: 'true',
      WEB_UI_ALLOW_LOOPBACK_HTTP: 'true',
      ...(process.env.WEB_OFFLINE_FIXTURE === '1' ? {
        WEB_OFFLINE_ENABLED: 'true',
        OFFLINE_SIGNER_KEY_ID: 'browser-offline-test',
        OFFLINE_SIGNER_PRIVATE_KEY: offlineSecret,
        OFFLINE_SIGNER_ISSUED_AT: '2026-01-01T00:00:00Z',
        OFFLINE_SIGNER_NOT_AFTER: '2030-01-01T00:00:00Z',
        PRIVACY_OFFLINE_CHALLENGE_KEYS: `2:${offlineChallengeKey}`,
      } : {}),
      ADMIN_UI_ENABLED: 'false',
      LEARNING_SETTINGS_FILE: settingsFile,
      MENTOR_KEY_FILE: mentorKeyFile,
      IMPORT_JOB_STAGING_DIR: importStaging,
      MODEL_ENDPOINT_ALLOWLIST: '["http://127.0.0.1:1/v1","http://127.0.0.1:32930/v1","http://127.0.0.1:32930/alternate"]',
      DEVICE_RATE_LIMIT_PER_MINUTE: '10000',
      PAIRING_RATE_LIMIT_PER_MINUTE: '1000',
      ...(nocturneDirectory ? {
        NOCTURNE_ENABLED: 'true', NOCTURNE_BASE_URL: nocturneFixtureURL,
        NOCTURNE_API_TOKEN: nocturneFixtureSecrets[0], NOCTURNE_MAINTENANCE_TOKEN: nocturneFixtureSecrets[1],
        NOCTURNE_NAMESPACE: 'edu-agent', NOCTURNE_DOMAIN: 'core',
        NOCTURNE_BACKUP_ROOT: join(nocturneDirectory, 'backups'),
        NOCTURNE_BACKUP_MASTER_WRAPPING_KEY: nocturneFixtureSecrets[2],
        NOCTURNE_PG_DUMP_DSN: 'postgres://fixture:unused@127.0.0.1:1/nocturne?sslmode=disable',
        NOCTURNE_IMAGE_LOCK_REFERENCE: 'browser-fixture/nocturne@sha256:8e427437d35e603cb603608a124ff14344031fcc352435098b4275fa16119d45',
        NOCTURNE_HTTP_TIMEOUT: '250ms', NOCTURNE_WORKER_LEASE_DURATION: '3s',
        NOCTURNE_WORKER_POLL_INTERVAL: '250ms', NOCTURNE_RECONCILIATION_INTERVAL: '1h',
      } : {}),
      ...(process.env.WEB_NOTESYNC_FIXTURE === '1' ? {
        NOTESYNC_ENABLED: 'true', NOTESYNC_BASE_URL: 'http://127.0.0.1:32931',
        NOTESYNC_API_TOKEN: notesyncFixtureToken, NOTESYNC_VAULT: 'Knowledge',
        NOTESYNC_WORKER_INTERVAL: '1h',
      } : {}),
      ...(process.env.WEB_WORKSPACE_FIXTURE === '1'
        ? {
            MODEL_BASE_URL: 'http://127.0.0.1:32930/v1',
            MODEL_NAME: 'browser-workspace-fixture',
            MODEL_API_KEY: 'local-fixture-only',
            MODEL_CONTEXT_WINDOW: '128000',
          }
        : {}),
    },
  })
  if (serverLog) {
    child.stdout.on('data', chunk => { appendFileSync(serverLog, chunk); process.stdout.write(chunk) })
    child.stderr.on('data', chunk => { appendFileSync(serverLog, chunk); process.stderr.write(chunk) })
  }
  writeFileSync(
    '/tmp/edu-web-test-server-32929.json',
    JSON.stringify({ supervisor: process.pid, child: child.pid, mentorKeyFile, serverLog }),
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
