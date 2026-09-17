import { spawn } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { createServer } from 'node:http'
import { workspaceModel } from './workspace-model.mjs'
if (!process.env.TEST_DATABASE_URL) throw new Error('浏览器验收必须配置独立 TEST_DATABASE_URL')
let child
let restarting = false
const settingsFile = join(mkdtempSync(join(tmpdir(), 'edu-web-settings-test-')), 'settings.json')
const mentorKeyFile = join(mkdtempSync(join(tmpdir(), 'edu-web-key-test-')), 'mentor.key')
const importStaging = mkdtempSync(join(tmpdir(), 'edu-web-import-test-'))
writeFileSync(mentorKeyFile, randomBytes(32), { mode: 0o600 })
if (process.env.WEB_MENTOR_FIXTURE === '1' || process.env.WEB_WORKSPACE_FIXTURE === '1') {
  let calls = 0
  const fixture = createServer(async (request, response) => {
    if (request.url === '/stats') {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ calls }))
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
    const input = messages.findLast((message) => message.role === 'user')?.content ?? ''
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
      MENTOR_KEY_FILE: mentorKeyFile,
      IMPORT_JOB_STAGING_DIR: importStaging,
      MODEL_ENDPOINT_ALLOWLIST: '["http://127.0.0.1:1/v1","http://127.0.0.1:32930/v1"]',
      DEVICE_RATE_LIMIT_PER_MINUTE: '10000',
      PAIRING_RATE_LIMIT_PER_MINUTE: '1000',
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
