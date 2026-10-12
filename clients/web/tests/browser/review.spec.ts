import {
  test,
  expect,
  type APIRequestContext,
  type BrowserContext,
  type Page,
} from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'

const space = '00000000-0000-4000-8000-000000000001'
const origin = 'http://127.0.0.1:32929'
const databaseEnv = () => ({
  PATH: process.env.PATH,
  DATABASE_URL: process.env.TEST_DATABASE_URL,
  MIGRATE_ON_START: 'true',
})
const code = (profile = 'user') =>
  execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', profile], {
    encoding: 'utf8',
    env: databaseEnv(),
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
const op = (kind: string, id: string, version = 0) => ({
  operation_id: randomUUID(),
  payload_schema_version: 1,
  aggregate_type: kind,
  aggregate_id: id,
  expected_version: version,
})
const reviewURL = (spaceId?: string, goal?: string) =>
  `/app/progress?view=reviews&status=all${spaceId ? `&space=${spaceId}` : ''}${goal ? `&goal=${goal}` : ''}`

async function pair(page: Page, destination: string, profile = 'user') {
  await page.goto(destination)
  await page.getByLabel('配对码', { exact: true }).fill(code(profile))
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(
    page.getByRole('heading', {
      name: destination.includes('/progress') ? '学习进度与复习' : '你的学习区',
      exact: true,
    }),
  ).toBeVisible()
}

async function api(request: APIRequestContext) {
  const response = await request.post('/v1/pairings/exchange', {
    data: { code: code('references'), display_name: '复习历史初始化' },
  })
  expect(response.ok(), await response.text()).toBe(true)
  const identity = await response.json()
  const call = async (spaceId: string, path: string, body?: unknown, collection?: string) => {
    const result = await request.fetch(path, {
      method: body ? 'POST' : 'GET',
      data: body,
      headers: {
        Authorization: `Bearer ${identity.token}`,
        'X-Learning-Space-ID': spaceId,
        ...(collection ? { 'X-Knowledge-Collection-ID': collection } : {}),
      },
    })
    expect(result.ok(), `${path}: ${await result.text()}`).toBe(true)
    return result.json()
  }
  return { device: identity.device.id as string, token: identity.token as string, call }
}

type API = Awaited<ReturnType<typeof api>>

async function materials(client: API, spaceId: string, name: string) {
  const collection = randomUUID()
  await client.call(spaceId, '/v1/knowledge/collections', {
    id: collection,
    action: 'create',
    name,
    source: 'review-e2e',
  })
  const documents = [
    { path: 'even.md', markdown: '# 偶数原出处\n\n偶数可以被 2 整除，2 是偶数，3 是奇数。\n' },
  ]
  const imported = await client.call(
    spaceId,
    '/v1/knowledge/imports',
    {
      operation_id: randomUUID(),
      expected_parent_revision_id: null,
      source: '复习原出处',
      documents,
    },
    collection,
  )
  const revision = imported.revision.revision_id as string
  const scope = await client.call(spaceId, '/v1/knowledge/scopes', {
    id: randomUUID(),
    entries: [{ collection_id: collection, revision_id: revision }],
  })
  const id = randomUUID()
  let goal = (
    await client.call(spaceId, '/v1/learning/goals', {
      ...op('goal', id),
      text: `${name}：理解偶数并说明依据`,
      source: '复习端到端验收',
      details: { name, scope_snapshot_id: scope.id },
    })
  ).result
  goal = (
    await client.call(spaceId, '/v1/learning/goals', {
      ...op('goal', id, goal.revision),
      previous_revision_id: goal.goal_revision_id,
      text: goal.text,
      source: '复习端到端验收',
      action: 'start',
    })
  ).result
  return { collection, revision, scope: scope.id as string, goal, documents }
}

