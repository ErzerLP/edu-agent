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
  const markdownAnswer = String.raw`# Markdown 学习回答

这是 **重点** 与 *解释*，行内公式 \(x^2\)。

- 第一项 [1]
- 第二项

1. 有序步骤

> 资料需要核验。

\[x^2 + y^2 = 1\]

[资料链接](https://example.org/doc)

<img src=x onerror=alert(1)>
` + '\n```ts\nconst x = "$x$ [1]"\n```'
  let calls = 0
  let historyRequests = 0
  let studioCalls = 0
  let studioSectionCalls = 0
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
    if (process.env.WEB_SEARCH_BROWSER_FIXTURE === '1' && payload.messages?.[0]?.content?.includes('公开检索判断')) {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ message: { role: 'assistant', content: JSON.stringify({ search_needed: true }) }, finish_reason: 'stop' }], usage: { prompt_tokens: 40, completion_tokens: 10, total_tokens: 50 } }))
      return
    }
    if (process.env.WEB_SEARCH_BROWSER_FIXTURE === '1' && payload.messages?.[0]?.content?.includes('公开资料检索阶段')) {
      const last = payload.messages.at(-1)
      const message = { role: 'assistant' }
      if (last.role === 'user') message.tool_calls = [{ id: 'search-web', type: 'function', function: { name: 'search_web', arguments: JSON.stringify({ query: '公开网页新版本', count: 1 }) } }]
      else if (last.tool_call_id === 'search-web') message.tool_calls = [{ id: 'read-web', type: 'function', function: { name: 'read_web', arguments: JSON.stringify({ resource_id: JSON.parse(last.content).items[0].resource_id }) } }]
      else message.content = '公开网页已读取。'
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ message, finish_reason: message.tool_calls ? 'tool_calls' : 'stop' }], usage: { prompt_tokens: 60, completion_tokens: 20, total_tokens: 80 } }))
      return
    }
    if (process.env.WEB_SEARCH_BROWSER_FIXTURE === '1' && payload.messages?.[0]?.content?.includes('区分学习区资料与网络来源')) {
      const material = JSON.parse(payload.messages.find(m => m.role === 'user' && m.content.includes('web_documents')).content)
      const document = material.web_documents[0]
      const result = { answer: '新版本支持公开网页检索。[1]', citations: [{ origin: 'web', document_id: document.document_id, revision_id: document.revision_id, fragment_id: document.fragments[0].id, quote: '新版本支持公开网页检索。' }] }
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ message: { role: 'assistant', content: JSON.stringify(result) }, finish_reason: 'stop' }], usage: { prompt_tokens: 120, completion_tokens: 60, total_tokens: 180 } }))
      return
    }
    if (payload.messages?.[0]?.content?.includes('你在 Studio 规划学习大纲')) {
      const material = JSON.parse(payload.messages[1].content)
      if (!material.directory?.[0]?.node_revision_id) { response.writeHead(400); response.end(); return }
      const instruction = material.instruction
      const result = {
        title: '偶数学习大纲', coverage: 'partial', uncovered: [], explanation: '已调整学习顺序与重点。',
        sections: [
          { id: 'definition', topic: instruction === '' ? '偶数的定义' : instruction.includes('简洁') ? '先理解整除' : '从整除认识偶数', objective: '理解整除', chapters: [1] },
          { id: 'examples', topic: '偶数的例子', objective: '辨认偶数', chapters: [1] },
        ],
      }
      if (material.kind === 'quiz') {
        result.title = '偶数题目规划'
        result.sections = (material.current_plan?.sections ?? Array.from({ length: material.question_count }, (_, index) => ({ id: `q${index + 1}`, topic: `偶数知识点${index + 1}` }))).map(question => ({
          id: question.id, topic: question.topic, objective: '用自己的话解释所选知识点', chapters: [1], question_type: 'explanation',
          difficulty: instruction.includes('更难') ? 'hard' : material.difficulty || 'medium',
        }))
      }
      if (material.focus?.includes('分段验收')) result.sections = Array.from({length:8},(_,index)=>({id:`s${index+1}`,topic:`偶数学习第${index+1}部分`,objective:'根据所选资料整理',chapters:[1]}))
      if (material.conversation_topics) result.sections = material.conversation_topics.map((topic, index) => ({ id: `topic${index + 1}`, topic: topic.title, objective: '复习所选话题', chapters: [1], topic_ids: [topic.id] }))
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ index: 0, message: { role: 'assistant', content: JSON.stringify(result) }, finish_reason: 'stop' }], usage: { prompt_tokens: 100, completion_tokens: 80, total_tokens: 180 } }))
      return
    }
    if (payload.messages?.[0]?.content?.startsWith('仅生成已确认题目规划的当前一道')) {
      const input = JSON.parse(payload.messages[1].content)
      await new Promise(resolve => setTimeout(resolve, 1800))
      const result = { title: input.topic, coverage: 'partial', questions: [{
        prompt: `${input.topic}：解释偶数并给出一个例子。`,
        rubric: [{ id: 'definition', criterion: '说明偶数能被 2 整除并举例' }],
        allowed_help: ['none', 'hint', 'scaffold', 'answer_revealed'], citations: [{ source: 1, fragment: 1 }],
      }] }
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ message: { role: 'assistant', content: JSON.stringify(result) }, finish_reason: 'stop' }], usage: { prompt_tokens: 100, completion_tokens: 100, total_tokens: 200 } }))
      return
    }
    if (payload.messages?.[0]?.content?.startsWith('仅生成已确认大纲的当前部分')) {
      const input=JSON.parse(payload.messages[1].content)
      studioSectionCalls++
      await new Promise(resolve=>setTimeout(resolve,studioSectionCalls===4?8000:600))
      const result={title:input.topic,coverage:'partial',claims:[{text:'偶数可被 2 整除。',citations:[{source:1,fragment:1}]}]}
      response.setHeader('Content-Type','application/json')
      response.end(JSON.stringify({choices:[{message:{role:'assistant',content:JSON.stringify(result)},finish_reason:'stop'}],usage:{prompt_tokens:100,completion_tokens:60,total_tokens:160}}))
      return
    }
    if (payload.messages?.[0]?.content?.startsWith('所有已确认部分已完成') || payload.messages?.[0]?.content?.startsWith('所有已确认题目已完成')) {
      const input=JSON.parse(payload.messages[1].content)
      const result={overview:[input.allowed_claims[0].id],transitions:[],uncovered:[]}
      if (payload.messages[0].content.startsWith('所有已确认题目已完成')) result.transitions = input.allowed_claims.map(claim => claim.id)
      response.setHeader('Content-Type','application/json')
      response.end(JSON.stringify({choices:[{message:{role:'assistant',content:JSON.stringify(result)},finish_reason:'stop'}],usage:{prompt_tokens:100,completion_tokens:30,total_tokens:130}}))
      return
    }
    if (payload.messages?.[0]?.content?.includes('本轮已达到用户设置的工具调用上限')) {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({choices:[{message:{role:'assistant',content:'已读取目标，还需整理例子。'},finish_reason:'stop'}],usage:{prompt_tokens:10,completion_tokens:5,total_tokens:15}}))
      return
    }
    if (payload.messages?.[0]?.content?.includes('整理对话的只读话题索引')) {
      const batch = JSON.parse(payload.messages[1].content)
      const anchor_ids = batch.messages.map(message => message.id)
      const even = batch.messages.some(message => message.content?.includes('偶数'))
      const topics = (even ? ['偶数的定义', '偶数的例子'] : ['TCP', '子网']).map(title => ({
        id: batch.candidates.find(topic => topic.title === title)?.id ?? '', title, anchor_ids,
        points: [{ text: even ? title : '比较连接与地址的作用', anchor_ids }],
        observations: [{ state: 'question_open', text: '继续比较两个概念', evidence: 'tutor_explanation', anchor_ids }],
      }))
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ choices: [{ message: { role: 'assistant', content: JSON.stringify({ topics }) }, finish_reason: 'stop' }], usage: { prompt_tokens: 100, completion_tokens: 80, total_tokens: 180 } }))
      return
    }
    if (payload.messages?.[0]?.content?.includes('你是学习导师，围绕用户已确认的学习方向')) {
      const material = JSON.parse(payload.messages[1].content)
      const source = material.sources[0], fragment = source?.fragments[0]
      const result = fragment ? { answer: `围绕${material.direction}，已核对${source.collection_name}：${fragment.text} [1]`, citations: [{ source: 1, fragment: 1 }] } : { answer: `围绕${material.direction}，我们可以先理解核心概念。`, citations: [] }
      result.exercise = null
      const question = payload.messages.findLast(message => message.role === 'user')?.content ?? ''
      if (question.includes('UX 慢速思考')) await new Promise(resolve => setTimeout(resolve, 30000))
      if (question.includes('Markdown 渲染验收')) result.answer = markdownAnswer
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
    if (input.includes('Markdown 渲染验收')) delta.content = markdownAnswer
    let finish = 'stop'
    if (input.includes('工具上限验收')) {
      delta.content = ''
      delta.tool_calls = [{index:0,id:`browser-goal-${calls}`,type:'function',function:{name:'read_goal',arguments:'{}'}}]
      finish = 'tool_calls'
    }
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
      ...(process.env.WEB_SEARCH_BROWSER_FIXTURE === '1' ? { WEB_SEARCH_BROWSER_FIXTURE: '1' } : {}),
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
      AUTH_FAILURE_LIMIT_PER_MINUTE: '1000',
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
