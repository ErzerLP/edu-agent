import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import AxeBuilder from '@axe-core/playwright'

const space = '00000000-0000-4000-8000-000000000001'
const origin = 'http://127.0.0.1:32929'
const code = (profile = 'user') =>
  execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', profile], {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env: {
      PATH: process.env.PATH,
      DATABASE_URL: process.env.TEST_DATABASE_URL,
      MIGRATE_ON_START: 'true',
    },
  }).trim()

async function legacySession(request: APIRequestContext, spaceId = space) {
  const pair = await request.post('/v1/pairings/exchange', {
    data: { code: code(), display_name: '旧协议教学验收' },
  })
  expect(pair.ok(), await pair.text()).toBe(true)
  const token = (await pair.json()).token
  const headers = { Authorization: `Bearer ${token}`, 'X-Learning-Space-ID': spaceId }
  const collectionId = randomUUID()
  const call = async (method: string, path: string, data?: unknown) => {
    const response = await request.fetch(path, {
      method,
      headers: {
        ...headers,
        ...(['/v1/knowledge/imports', '/v1/knowledge/revisions/head'].includes(path)
          ? { 'X-Knowledge-Collection-ID': collectionId }
          : {}),
      },
      data,
    })
    expect(response.ok(), `${path}: ${await response.text()}`).toBe(true)
    return response.json()
  }
  await call('POST', '/v1/knowledge/collections', {
    id: collectionId,
    action: 'create',
    name: '教学验收资料',
    source: 'browser-workspace',
  })
  await call('POST', '/v1/knowledge/imports', {
    operation_id: randomUUID(),
    expected_parent_revision_id: null,
    source: '浏览器工作区验收',
    documents: [
      { path: 'even.md', markdown: '# 偶数\n\n偶数可以被 2 整除。2 是偶数，3 是奇数。\n' },
      {
        path: 'examples.md',
        markdown:
          '# 偶数的阅读与核对\n\n偶数。完成每一轮时检查问题与资料定位，保留会话和操作编号以便继续学习。确认反馈后进入下一轮。\n',
      },
    ],
  })
  const collectionRevision = (await call('GET', '/v1/knowledge/revisions/head')).revision
    .revision_id
  const frozenScope = await call('POST', '/v1/knowledge/scopes', {
    id: randomUUID(),
    entries: [{ collection_id: collectionId, revision_id: collectionRevision }],
  })
  const revision = frozenScope.id
  const goal = (
    await call('POST', '/v1/learning/goals', {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'goal',
      aggregate_id: randomUUID(),
      expected_version: 0,
      text: '理解偶数并使用原资料说明依据',
      source: '旧协议教学验收',
      details: { name: '理解偶数并使用原资料说明依据', scope_snapshot_id: revision },
    })
  ).result
  const id = randomUUID()
  await call('POST', '/v1/tutoring/sessions', {
    operation_id: randomUUID(),
    payload_schema_version: 1,
    aggregate_type: 'session',
    aggregate_id: id,
    expected_version: 0,
    goal_revision_id: goal.goal_revision_id,
  })
  const get = () => call('GET', `/v1/tutoring/sessions/${id}`)
  const action = async (action: string, extra = {}) => {
    const view = await get()
    return call('POST', `/v1/tutoring/sessions/${id}/actions`, {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'session',
      aggregate_id: id,
      expected_version: view.session.aggregate_version,
      action,
      ...extra,
    })
  }
  await action('start_diagnostic')
  for (const kind of ['route', 'activity']) {
    const view = await get()
    const retrieval = await call('POST', '/v1/knowledge/retrievals', {
      query: '偶数',
      scope_snapshot_id: revision,
      query_context_schema_version: 'query-context-v1',
      context: { session_id: id },
    })
    expect(retrieval.hits.length, JSON.stringify(retrieval.trace)).toBeGreaterThan(1)
    const hits = retrieval.hits.map((hit: Record<string, unknown>) => ({
      knowledge_revision_id: revision,
      node_id: hit.node_id,
      node_revision_id: hit.node_revision_id,
      document_revision_id: hit.document_revision_id,
      range: hit.section_range,
      slice: hit.canonical_slice,
      slice_sha256: hit.slice_sha256,
    }))
    const proposal = await call('POST', '/v1/tutoring/proposals', {
      request_id: randomUUID(),
      proposal_type: kind,
      aggregate_type: 'session',
      aggregate_id: id,
      aggregate_version: view.session.aggregate_version,
      goal_revision_id: goal.goal_revision_id,
      ...view.session.focus,
      tutoring_state: view.session.state,
      knowledge_revision_id: revision,
      node_revision_ids: [
        ...new Set(hits.map((h: { node_revision_id: string }) => h.node_revision_id)),
      ],
      input: {
        schema_version: 'go-cli-context-v1',
        work_item: view.work_item,
        retrieval: { knowledge_revision_id: revision, hits },
      },
    })
    await action(kind === 'route' ? 'apply_route' : 'issue_activity', {
      proposal_id: proposal.proposal_id,
    })
  }
  return { id, goal, call, get, action }
}