async function dynamicSession(client: API, spaceId: string, name: string) {
  const source = await materials(client, spaceId, name)
  const referenceRequest = {
    operation_id: randomUUID(),
    expected_goal_version: source.goal.revision,
    selection: {
      session_id: '',
      entries: [
        { collection_id: source.collection, revision_id: source.revision, role: 'restrict' },
      ],
    },
  }
  const preview = await client.call(
    spaceId,
    `/v1/learning/goals/${source.goal.goal_id}/references/previews`,
    referenceRequest,
  )
  const references = await client.call(
    spaceId,
    `/v1/learning/goals/${source.goal.goal_id}/references/confirm`,
    { request: referenceRequest, receipt: preview.receipt, confirm_scope: true },
  )
  const run = await client.call(spaceId, `/v1/learning/goals/${source.goal.goal_id}/runs`, {
    operation_id: randomUUID(),
    session_id: randomUUID(),
    expected_version: source.goal.revision,
    prompt: '偶数',
    save: true,
    request_budget: 8,
    token_budget: 50000,
    research: {
      topic: '偶数',
      external_consent: true,
      auto_adopt: true,
      policy: { mode: 'supplement', domains: [] },
    },
    start_learning: {
      new_session: true,
      model_consent: true,
      reference_context_id: references.context_id,
    },
  })
  await expect
    .poll(async () => (await client.call(spaceId, `/v1/learning/runs/${run.run_id}`)).status, {
      timeout: 45_000,
    })
    .toBe('succeeded')
  const started = await client.call(spaceId, `/v1/learning/runs/${run.run_id}`)
  const id = started.start_learning.result.session_id as string
  const knowledgeContext = (
    await client.call(spaceId, `/v1/tutoring/sessions/${id}/knowledge-context`)
  ).knowledge_context
  expect(knowledgeContext.concepts[0].support).toHaveLength(1)
  expect(knowledgeContext.references.entries).toEqual(referenceRequest.selection.entries)
  return { ...source, id, spaceId, knowledgeContext }
}

function history(client: API, spaceId: string, id: string, completed = false) {
  if (!process.env.WEB_REVIEW_FIXTURE) throw new Error('需要独立 Go 夹具 WEB_REVIEW_FIXTURE')
  const supervisor = JSON.parse(readFileSync('/tmp/edu-web-test-server-32929.json', 'utf8'))
  return JSON.parse(
    execFileSync(
      process.env.WEB_REVIEW_FIXTURE,
      [
        '--device',
        client.device,
        '--space',
        spaceId,
        '--session',
        id,
        ...(completed ? ['--completed'] : []),
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          WEB_REVIEW_TOKEN: client.token,
          WEB_REVIEW_KEY_FILE: supervisor.mentorKeyFile,
        },
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    ),
  )
}

async function configureMentor(page: Page, context: BrowserContext) {
  await pair(page, '/app/', 'settings')
  const identity = await (await page.request.get('/v1/web/session')).json()
  const headers = {
    Origin: origin,
    'X-CSRF-Token': identity.csrf_token,
    'X-Web-Principal-ID': identity.device.id,
    'X-Web-Generation': String(identity.generation),
  }
  const settings = await (await page.request.get('/v1/settings', { headers })).json()
  const configured = await page.request.put('/v1/settings', {
    headers,
    data: {
      expected_revision: settings.revision,
      target: 'mentor',
      connection: {
        enabled: true,
        provider: 'openai_compatible',
        endpoint: 'http://127.0.0.1:32930/v1',
        model: 'browser-workspace-fixture',
        auth_mode: 'none',
      },
    },
  })
  expect(configured.ok(), await configured.text()).toBe(true)
  await context.clearCookies()
}

