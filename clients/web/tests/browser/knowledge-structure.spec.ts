import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { randomUUID } from 'node:crypto'
import type { ConceptContent, StructureEdit, StructureProposal } from '../../src/api/structure'
import { offlineFixture, serverCommand } from './offline-fixture'
import { navigateFromMenu } from './notebook-fixture'

const space = '00000000-0000-4000-8000-000000000001'
async function api(page: Page, path: string, body?: unknown, headers = {}) {
  const session = await (await page.request.get('/v1/web/session')).json()
  const response = await page.request.fetch(path, { method: body ? 'POST' : 'GET', data: body, headers: { Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token, 'X-Learning-Space-ID': space, ...headers } })
  expect(response.ok(), await response.text()).toBeTruthy()
  return response.json()
}
async function pair(page: Page, profile = 'references') {
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(serverCommand(['pairing-code', 'create', '--profile', profile]))
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '你的学习区', exact: true, level: 1 })).toBeVisible()
}
async function applyFixture(page: Page, edits: StructureEdit[]) {
  const structure = await api(page, '/v1/knowledge/structure')
  const proposal = await api(page, '/v1/knowledge/structure/proposals', {
    operation_id: randomUUID(), base_version: structure.version, generation: structure.generation,
    kind: 'edit', reason: '浏览器补验初始知识', edits,
  })
  const applied: StructureProposal = await api(page, `/v1/knowledge/structure/proposals/${proposal.id}/decisions`, {
    operation_id: randomUUID(), hash: proposal.hash, decision: 'approve', reason: '正式批准补验初始知识',
  })
  expect(applied.status).toBe('applied')
  return applied
}
test('真实知识列表、局部图、审阅、未知结果恢复和键盘替代', async ({ page }) => {
  test.setTimeout(120_000)
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  await pair(page)
  const goal = randomUUID()
  const relatedID = randomUUID(), relatedTitle = `前置概念 ${goal.slice(0, 8)}`
  await api(page, '/v1/learning/goals', { operation_id: randomUUID(), payload_schema_version: 1, aggregate_type: 'goal', aggregate_id: goal, expected_version: 0, text: '知识结构浏览器验收', source: '浏览器验收', details: { name: '知识结构浏览器验收' } })
  // 固定保留同名已应用记录，单文件执行也必须能区分历史提案和本次操作。
  const reason = '明确的可审阅概念提案'
  const structure = await api(page, '/v1/knowledge/structure')
  // 用例在 Node.js 中收集，夹具只引用协议类型，避免加载依赖 window 的浏览器客户端。
  const historicalContent: ConceptContent = {
    description: '', source_status: 'candidate', suggested: false,
    sources: [], claims: [], relations: [], replaced_by: [],
  }
  const historical = await api(page, '/v1/knowledge/structure/proposals', {
    operation_id: randomUUID(), base_version: structure.version, generation: structure.generation,
    kind: 'edit', reason, edits: [
      { concept_id: randomUUID(), goal_id: '', name: '历史独立概念', content: historicalContent },
      { concept_id: relatedID, goal_id: goal, name: relatedTitle, content: historicalContent },
    ],
  })
  const applied = await api(page, `/v1/knowledge/structure/proposals/${historical.id}/decisions`, {
    operation_id: randomUUID(), hash: historical.hash, decision: 'approve', reason: '保留历史审阅结果',
  })
  expect(applied.status).toBe('applied')
  await page.goto(`/app/spaces/${space}/knowledge?goal=${goal}`)
  const panel = page.getByRole('region', { name: '动态知识结构', exact: true })
  await panel.getByRole('button', { name: '新增独立概念' }).click()
  const title = `独立概念 ${randomUUID().slice(0, 8)}`
  await panel.getByLabel('概念名称', { exact: true }).fill(title)
  await panel.getByLabel('含义、纠正说明与适用范围').fill('同名也保留独立身份')
  await panel.getByLabel('维护理由与语义依据').fill(reason)
  await panel.getByRole('button', { name: '添加关系修正' }).click()
  await panel.getByLabel('关系类型').selectOption('prerequisite')
  await panel.getByRole('combobox', { name: '目标概念', exact: true }).selectOption(relatedID)
  let lost = false
  let created: StructureProposal
  await page.route('**/v1/knowledge/structure/proposals', async route => {
    if (route.request().method() === 'POST' && !lost) {
      lost = true
      const response = await route.fetch()
      expect(response.ok(), await response.text()).toBe(true)
      created = await response.json()
      await route.abort('failed')
      return
    }
    await route.continue()
  })
  await panel.getByRole('button', { name: '提交维护提案' }).click()
  await expect(panel.getByText('提案提交结果未知，已保留原操作。请核对后再继续。')).toBeVisible()
  const replayed = page.waitForResponse(response => response.url().endsWith('/v1/knowledge/structure/proposals') && response.request().method() === 'POST')
  await panel.getByRole('button', { name: '核对并重试原提案' }).click()
  const replay = await replayed
  expect(replay.ok(), await replay.text()).toBe(true)
  expect(await replay.json()).toMatchObject({ id: created!.id, operation_id: created!.operation_id, replayed: true })
  const review = panel.getByRole('article').filter({ hasText: created!.id })
  await expect(review).toHaveCount(1)
  await expect(review).toContainText('待审阅')
  await review.getByLabel('审阅 / 补偿理由').fill('核对通过，来源状态和学习状态独立')
  await review.getByRole('button', { name: '批准知识维护', exact: true }).click()
  await page.getByRole('button', { name: '确认批准知识维护', exact: true }).click()
  await expect(panel.getByRole('button', { name: title, exact: true })).toBeVisible()
  await expect(panel.getByText('来源：候选；学习：未接触', { exact: true })).toHaveCount(2)
  await panel.getByRole('button', { name: '显示局部关系图' }).click()
  const graph = panel.getByRole('img', { name: '当前页概念关系，无掌握颜色' })
  const rows = panel.locator('.structure-list > li')
  await expect(graph).toBeVisible()
  await expect(rows).toHaveCount(2)
  for (const name of [title, relatedTitle]) {
    await expect(rows.getByRole('button', { name, exact: true })).toBeVisible()
    await expect(panel.getByRole('button', { name: `图节点：${name}`, exact: true })).toBeVisible()
  }
  expect((await graph.locator('title').allTextContents()).sort()).toEqual([title, relatedTitle].sort())
  await expect(graph.locator('line')).toHaveCount(1)
  await expect(graph.locator('text').filter({ hasText: /^前置$/ })).toHaveCount(1)
  const currentRow = rows.filter({ has: page.getByRole('button', { name: title, exact: true }) })
  await expect(currentRow).toContainText(`前置 → ${relatedTitle}`)
  await currentRow.getByRole('button', { name: '查看一跳关系' }).click()
  await expect(panel.getByRole('button', { name: '退出局部节点范围' })).toBeVisible()
  await expect(rows).toHaveCount(2)
  await expect(graph.locator('line')).toHaveCount(1)
  await panel.getByRole('button', { name: `图节点：${title}`, exact: true }).focus()
  await page.keyboard.press('Enter')
  await expect(panel.getByRole('article', { name: '概念详情' })).toBeVisible()
  const list = await api(page, `/v1/knowledge/structure?goal_id=${goal}`)
  expect(list.items.filter((n: { name: string }) => n.name === title)).toHaveLength(1)
  expect(list.items.map((n: { name: string }) => n.name).sort()).toEqual([title, relatedTitle].sort())
  expect(list.edges).toEqual([{ source_id: created!.after[0].concept_id, target_id: relatedID, kind: 'prerequisite', suggested: false, sources: [] }])
  expect((await api(page, '/v1/knowledge/structure/proposals')).items.filter((p: StructureProposal) => p.operation_id === created!.operation_id)).toHaveLength(1)
  expect(await api(page, `/v1/knowledge/structure/proposals/${historical.id}`)).toMatchObject({ status: 'applied', decision_reason: '保留历史审阅结果' })
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy()
  const audit = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()
  expect(audit.violations.map(v => v.id)).toEqual([])
  expect(errors).toEqual([])
})