async function webCall(page: Page, path: string, data?: unknown) {
  const identity = await (await page.request.get('/v1/web/session')).json()
  return page.request.fetch(path, {
    method: data ? 'POST' : 'GET',
    data,
    headers: {
      Origin: origin,
      'X-CSRF-Token': identity.csrf_token,
      'X-Learning-Space-ID': space,
      'X-Learning-Content-Version': '1',
      'X-Learning-Change-Version': '1',
    },
  })
}

test('旧会话真实阅读、版本、讨论、丢响应作答、反馈及续学', async ({
  page,
  context,
  request,
}, testInfo) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地教学模型 fixture')
  test.setTimeout(180000)
  page.setDefaultTimeout(15000)
  const fixture = await legacySession(request)
  const before = await fixture.get()
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code())
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  await page.goto(`/app/spaces/${space}/goals/${fixture.goal.goal_id}`)
  await page
    .getByRole('region', { name: '教学会话', exact: true })
    .getByRole('link')
    .first()
    .click()
  await expect(page.getByRole('heading', { name: '活动已就绪' })).toBeVisible()
  await expect(page.getByText('内容第 1 版已保存', { exact: false })).toBeVisible()
  const contentLink = await page.getByRole('link', { name: '内容与版本历史' }).getAttribute('href')
  expect(contentLink).toMatch(/\/content\/[^?]+\?space=/)
  await page.getByRole('button', { name: '开始当前活动' }).click()
  const input = page.getByLabel('我的正式答案')
  await expect(input).toBeEnabled()
  await input.fill('还没提交的草稿')
  await input.dispatchEvent('keydown', { key: 'Enter', ctrlKey: true, isComposing: true })
  expect((await fixture.get()).work_item.attempt).toBeUndefined()
  await page.getByRole('link', { name: '内容与版本历史' }).click()
  await expect(page.getByRole('heading', { name: '学习内容 · 第 1 版' })).toBeVisible()
  await page.getByRole('link', { name: '← 返回教学会话' }).click()
  await expect(input).toHaveValue('还没提交的草稿')
  await page.getByRole('button', { name: '切换深色主题' }).click()
  await expect(input).toHaveValue('还没提交的草稿')
  await page.getByRole('button', { name: '切换浅色主题' }).click()
  const source = page.getByRole('button', { name: '原资料依据 1', exact: true })
  await source.click()
  await expect(page.getByRole('dialog', { name: '原资料依据' })).toContainText('偶数可以被 2 整除')
  await page.getByRole('button', { name: '关闭来源' }).click()
  await expect(source).toBeFocused()
  for (const theme of ['light', 'dark']) {
    if (theme === 'dark') await page.getByRole('button', { name: '切换深色主题' }).click()
    for (const width of [390, 768, 1280, 1440]) {
      await page.setViewportSize({ width, height: 1000 })
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
        true,
      )
      const audit = await new AxeBuilder({ page })
        .withTags(['wcag2a', 'wcag2aa', 'wcag21aa'])
        .analyze()
      expect(audit.violations.map((v) => v.id)).toEqual([])
      await page.screenshot({
        path: testInfo.outputPath(`workspace-${theme}-${width}.png`),
        fullPage: true,
      })
    }
  }
  await page.setViewportSize({ width: 1440, height: 1000 })
  const widthSlider = page.getByLabel('知识栏宽度')
  await widthSlider.focus()
  await page.keyboard.press('ArrowRight')
  await expect(widthSlider).toHaveValue('273')
  const second = await context.newPage()
  await second.goto(page.url())
  await expect(second.getByLabel('我的正式答案')).toHaveValue('')
  await second.getByLabel('我的正式答案').fill('另一个标签页草稿')
  await page.getByRole('textbox', { name: '和导师讨论', exact: true }).fill('为什么 2 是偶数？')
  await page.getByRole('textbox', { name: '和导师讨论', exact: true }).press('Enter')
  await expect(page.getByRole('button', { name: '请导师回答' })).toBeVisible()
  expect((await fixture.get()).work_item.attempt).toBeUndefined()
  await page.getByRole('button', { name: '请导师回答' }).click()
  await expect(page.getByText('原资料说明：偶数可以被 2 整除。先检查选项的含义。')).toBeVisible()
  await page.getByRole('button', { name: '返回原学习焦点' }).click()
  await expect(input).toHaveValue('还没提交的草稿')
  await expect(second.getByLabel('我的正式答案')).toHaveValue('另一个标签页草稿')
  await second.close()
  await page.getByRole('textbox', { name: '和导师讨论', exact: true }).fill('稍后再问的问题')
  await page.getByRole('button', { name: '请求提示', exact: true }).click()
  await page.getByRole('button', { name: '请导师回答' }).click()
  await expect(page.getByText('原资料说明：偶数可以被 2 整除。先检查选项的含义。')).toBeVisible()
  await page.getByRole('button', { name: '返回原学习焦点' }).click()
  await expect(page.getByRole('textbox', { name: '和导师讨论', exact: true })).toHaveValue(
    '稍后再问的问题',
  )
  await expect(page.getByLabel('作答帮助等级')).toHaveValue('hint')
  await input.fill('A')
  let posts = 0
  await page.route('**/v1/learning/content/*/answers', async (route) => {
    posts++
    const response = await route.fetch()
    expect(response.ok(), await response.text()).toBe(true)
    await route.abort('failed')
  })
  await input.press('Control+Enter')
  await expect(page.getByText('已核对原操作：提交已保存。')).toBeVisible()
  expect(posts).toBe(1)
  await page.getByRole('button', { name: '获取教学反馈' }).click()
  await expect(page.getByRole('region', { name: '教学反馈' })).toContainText('已接纳')
  const after = await fixture.get()
  expect(after.work_item.activity).toEqual(before.work_item.activity)
  expect(after.work_item.attempt.answer).toBe('A')
  expect(after.work_item.attempt.help).toBe('hint')
  await page.getByRole('button', { name: '查看完毕，继续学习' }).click()
  await expect(page.getByRole('heading', { name: '阅读与探索' })).toBeVisible()
  await page.getByRole('button', { name: '生成下一活动' }).click()
  await expect(page.getByRole('heading', { name: '活动已就绪' })).toBeVisible()
  await page.reload()
  await expect(page.getByText('内容第 1 版已保存', { exact: false })).toBeVisible()
  expect(
    await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage })),
  ).not.toContain('草稿')
  expect(errors).toEqual([])
})