function cli() {
  if (!process.env.EDU_AGENT_INTEGRATION_CLI)
    throw new Error('需要 make cli-build 产出的 EDU_AGENT_INTEGRATION_CLI')
  const env = {
    ...process.env,
    XDG_CONFIG_HOME: mkdtempSync(join(tmpdir(), 'edu-review-cli-')),
    TERM: 'dumb',
    NO_COLOR: '1',
  }
  const run = (args: string[], input = '') =>
    execFileSync(process.env.EDU_AGENT_INTEGRATION_CLI!, args, {
      input,
      encoding: 'utf8',
      env,
      stdio: ['pipe', 'pipe', 'pipe'],
    })
  run(['pair', '--server', origin, '--name', '复习双端读取'], code() + '\n')
  return { run, json: (args: string[]) => JSON.parse(run(args)) }
}

async function answerReview(page: Page) {
  const proposed = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' && response.url().endsWith('/v1/tutoring/proposals'),
  )
  await page.getByRole('button', { name: '生成本任务复习活动', exact: true }).click()
  const proposal = await proposed
  const input = proposal.request().postDataJSON()
  expect(
    proposal.ok(),
    JSON.stringify({
      response: await proposal.text(),
      focus: input.focus_node_revision_id,
      nodes: input.node_revision_ids,
      hits: input.input.retrieval.hits.map((hit: Record<string, unknown>) => ({
        node: hit.node_revision_id,
        range: hit.range,
        hash: hit.slice_sha256,
      })),
    }),
  ).toBe(true)
  await expect(page.getByRole('heading', { name: '活动已就绪' })).toBeVisible()
  await page.getByRole('button', { name: '开始当前活动', exact: true }).click()
  await expect(page.getByLabel('我的正式答案')).toBeEnabled()
  await page.getByLabel('我的正式答案').fill('A')
  await page.getByRole('button', { name: '提交正式答案', exact: true }).click()
  await page.getByRole('button', { name: '获取教学反馈', exact: true }).click()
  await expect(page.getByRole('heading', { name: '本次反馈', exact: true })).toBeVisible()
}