test('知识维护经教学变更排队，原题和未提交草稿在页面往返后保留', async ({ page, request }) => {
  test.skip(process.env.WEB_WORKSPACE_FIXTURE !== '1', '需要本地教学模型 fixture')
  test.setTimeout(180_000)
  const fixture = await offlineFixture(request)
  await pair(page)
  const conceptID = randomUUID(), title = `课堂概念 ${conceptID.slice(0, 8)}`
  const content: ConceptContent = {
    description: '待审阅的偶数解释', source_status: 'candidate', suggested: false,
    sources: [], claims: [], relations: [], replaced_by: [],
  }
  await applyFixture(page, [{ concept_id: conceptID, goal_id: fixture.goal.goal_id, name: title, content }])
  await page.goto(`/app/spaces/${space}/learn/${fixture.id}`)
  await expect(page.getByText('内容第 1 版已保存', { exact: false })).toBeVisible()
  const answer = page.getByLabel('我的正式答案')
  await expect(answer).toBeEnabled()
  const draft = '知识更新期间尚未提交的答案：2 可以被 2 整除。'
  await answer.fill(draft)
  const blocks = page.getByLabel('当前学习', { exact: true }).locator('.content-block')
  const originalBody = await blocks.allTextContents()
  expect(originalBody.length).toBeGreaterThan(0)
  const original = await fixture.get()
  const contextPath = `/v1/learning/goals/${fixture.goal.goal_id}/change-context?session_id=${fixture.id}`
  const headers = { 'X-Learning-Change-Version': '1' }
  const before = await api(page, contextPath, undefined, headers)
  expect(before.base.activity_id).toBe(original.work_item.activity.activity_id)

  // 同一应用内往返，保留真实标签页内存草稿；不以重新填入答案代替恢复。
  await page.getByText('课堂冻结资料与知识', { exact: true }).click()
  await page.getByRole('link', { name: '查看知识、冲突与维护' }).click()
  const panel = page.getByRole('region', { name: '动态知识结构', exact: true })
  await panel.getByLabel(`选择「${title}」${conceptID.slice(0, 8)}`, { exact: true }).check()
  await panel.getByRole('button', { name: '修订所选概念' }).click()
  const revisedTitle = `${title}（已纠正）`
  await panel.getByLabel('概念名称', { exact: true }).fill(revisedTitle)
  await panel.getByLabel('含义、纠正说明与适用范围').fill('审阅后明确：整数能被 2 整除时是偶数。')
  await panel.getByLabel('维护理由与语义依据').fill('纠正适用条件，后续课堂安全接入')
  const proposed = page.waitForResponse(response => response.url().endsWith('/v1/knowledge/structure/proposals') && response.request().method() === 'POST')
  await panel.getByRole('button', { name: '提交维护提案' }).click()
  const proposal: StructureProposal = await (await proposed).json()
  const review = panel.getByRole('article').filter({ hasText: proposal.id })
  await review.getByLabel('审阅 / 补偿理由').fill('核对适用条件并保留当前题目')
  await review.getByRole('button', { name: '批准知识维护', exact: true }).click()
  await page.getByRole('button', { name: '确认批准知识维护', exact: true }).click()
  await expect(review).toContainText('已应用')
  await expect(panel.getByRole('button', { name: revisedTitle, exact: true })).toBeVisible()
  await page.getByRole('link', { name: '返回原课堂', exact: true }).click()
  await expect(answer).toHaveValue(draft)
  await expect.poll(() => blocks.allTextContents()).toEqual(originalBody)
  expect((await api(page, contextPath, undefined, headers)).base).toEqual(before.base)

  const changes = page.getByRole('region', { name: '教学变更', exact: true })
  const queued = page.waitForResponse(response => /\/changes\/[^/]+$/.test(new URL(response.url()).pathname) && response.request().method() === 'POST')
  await changes.getByRole('button', { name: '接入已审阅知识维护' }).click()
  const response = await queued
  expect(response.ok(), await response.text()).toBe(true)
  const change = await response.json()
  expect(change).toMatchObject({ status: 'queued_for_boundary', policy: 'boundary', base: before.base, candidate: { kind: 'route', trigger: 'authorized_source' } })
  await expect(changes.getByText('已排队，本题处理后应用', { exact: false })).toBeVisible()
  await expect(answer).toHaveValue(draft)
  await expect.poll(() => blocks.allTextContents()).toEqual(originalBody)
  expect((await api(page, contextPath, undefined, headers)).base).toEqual(before.base)
  expect((await fixture.get()).work_item.activity).toEqual(original.work_item.activity)
  await page.getByText('课堂冻结资料与知识', { exact: true }).click()
  await page.getByRole('link', { name: '查看知识、冲突与维护' }).click()
  await page.getByRole('link', { name: '返回原课堂', exact: true }).click()
  await expect(answer).toHaveValue(draft)
})