test('内容草稿、未知交互、来源安全与指定版本', async ({ page, request }) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地教学模型 fixture')
  const fixture = await legacySession(request)
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code())
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  await page.goto(`/app/spaces/${space}/learn/${fixture.id}`)
  await expect(page.getByRole('link', { name: '内容与版本历史' })).toBeVisible()
  const view = await fixture.get()
  const first = await (
    await webCall(page, `/v1/tutoring/sessions/${fixture.id}/content`, {
      protocol_version: 1,
      activity_id: view.work_item.activity.activity_id,
    })
  ).json()
  const path = `/v1/learning/content/${first.artifact_id}`
  const unsafe = {
    block_id: randomUUID(),
    kind: 'markdown',
    text: '<script>window.contentXSS=1</script>\n\n![追踪](https://tracker.invalid/pixel)\n\n[危险](javascript:alert%281%29)',
    fallback: '安全回退',
  }
  const math = {
    block_id: randomUUID(),
    kind: 'math',
    text: '\\includegraphics{https://tracker.invalid/pixel}',
    fallback: '不允许外部公式资源',
  }
  const draft = await webCall(page, `${path}/revisions`, {
    protocol_version: 1,
    operation_id: randomUUID(),
    expected_version: 1,
    status: 'draft',
    blocks: [unsafe, math],
    interaction: { kind: 'text' },
  })
  expect(draft.status()).toBe(201)
  expect((await (await webCall(page, path)).json()).version).toBe(1)
  const invalid = await webCall(page, `${path}/revisions`, {
    protocol_version: 1,
    operation_id: randomUUID(),
    expected_version: 2,
    status: 'committed',
    blocks: [unsafe],
    interaction: { kind: 'text' },
  })
  expect(invalid.status()).toBe(400)
  let tracker = 0
  page.on('request', (req) => {
    if (req.url().includes('tracker.invalid')) tracker++
  })
  await page.goto(`/app/content/${first.artifact_id}?space=${space}&version=2`)
  await expect(page.getByText('未提交草稿。此页面只供阅读', { exact: false })).toBeVisible()
  await expect(page.getByText('公式文本（不支持或超过安全限制）', { exact: false })).toBeVisible()
  expect(await page.evaluate(() => Reflect.get(window, 'contentXSS'))).toBeUndefined()
  expect(tracker).toBe(0)
  const committed = await webCall(page, `${path}/revisions`, {
    protocol_version: 1,
    operation_id: randomUUID(),
    expected_version: 2,
    status: 'committed',
    blocks: first.body.blocks,
    interaction: { kind: 'future_execution' },
  })
  expect(committed.status()).toBe(201)
  await page.goto(`/app/spaces/${space}/learn/${fixture.id}`)
  await page.getByRole('button', { name: '开始当前活动' }).click()
  await expect(page.getByText('此内容的作答类型需要升级客户端，当前禁止提交。')).toBeVisible()
  await expect(page.getByRole('button', { name: '提交正式答案' })).toBeDisabled()
  const session = await fixture.get()
  const answer = await webCall(page, `${path}/answers`, {
    operation_id: randomUUID(),
    payload_schema_version: 1,
    aggregate_type: 'session',
    aggregate_id: fixture.id,
    expected_version: session.session.aggregate_version,
    action: 'submit_attempt',
    answer: 'A',
    help: 'none',
    content_version: 3,
  })
  expect(answer.status()).toBe(422)
  expect((await fixture.get()).work_item.attempt).toBeUndefined()
  const swapped = await webCall(page, `${path}/revisions`, {
    protocol_version: 1,
    operation_id: randomUUID(),
    expected_version: 3,
    status: 'committed',
    blocks: first.body.blocks,
    interaction: {
      kind: 'single_choice',
      choices: [
        { value: 'A', label: '3' },
        { value: 'B', label: '2' },
      ],
    },
  })
  expect(swapped.status()).toBe(400)
  const choices = await webCall(page, `${path}/revisions`, {
    protocol_version: 1,
    operation_id: randomUUID(),
    expected_version: 3,
    status: 'committed',
    blocks: first.body.blocks,
    interaction: {
      kind: 'single_choice',
      choices: [
        { value: 'A', label: '2' },
        { value: 'B', label: '3' },
      ],
    },
  })
  expect(choices.status()).toBe(201)
  await page.reload()
  await page.getByRole('radio', { name: '2', exact: true }).check()
  await page.getByRole('button', { name: '提交正式答案' }).click()
  await expect(page.getByRole('button', { name: '获取教学反馈' })).toBeVisible()
  expect((await fixture.get()).work_item.attempt.answer).toBe('A')
})