test('真实到期复习在两区三目标保留原会话并由生产 CLI 读回结果', async ({
  page,
  context,
  request,
}) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地确定性教学模型')
  test.setTimeout(180_000)
  page.setDefaultTimeout(15_000)
  await configureMentor(page, context)
  const client = await api(request)
  const other = await client.call(space, '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '复习验收另一区',
    description: '',
    status: 'active',
  })
  const sessions = [
    await dynamicSession(client, space, '复习验收目标一'),
    await dynamicSession(client, space, '复习验收目标二'),
    await dynamicSession(client, other.id, '复习验收跨区目标'),
  ]
  const sources = sessions.map((s) => history(client, s.spaceId, s.id))
  const before = await client.call(space, '/v1/learning/reviews?global=true&status=all')
  expect(before.total).toBe(3)
  const terminal = cli()
  expect(terminal.json(['reviews', '--global', '--status', 'all', '--json']).items).toEqual(
    before.items,
  )
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  await pair(page, reviewURL())
  for (const [index, session] of sessions.entries()) {
    const review = before.items.find((r: { session_id: string }) => r.session_id === session.id)
    expect(review).toMatchObject({
      learning_space_id: session.spaceId,
      goal_id: session.goal.goal_id,
      evidence_id: sources[index].evidence[0].evidence_id,
      attempt_id: sources[index].attempt.attempt_id,
      startable: true,
    })
    expect(Date.parse(review.due_at)).toBeLessThan(Date.now())
    expect(Date.parse(review.due_at) - Date.parse(sources[index].evidence[0].received_at)).toBe(
      24 * 60 * 60 * 1000,
    )
    const item = page.locator(`[data-review-task="${review.task_id}"]`)
    await expect(item.getByRole('link', { name: '继续原任务复习', exact: true })).toHaveAttribute(
      'href',
      `/app/spaces/${session.spaceId}/learn/${session.id}`,
    )
    await expect(item.getByRole('link', { name: '返回原目标', exact: true })).toHaveAttribute(
      'href',
      `/app/spaces/${session.spaceId}/goals/${session.goal.goal_id}`,
    )
  }
  const target = sessions[2]
  await page.getByRole('combobox', { name: '范围', exact: true }).selectOption(target.spaceId)
  await expect(page.locator('[data-review-task]')).toHaveCount(1)
  await page.getByRole('link', { name: '继续原任务复习', exact: true }).click()
  await expect(page).toHaveURL(`/app/spaces/${target.spaceId}/learn/${target.id}`)
  await answerReview(page)
  const saved = await client.call(target.spaceId, `/v1/tutoring/sessions/${target.id}`)
  const result = await client.call(
    target.spaceId,
    `/v1/learning/attempts/${saved.work_item.attempt.attempt_id}/feedback`,
  )
  expect(result.evidence).toHaveLength(1)
  expect(result.knowledge_context_revision_id).toBe(target.knowledgeContext.id)
  expect(result.evidence[0]).toMatchObject({
    kind: 'review_recall',
    outcome: 'pass',
    help: 'none',
    attempt_id: saved.work_item.attempt.attempt_id,
  })
  expect(result.activity.activity_id).not.toBe(sources[2].activity.activity_id)
  expect(result.activity).toMatchObject({
    session_id: target.id,
    goal_revision_id: sources[2].activity.goal_revision_id,
    knowledge_revision_id: sources[2].activity.knowledge_revision_id,
    route_revision_id: sources[2].activity.route_revision_id,
    target_node_revision_id: sources[2].activity.target_node_revision_id,
    review: true,
  })
  const cliSaved = terminal.json([
    'learn',
    'show',
    '--space',
    target.spaceId,
    '--session',
    target.id,
  ])
  expect(cliSaved.work_item.attempt).toEqual(saved.work_item.attempt)
  expect(cliSaved.work_item.assessment_decision).toEqual(saved.work_item.assessment_decision)
  expect(
    terminal.run(['assessment', 'show', '--space', target.spaceId, '--session', target.id]),
  ).toContain(result.evidence[0].evidence_id)
  const progress = await client.call(
    target.spaceId,
    `/v1/learning/progress?goal_id=${target.goal.goal_id}&status=all`,
  )
  expect(
    terminal.json([
      'progress',
      '--space',
      target.spaceId,
      '--goal-id',
      target.goal.goal_id,
      '--status',
      'all',
      '--json',
    ]).items,
  ).toEqual(progress.items)
  expect(progress.items[0].evidence_count).toBe(2)
  for (const source of [sources[2], result]) {
    expect(progress.items[0].evidence_sources).toContainEqual(
      expect.objectContaining({
        evidence_id: source.evidence[0].evidence_id,
        attempt_id: source.attempt.attempt_id,
        knowledge_revision_id: source.activity.knowledge_revision_id,
      }),
    )
  }
  const original = await client.call(
    target.spaceId,
    `/v1/learning/attempts/${sources[2].attempt.attempt_id}/feedback`,
  )
  expect(original.attempt).toEqual(sources[2].attempt)
  expect(original.evidence).toEqual(sources[2].evidence)
  const scheduled = progress.items[0].reviews[0]
  expect(scheduled).toMatchObject({
    task_id: before.items.find((r: { session_id: string }) => r.session_id === target.id).task_id,
    evidence_id: result.evidence[0].evidence_id,
    attempt_id: result.attempt.attempt_id,
  })
  expect(Date.parse(scheduled.due_at)).toBeGreaterThan(Date.now())
  await page.goto(reviewURL(target.spaceId, target.goal.goal_id))
  await expect(page.getByText('当前筛选和截止时间内没有到期复习。')).toBeVisible()
  expect(
    terminal.json([
      'reviews',
      '--space',
      target.spaceId,
      '--goal-id',
      target.goal.goal_id,
      '--status',
      'all',
      '--json',
    ]).total,
  ).toBe(0)
  expect(errors).toEqual([])
})