test('隐私清除使已打开的知识图、列表和详情失效，重新配对与历史返回不恢复正文', async ({ page, browser }) => {
  test.setTimeout(120_000)
  await pair(page)
  const identity = await (await page.request.get('/v1/web/session')).json()
  const conceptID = randomUUID(), targetID = randomUUID()
  const title = `待清除概念 ${conceptID.slice(0, 8)}`, targetTitle = `待清除关系 ${targetID.slice(0, 8)}`
  const description = `隐私正文 ${randomUUID()}`
  const content: ConceptContent = {
    description, source_status: 'candidate', suggested: false,
    sources: [], claims: [], relations: [], replaced_by: [],
  }
  const applied = await applyFixture(page, [
    { concept_id: targetID, goal_id: '', name: targetTitle, content: { ...content, description: '隐私关系的目标正文' } },
    { concept_id: conceptID, goal_id: '', name: title, content: { ...content, relations: [{ target_id: targetID, kind: 'related', suggested: false, sources: [] }] } },
  ])
  const knowledgeURL = `/app/spaces/${space}/knowledge`
  await page.goto(knowledgeURL)
  const panel = page.getByRole('region', { name: '动态知识结构', exact: true })
  await expect(panel.getByRole('button', { name: title, exact: true })).toBeVisible()
  await panel.getByRole('button', { name: '显示局部关系图' }).click()
  await expect(panel.getByRole('button', { name: `图节点：${targetTitle}`, exact: true })).toBeVisible()
  await panel.getByRole('button', { name: `图节点：${title}`, exact: true }).click()
  await expect(panel.getByRole('article', { name: '概念详情' })).toContainText(description)
  await expect(panel.getByRole('img').locator('text').filter({ hasText: /^相关$/ })).toHaveCount(1)

  const eraserContext = await browser.newContext()
  try {
    const eraser = await eraserContext.newPage()
    await pair(eraser, 'memory')
    const eraserIdentity = await (await eraser.request.get('/v1/web/session')).json()
    const grant = serverCommand(['privacy-grant', 'create', '--device', eraserIdentity.device.id])
    await eraser.goto('/app/settings/data')
    await eraser.getByLabel('一次性清除授权').fill(grant)
    const erased = eraser.waitForResponse(response => response.url().endsWith('/v1/privacy/erasures') && response.request().method() === 'POST')
    await eraser.getByRole('button', { name: '发起全局隐私清除', exact: true }).click()
    await eraser.getByRole('button', { name: '确认发起全局隐私清除', exact: true }).click()
    // 等原响应完整返回再操作旧页面，避免取消仍在进行的 owner 清除。
    const response = await erased
    const receipt = await response.json()
    expect(response.status(), JSON.stringify(receipt)).toBe(202)
    for (const store of ['knowledge_content', 'knowledge_index', 'process_cache']) {
      expect(receipt.steps).toContainEqual(expect.objectContaining({ store, status: 'succeeded' }))
    }
    expect((await page.request.get('/v1/web/session')).status()).toBe(401)
    await page.bringToFront()
    await page.evaluate(() => window.dispatchEvent(new Event('focus')))
    await expect(page.getByLabel('配对码', { exact: true })).toBeVisible()
    await expect(panel).toHaveCount(0)
    for (const text of [title, targetTitle, description]) expect(await page.content()).not.toContain(text)

    // 不刷新页面：直接采用新身份，检查仍存活的 QueryClient 不复用旧代次缓存。
    await page.getByLabel('配对码', { exact: true }).fill(serverCommand(['pairing-code', 'create', '--profile', 'references']))
    await page.getByRole('button', { name: '配对并进入' }).click()
    await expect(panel).toBeVisible()
    const repaired = await (await page.request.get('/v1/web/session')).json()
    expect(repaired.generation).toBe(receipt.learner_generation)
    expect(repaired.generation).toBeGreaterThan(identity.generation)
    const assertEmpty = async () => {
      await expect(panel.getByText('当前授权范围没有匹配的概念。资料章节不会自动按标题合并成概念。')).toBeVisible()
      await expect(panel.locator('.structure-list > li')).toHaveCount(0)
      await expect(panel.getByRole('article', { name: '概念详情' })).toHaveCount(0)
      await panel.getByRole('button', { name: '显示局部关系图' }).click()
      await expect(panel.getByRole('img').locator('title, line')).toHaveCount(0)
      await expect(panel.getByRole('button', { name: /^图节点：/ })).toHaveCount(0)
      for (const text of [title, targetTitle, description]) expect(await page.content()).not.toContain(text)
    }
    await assertEmpty()
    const current = await api(page, '/v1/knowledge/structure')
    expect(current).toMatchObject({ generation: receipt.learner_generation, items: [], edges: [] })
    const oldRevision = applied.after.find(n => n.concept_id === conceptID)!.revision_id
    const old = await page.request.get(`/v1/knowledge/structure/concepts/${conceptID}?revision_id=${oldRevision}`, { headers: { 'X-Learning-Space-ID': space } })
    expect(old.status()).toBe(404)
    expect(await old.text()).not.toContain(description)
    await navigateFromMenu(page, '全部学习区')
    await expect(page.getByRole('heading', { name: '你的学习区', exact: true, level: 1 })).toBeVisible()
    await page.goBack()
    await expect(page).toHaveURL(new RegExp(`${knowledgeURL}$`))
    await assertEmpty()
    await page.reload()
    await assertEmpty()
  } finally {
    await eraserContext.close()
  }
})