test('切区后旧答案迟到响应不修改新会话草稿', async ({ page, request }) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地教学模型 fixture')
  page.setDefaultTimeout(15000)
  const first = await legacySession(request)
  const otherSpace = await first.call('POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: `迟到输出隔离 ${randomUUID().slice(0, 8)}`,
    description: '',
    status: 'active',
  })
  const second = await legacySession(request, otherSpace.id)
  await first.action('present_activity')
  await second.action('present_activity')
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code())
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  await page.goto(`/app/spaces/${space}/learn/${first.id}`)
  const answer = page.getByRole('textbox', { name: '我的正式答案' })
  await expect(answer).toBeEnabled()
  await answer.fill('A')
  let saved!: () => void
  let release!: () => void
  let delivered!: () => void
  const committed = new Promise<void>((resolve) => {
    saved = resolve
  })
  const delay = new Promise<void>((resolve) => {
    release = resolve
  })
  const finished = new Promise<void>((resolve) => {
    delivered = resolve
  })
  await page.route('**/v1/learning/content/*/answers', async (route) => {
    const response = await route.fetch()
    expect(response.ok(), await response.text()).toBe(true)
    saved()
    await delay
    await route.fulfill({ response })
    delivered()
  })
  await page.getByRole('button', { name: '提交正式答案' }).click()
  await committed
  await page.getByText('切换学习区', { exact: true }).click()
  await page.getByRole('link', { name: otherSpace.name, exact: true }).click()
  await page.locator(`a[href="/app/spaces/${otherSpace.id}/goals/${second.goal.goal_id}"]`).click()
  await page
    .getByRole('region', { name: '教学会话', exact: true })
    .getByRole('link')
    .first()
    .click()
  await expect(answer).toBeEnabled()
  await answer.fill('新区未提交草稿')
  release()
  await finished
  await expect(answer).toHaveValue('新区未提交草稿')
  await expect(page).toHaveURL(new RegExp(`/learn/${second.id}$`))
  expect((await first.get()).work_item.attempt.answer).toBe('A')
  expect((await second.get()).work_item.attempt).toBeUndefined()
})