test('动态 context 真实复习承载恢复、重复进入、CLI 出处与隐私清除', async ({
  page,
  context,
  request,
}) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地确定性教学模型')
  test.setTimeout(240_000)
  page.setDefaultTimeout(15_000)
  await configureMentor(page, context)
  const client = await api(request)
  const source = await dynamicSession(client, space, '动态 context 复习验收')
  const id = source.id
  const originalContext = source.knowledgeContext
  const historical = history(client, space, id, true)
  expect(historical.knowledge_context_revision_id).toBe(originalContext.id)
  const originalSession = await client.call(space, `/v1/tutoring/sessions/${id}`)
  expect(originalSession.session.state).toBe('Completed')
  // 当前集合推进到新版本，旧 context 和复习仍必须沿用原指针。
  const exported = await client.call(
    space,
    `/v1/knowledge/revisions/${source.revision}/export`,
    undefined,
    source.collection,
  )
  const changed = await client.call(
    space,
    '/v1/knowledge/imports',
    {
      operation_id: randomUUID(),
      expected_parent_revision_id: source.revision,
      source: '复习后资料新版',
      documents: [
        ...exported.documents.map((document: { path: string; markdown: string }) => ({
          path: document.path,
          markdown: document.markdown,
        })),
        {
          path: 'later.md',
          markdown: '# 新版补充说明\n\n这个版本增加了另一份材料，冻结的旧资料继续保留。\n',
        },
      ],
    },
    source.collection,
  )
  expect(changed.revision.revision_id).not.toBe(source.revision)
  const reviews = await client.call(
    space,
    `/v1/learning/reviews?goal_id=${source.goal.goal_id}&status=all`,
  )
  expect(reviews.total).toBe(1)
  const review = reviews.items[0]
  expect(review).toMatchObject({
    session_id: id,
    unavailable_reason: 'original_session_not_at_review_node',
    evidence_id: historical.evidence[0].evidence_id,
    attempt_id: historical.attempt.attempt_id,
    knowledge_revision_id: originalContext.scope_snapshot_id,
  })
  expect(Boolean(review.startable)).toBe(false)
  const terminal = cli()
  expect(
    terminal.json([
      'reviews',
      '--space',
      space,
      '--goal-id',
      source.goal.goal_id,
      '--status',
      'all',
      '--json',
    ]).items,
  ).toEqual(reviews.items)
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  const destination = reviewURL(space, source.goal.goal_id)
  await pair(page, destination)
  const item = page.locator(`[data-review-task="${review.task_id}"]`)
  await expect(item).toContainText('原会话已完成')
  await expect(item.getByRole('link', { name: '原答案与证据' })).toHaveAttribute(
    'href',
    `/app/spaces/${space}/feedback/${historical.attempt.attempt_id}`,
  )
  const sessionsBefore = await client.call(
    space,
    `/v1/tutoring/sessions?goal_id=${source.goal.goal_id}`,
  )
  await expect(item.getByRole('button', { name: '新建复习承载', exact: true })).toBeVisible()
  const bodies: Record<string, unknown>[] = []
  let carrier = ''
  let dropped!: () => void
  const responseDropped = new Promise<void>((resolve) => {
    dropped = resolve
  })
  await page.route('**/v1/tutoring/sessions', async (route) => {
    if (route.request().method() !== 'POST') return route.continue()
    const body = route.request().postDataJSON()
    bodies.push(body)
    carrier = body.aggregate_id
    const response = await route.fetch()
    expect(response.status()).toBe(201)
    await route.abort('failed')
    dropped()
  })
  await item.getByRole('button', { name: '新建复习承载', exact: true }).click()
  await expect(page.getByRole('alertdialog')).toContainText(
    '沿用本任务的原目标修订、来源版本及证据',
  )
  await page.getByRole('button', { name: '确认新建复习承载', exact: true }).click()
  await responseDropped
  await expect(item.getByRole('link', { name: '继续原任务复习', exact: true })).toHaveAttribute(
    'href',
    `/app/spaces/${space}/learn/${carrier}`,
  )
  expect(bodies).toHaveLength(1)
  expect(bodies[0]).toMatchObject({
    review_source: {
      task_id: review.task_id,
      evidence_id: review.evidence_id,
      attempt_id: review.attempt_id,
    },
  })
  await page.unroute('**/v1/tutoring/sessions')
  expect(
    (await client.call(space, `/v1/tutoring/sessions?goal_id=${source.goal.goal_id}`)).items,
  ).toHaveLength(sessionsBefore.items.length + 1)
  expect(
    (await client.call(space, `/v1/learning/progress?goal_id=${source.goal.goal_id}&status=all`))
      .items[0].evidence_count,
  ).toBe(1)
  // 完成第一次承载但不作答，再明确新建；第二次响应在切区后才释放。
  const firstCarrier = await client.call(space, `/v1/tutoring/sessions/${carrier}`)
  await client.call(space, `/v1/tutoring/sessions/${carrier}/actions`, {
    ...op('session', carrier, firstCarrier.session.aggregate_version),
    action: 'complete_session',
  })
  await page.reload()
  await expect(item.getByRole('button', { name: '新建复习承载', exact: true })).toBeVisible()
  const other = await client.call(space, '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '复习迟到响应目的区',
    description: '',
    status: 'active',
  })
  await page.reload()
  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  let committed!: () => void
  const responseCommitted = new Promise<void>((resolve) => {
    committed = resolve
  })
  let delivered!: () => void
  const responseDelivered = new Promise<void>((resolve) => {
    delivered = resolve
  })
  await page.route('**/v1/tutoring/sessions', async (route) => {
    if (route.request().method() !== 'POST') return route.continue()
    const body = route.request().postDataJSON()
    bodies.push(body)
    carrier = body.aggregate_id
    const response = await route.fetch()
    expect(response.status()).toBe(201)
    committed()
    await gate
    await route.fulfill({ response })
    delivered()
  })
  await item.getByRole('button', { name: '新建复习承载', exact: true }).click()
  await page.getByRole('button', { name: '确认新建复习承载', exact: true }).click()
  await responseCommitted
  await page.getByRole('combobox', { name: '范围', exact: true }).selectOption(other.id)
  await expect(page).toHaveURL(new RegExp(`space=${other.id}`))
  release()
  await responseDelivered
  await page.unroute('**/v1/tutoring/sessions')
  await expect(page.getByText('当前筛选和截止时间内没有到期复习。')).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`space=${other.id}`))
  await page.getByRole('combobox', { name: '范围', exact: true }).selectOption(space)
  const current = page.locator(`[data-review-task="${review.task_id}"]`)
  await expect(current.getByRole('link', { name: '继续原任务复习', exact: true })).toHaveAttribute(
    'href',
    `/app/spaces/${space}/learn/${carrier}`,
  )
  await page.reload()
  await expect(current.getByRole('button', { name: '新建复习承载', exact: true })).toHaveCount(0)
  expect(bodies).toHaveLength(2)
  expect(bodies[1].aggregate_id).not.toBe(bodies[0].aggregate_id)
  const after = await client.call(
    space,
    `/v1/learning/reviews?goal_id=${source.goal.goal_id}&status=all`,
  )
  expect(after.items).toHaveLength(1)
  expect(after.items[0]).toMatchObject({ ...review, carrier_session_id: carrier })
  expect(
    (await client.call(space, `/v1/tutoring/sessions?goal_id=${source.goal.goal_id}`)).items,
  ).toHaveLength(sessionsBefore.items.length + 2)
  expect((await client.call(space, `/v1/tutoring/sessions/${id}`)).session).toEqual(
    originalSession.session,
  )
  expect(
    (await client.call(space, `/v1/tutoring/sessions/${carrier}/knowledge-context`))
      .knowledge_context,
  ).toEqual(originalContext)
  expect(
    terminal.json(['study', 'context', '--space', space, '--session', carrier]).knowledge_context,
  ).toEqual(originalContext)
  await current.getByRole('link', { name: '继续原任务复习', exact: true }).click()
  await expect(page).toHaveURL(`/app/spaces/${space}/learn/${carrier}`)
  await answerReview(page)
  const saved = await client.call(space, `/v1/tutoring/sessions/${carrier}`)
  const result = await client.call(
    space,
    `/v1/learning/attempts/${saved.work_item.attempt.attempt_id}/feedback`,
  )
  expect(result.knowledge_context_revision_id).toBe(originalContext.id)
  expect(result.evidence).toHaveLength(1)
  expect(result.evidence[0]).toMatchObject({ kind: 'review_recall', outcome: 'pass' })
  const focusReference = (ref: { node_revision_id: string }) =>
    ref.node_revision_id === review.node_revision_id
  expect(result.activity.knowledge_references.find(focusReference)).toEqual(
    historical.activity.knowledge_references.find(focusReference),
  )
  expect(result.activity.knowledge_references.find(focusReference)).toBeDefined()
  for (const ref of result.activity.knowledge_references)
    expect(ref.knowledge_revision_id).toBe(review.knowledge_revision_id)
  expect(result.activity).toMatchObject({
    goal_revision_id: review.goal_revision_id,
    knowledge_revision_id: review.knowledge_revision_id,
    route_revision_id: review.route_revision_id,
    target_node_revision_id: review.node_revision_id,
  })
  expect(result.activity.activity_id).not.toBe(historical.activity.activity_id)
  expect(result.attempt.attempt_id).not.toBe(review.attempt_id)
  expect(
    terminal.json(['learn', 'show', '--space', space, '--session', carrier]).work_item.attempt,
  ).toEqual(result.attempt)
  const progress = terminal.json([
    'progress',
    '--space',
    space,
    '--goal-id',
    source.goal.goal_id,
    '--status',
    'all',
    '--json',
  ])
  expect(progress.items[0].evidence_count).toBe(2)
  expect(progress.items[0].reviews).toHaveLength(1)
  expect(progress.items[0].reviews[0].task_id).toBe(review.task_id)
  expect(
    (await client.call(space, `/v1/learning/attempts/${historical.attempt.attempt_id}/feedback`))
      .attempt,
  ).toEqual(historical.attempt)
  // 缓存/历史先真实加载两份答案、原 context 和正文，再经正式清除 UI 撤销。
  const historyURLs = [
    destination,
    `/app/progress?space=${space}&goal=${source.goal.goal_id}&status=all`,
    `/app/spaces/${space}/feedback/${review.attempt_id}`,
    `/app/spaces/${space}/feedback/${result.attempt.attempt_id}`,
    `/app/spaces/${space}/learn/${carrier}`,
    `/app/content/${result.content.artifact_id}?space=${space}`,
  ]
  for (const path of historyURLs.slice(0, 4)) {
    await page.goto(path)
    if (path.includes('/feedback/'))
      await expect(page.getByRole('heading', { name: '评估详情' })).toBeVisible()
    else await expect(page.getByRole('heading', { name: '学习进度与复习' })).toBeVisible()
  }
  const cached = await context.newPage()
  await cached.goto(`/app/spaces/${space}/learn/${carrier}`)
  await expect(cached.getByRole('heading', { name: '本次反馈', exact: true })).toBeVisible()
  const identity = await (await page.request.get('/v1/web/session')).json()
  const grant = execFileSync(
    '../../server/edu-agentd',
    ['privacy-grant', 'create', '--device', identity.device.id],
    { encoding: 'utf8', env: databaseEnv(), stdio: ['ignore', 'pipe', 'pipe'] },
  ).trim()
  await page.goto('/app/settings/data')
  await page.getByLabel('一次性清除授权').fill(grant)
  const erased = page.waitForResponse(
    (r) => r.url().endsWith('/v1/privacy/erasures') && r.request().method() === 'POST',
  )
  await page.getByRole('button', { name: '发起全局隐私清除', exact: true }).click()
  await page.getByRole('button', { name: '确认发起全局隐私清除', exact: true }).click()
  const receipt = await (await erased).json()
  expect(receipt.steps).toContainEqual(
    expect.objectContaining({ store: 'learning_event_payload', status: 'succeeded' }),
  )
  await expect.poll(async () => (await cached.request.get('/v1/web/session')).status()).toBe(401)
  await cached.bringToFront()
  await expect(cached.getByLabel('配对码', { exact: true })).toBeVisible()
  await pair(page, '/app/')
  const repaired = await (await page.request.get('/v1/web/session')).json()
  const read = async (path: string) =>
    page.request.get(path, {
      headers: { 'X-Learning-Space-ID': space, 'X-Learning-Content-Version': '1' },
    })
  for (const endpoint of [
    '/v1/learning/reviews?global=true&status=all',
    '/v1/learning/progress?global=true&status=all',
  ]) {
    const response = await read(endpoint)
    expect(response.ok(), await response.text()).toBe(true)
    expect(await response.json()).toMatchObject({ data_cleared: true, total: 0, items: [] })
  }
  for (const endpoint of [
    `/v1/tutoring/sessions/${carrier}`,
    `/v1/learning/attempts/${review.attempt_id}/feedback`,
    `/v1/learning/attempts/${result.attempt.attempt_id}/feedback`,
    `/v1/learning/content/${result.content.artifact_id}`,
    `/v1/learning/content/${result.content.artifact_id}/revisions`,
  ]) {
    const response = await read(endpoint)
    expect(response.ok(), endpoint).toBe(false)
    expect(await response.text()).not.toContain('偶数可以被 2 整除')
  }
  const clearedContext = await read(`/v1/tutoring/sessions/${carrier}/knowledge-context`)
  expect(clearedContext.ok(), await clearedContext.text()).toBe(true)
  expect(await clearedContext.json()).toEqual({ knowledge_context: null })
  const replay = await page.request.post('/v1/tutoring/sessions', {
    data: bodies[1],
    headers: {
      Origin: origin,
      'X-CSRF-Token': repaired.csrf_token,
      'X-Web-Principal-ID': repaired.device.id,
      'X-Web-Generation': String(repaired.generation),
      'X-Learning-Space-ID': space,
    },
  })
  expect(replay.ok()).toBe(false)
  for (const path of historyURLs) {
    await page.goto(path)
    if (path.includes('/progress'))
      await expect(page.getByRole('alert')).toContainText(
        '学习数据已清除或正在清除，旧结果已撤下。',
      )
    else await expect(page.getByRole('alert').first()).toBeVisible()
    expect(await page.locator('main').innerText()).not.toContain('偶数可以被 2 整除')
    expect(await page.locator('main').innerText()).not.toContain('根据原资料，哪个是偶数')
  }
  await page.goto(reviewURL(space))
  await expect(page.getByText('此范围的学习数据已清除，旧进度、复习及答案不可恢复。')).toBeVisible()
  await page.goBack()
  await expect(page.getByRole('alert').first()).toBeVisible()
  expect(await page.locator('main').innerText()).not.toContain('偶数可以被 2 整除')
  expect(
    await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage })),
  ).not.toContain('偶数可以被 2 整除')
  const clearedCLI = cli()
  expect(clearedCLI.json(['study', 'context', '--space', space, '--session', carrier])).toEqual({
    knowledge_context: null,
  })
  expect(clearedCLI.json(['reviews', '--global', '--status', 'all', '--json'])).toMatchObject({
    data_cleared: true,
    total: 0,
    items: [],
  })
  expect(clearedCLI.json(['progress', '--global', '--status', 'all', '--json'])).toMatchObject({
    data_cleared: true,
    total: 0,
    items: [],
  })
  await cached.close()
  expect(errors).toEqual([])
})