test('教学变更显示具体差异、排队、立即切换与原题草稿焦点恢复', async ({ page, request }) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地教学模型 fixture')
  test.setTimeout(180000)
  const fixture = await legacySession(request)
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code())
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  await page.goto(`/app/spaces/${space}/learn/${fixture.id}`)
  await expect(page.getByText('内容第 1 版已保存', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: '开始当前活动' }).click()
  const answer = page.getByLabel('我的正式答案')
  await expect(answer).toBeEnabled()
  await answer.fill('原题尚未提交的草稿')
  const panel = page.getByRole('region', { name: '教学变更', exact: true })
  const path = `/v1/learning/goals/${fixture.goal.goal_id}`
  const read = await webCall(page, `${path}/change-context?session_id=${fixture.id}`)
  expect(read.ok(), await read.text()).toBe(true)
  const current = await read.json()
  const candidate = { kind: 'route', trigger: 'user_request', reason: '先补偶数的前置概念', evidence_ids: [], context_id: '', explanation: '', steps: [{ node_revision_id: current.sources[0].node_revision_id, name: '前置概念练习', criterion: '说明可被二整除', prompt: '请用一个例子解释偶数', difficulty: 1, prerequisites: [] }] }
  const id = randomUUID()
  const proposed = await webCall(page, `${path}/changes/${id}`, { session_id: fixture.id, operation_id: randomUUID(), action: 'propose', expected_revision: 0, hash: '', interaction_id: '', immediate: false, base: current.base, candidate })
  expect(proposed.ok(), await proposed.text()).toBe(true)
  await expect(panel.getByText('已排队，本题处理后应用', { exact: false })).toBeVisible()
  await expect(answer).toHaveValue('原题尚未提交的草稿')
  await panel.getByRole('button', { name: '立即切换并保留原题现场' }).click()
  await expect(page.getByText('请用一个例子解释偶数', { exact: true }).first()).toBeVisible()
  await page.getByRole('button', { name: '开始当前活动' }).click()
  await answer.fill('新题草稿也要保留')
  await panel.getByRole('button', { name: '返回原题与草稿' }).click()
  await expect(answer).toHaveValue('原题尚未提交的草稿')
  await expect(answer).toBeFocused()
  const latest = await (await webCall(page, `${path}/change-context?session_id=${fixture.id}`)).json()
  const goalCandidate = { kind: 'goal', trigger: 'goal_constraint', reason: '增加具体完成标准', evidence_ids: [], context_id: '', explanation: '', steps: [], goal: { ...latest.goal.management.details, completion_criteria: '独立给出三个偶数并解释' } }
  const goalChange = await webCall(page, `${path}/changes/${randomUUID()}`, { session_id: fixture.id, operation_id: randomUUID(), action: 'propose', expected_revision: 0, hash: '', interaction_id: '', immediate: false, base: latest.base, candidate: goalCandidate })
  expect(goalChange.ok(), await goalChange.text()).toBe(true)
  await expect(panel.getByRole('table')).toContainText('独立给出三个偶数并解释')
  expect((await fixture.call('GET', path)).management.details.completion_criteria).not.toBe('独立给出三个偶数并解释')
  await panel.getByRole('button', { name: '采用新的目标范围和完成标准' }).click()
  await expect.poll(async () => (await fixture.call('GET', path)).management.details.completion_criteria).toBe('独立给出三个偶数并解释')
  expect(errors).toEqual([])
})

test('选段模型加工、答案保留、Studio、来源、固定与补偿版本', async ({
  page,
  request,
  context,
}, testInfo) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地教学及选段模型 fixture')
  test.setTimeout(180000)
  page.setDefaultTimeout(15000)
  const fixture = await legacySession(request)
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code('settings'))
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  const settingsIdentity = await (await page.request.get('/v1/web/session')).json()
  const settingsHeaders = {
    Origin: origin,
    'X-CSRF-Token': settingsIdentity.csrf_token,
    'X-Web-Principal-ID': settingsIdentity.device.id,
    'X-Web-Generation': String(settingsIdentity.generation),
  }
  const configuration = await (
    await page.request.get('/v1/settings', { headers: settingsHeaders })
  ).json()
  const configured = await page.request.put('/v1/settings', {
    headers: settingsHeaders,
    data: {
      expected_revision: configuration.revision,
      target: 'mentor',
      connection: {
        enabled: true,
        provider: 'openai_compatible',
        endpoint: 'http://127.0.0.1:32930/v1',
        model: 'browser-content-fixture',
        auth_mode: 'none',
      },
    },
  })
  expect(configured.ok(), await configured.text()).toBe(true)
  await context.clearCookies()
  const original = await fixture.get()
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code())
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  await page.goto(`/app/spaces/${space}/learn/${fixture.id}`)
  await page.getByRole('button', { name: '开始当前活动' }).click()
  const answer = page.getByLabel('我的正式答案')
  await answer.fill('加工期间保留的正式答案草稿')
  await page.getByRole('button', { name: '选择此段', exact: true }).first().click()
  const editor = page.getByRole('region', { name: '选段导师', exact: true })
  await editor.getByLabel('选段动作').selectOption('example')
  await editor.getByLabel('选段指令').fill('请用苹果说明这一段')
  await editor.getByLabel('允许把所选正文及来源发送给已配置导师', { exact: false }).check()
  await editor.getByLabel('选段指令').dispatchEvent('keydown', { key: 'Enter', isComposing: true })
  await expect(editor.getByLabel('选段操作结果')).toHaveCount(0)
  await editor.getByRole('button', { name: '执行选段请求' }).click()
  await expect(editor.getByRole('link', { name: '本段已更新 · 查看变化' })).toBeVisible()
  await expect(answer).toHaveValue('加工期间保留的正式答案草稿')
  await expect(page.getByLabel('当前学习', { exact: true })).toContainText(
    '把六个苹果每两个分成一组',
  )
  expect((await fixture.get()).work_item.activity).toEqual(original.work_item.activity)
  await editor.getByRole('link', { name: '本段已更新 · 查看变化' }).click()
  await expect(page.getByRole('heading', { name: '学习内容 · 第 2 版' })).toBeVisible()
  await expect(page.getByLabel('本版变化')).toContainText('请用苹果说明这一段')
  const contentURL = page.url()
  await page.getByRole('link', { name: '任务中心', exact: true }).click()
  await page.getByRole('combobox', { name: '任务类型', exact: true }).selectOption('content_edit')
  await page.getByRole('combobox', { name: '运行状态', exact: true }).selectOption('succeeded')
  await page.getByRole('link', { name: '查看原运行', exact: true }).click()
  await expect(page.getByRole('heading', { name: '内容生成与改写 · 已完成' })).toBeVisible()
  await page.getByRole('link', { name: '查看正式内容版本 2', exact: true }).click()
  await expect(page).toHaveURL(contentURL)
  await page.getByRole('button', { name: '收藏内容', exact: true }).click()
  await page.getByRole('button', { name: '固定当前阅读版本', exact: true }).click()
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: '导出 Markdown', exact: true }).click()
  expect((await download).suggestedFilename()).toContain('-v2.md')
  await page.getByRole('button', { name: '查看原资料依据', exact: true }).first().click()
  // 两份合法资料的检索顺序不固定，按真实出处核对对应正文。
  const sourceDialog = page.getByRole('dialog')
  await expect(sourceDialog).toContainText(/出处：(even|examples)\.md/)
  await expect(sourceDialog).toContainText(
    (await sourceDialog.textContent())?.includes('出处：even.md')
      ? '偶数可以被 2 整除'
      : '完成每一轮时检查问题与资料定位',
  )
  await page.getByRole('button', { name: '关闭来源' }).click()
  await expect(
    page.getByRole('button', { name: '查看原资料依据', exact: true }).first(),
  ).toBeFocused()
  await page.getByRole('link', { name: /第 1 版 · 正式/ }).click()
  await page.getByRole('button', { name: '恢复为新的补偿版本' }).click()
  await expect(page.getByRole('heading', { name: '学习内容 · 第 3 版' })).toBeVisible()
  expect((await fixture.get()).work_item.activity).toEqual(original.work_item.activity)
  await page.getByRole('link', { name: '查看 Studio 内容库', exact: true }).click()
  await page.getByLabel('关联目标').selectOption(fixture.goal.goal_id)
  await page.getByLabel('只看收藏').check()
  const entry = page.locator('.studio-item').first()
  await expect(entry).toContainText('第 3 版')
  await expect(entry).toContainText('固定阅读第 2 版')
  await entry.getByRole('link').first().click()
  await expect(page.getByRole('heading', { name: '学习内容 · 第 2 版' })).toBeVisible()
  for (const theme of ['light', 'dark']) {
    if (theme === 'dark') await page.getByRole('button', { name: '切换深色主题' }).click()
    for (const width of [390, 768, 1280, 1440]) {
      await page.setViewportSize({ width, height: 1000 })
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
        true,
      )
      await page.screenshot({
        path: testInfo.outputPath(`content-${theme}-${width}.png`),
        fullPage: true,
      })
    }
  }
  expect(errors).toEqual([])
})
