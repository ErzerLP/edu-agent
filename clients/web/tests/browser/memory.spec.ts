import { test, expect, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'
import type { MemoryRecord } from '../../src/api/memory'

const env = () => ({ PATH: process.env.PATH, DATABASE_URL: process.env.TEST_DATABASE_URL, MIGRATE_ON_START: 'true' })
async function pair(page: Page, profile = 'memory') {
  const code = execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', profile], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], env: env() }).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  const paired = page.waitForResponse(response => response.url().endsWith('/v1/web/pairings') && response.request().method() === 'POST')
  await page.getByRole('button', { name: '配对并进入' }).click()
  expect((await paired).status()).toBe(201)
  await expect(page.getByRole('heading', { name: '你的学习区', exact: true, level: 1 })).toBeVisible()
  // 首页标题早于后台查询完成；WebKit 会把硬导航取消的 fetch 上报为 access control pageerror。
  // 首页没有活动订阅，等待有限请求稳定后再离开，保留后续全部 pageerror 断言。
  await page.waitForLoadState('networkidle')
}
async function confirm(page: Page, label: string) {
  await page.getByRole('button', { name: label, exact: true }).click()
  const dialog = page.getByRole('alertdialog')
  await expect(dialog.getByRole('button', { name: '取消', exact: true })).toBeFocused()
  await dialog.getByRole('button', { name: `确认${label}`, exact: true }).click()
}

const remoteURL = 'http://127.0.0.1:32932/fixture'
async function remote(page: Page, data?: { available?: boolean; failCapabilities?: 1 }) {
  const response = data ? await page.request.post(remoteURL, { data }) : await page.request.get(remoteURL)
  expect(response.ok()).toBe(true)
  return response.json()
}
test.afterEach(async ({ page }) => {
  if (process.env.WEB_NOCTURNE_FIXTURE === '1') await remote(page, { available: true })
})

async function createRecord(page: Page, content: string, failOnce = false) {
  await page.goto('/app/memory')
  await page.getByRole('button', { name: '新建偏好候选', exact: true }).click()
  await page.getByLabel('具体内容', { exact: true }).fill(content)
  await page.getByLabel('保存或纠正理由').fill('稳定偏好，远端浏览器验收')
  await page.getByRole('button', { name: '创建待审阅候选' }).click()
  await page.getByRole('link', { name: '查看具体候选并审批' }).click()
  await expect(page.getByRole('region', { name: '记忆候选详情' })).toContainText(content)
  if (failOnce) await remote(page, { failCapabilities: 1 })
  const decided = page.waitForResponse(response => response.url().endsWith('/decisions') && response.request().method() === 'POST')
  await confirm(page, '批准此条记忆')
  const response = await decided
  expect(response.status()).toBe(200)
  const result = await response.json()
  await page.getByRole('link', { name: '查看真实记录与交付回执' }).click()
  await expect(page.getByRole('button', { name: '刷新交付回执', exact: true })).toBeVisible()
  return result
}

async function delivered(page: Page, status: 'applied' | 'deleted' = 'applied') {
  const id = new URL(page.url()).searchParams.get('record')!
  let value: MemoryRecord | undefined
  await expect.poll(async () => {
    const refreshed = page.waitForResponse(response => new URL(response.url()).pathname === `/v1/memory/records/${id}`)
    await page.getByRole('button', { name: '刷新交付回执', exact: true }).click()
    const response = await refreshed
    expect(response.ok()).toBe(true)
    value = await response.json()
    return value!.record.status
  }, { timeout: 10000 }).toBe(status)
  if (status === 'applied') await expect(page.getByText(/交付：远端已确认/)).toBeVisible()
  else await expect(page.getByText(/删除\/保存回执：已验证完成.*nocturne_complete_reference_absence/)).toBeVisible()
  await expect(page.getByText(`回执 ID：${value!.receipt.receipt_id}`, { exact: true })).toBeVisible()
  expect(value!.receipt.status).toBe('succeeded')
  return value!
}

async function downloadMemory(page: Page) {
  const downloaded = page.waitForEvent('download')
  await confirm(page, '导出本页长期记忆')
  const stream = await (await downloaded).createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream!) chunks.push(Buffer.from(chunk))
  const body = Buffer.concat(chunks)
  return { body, value: JSON.parse(body.toString('utf8')) }
}

function noKeys(text: string) {
  for (const key of ['browser-nocturne-fixture-only-api-token', Buffer.alloc(32, 0x34).toString('base64url'), Buffer.alloc(32, 0x35).toString('base64url')]) expect(text).not.toContain(key)
  expect(text).not.toMatch(/"(?:api_key|search_key|api_token|maintenance_token|master_wrapping_key)"\s*:/i)
}
function checkServerLog() {
  const { serverLog } = JSON.parse(readFileSync('/tmp/edu-web-test-server-32929.json', 'utf8'))
  const log = readFileSync(serverLog, 'utf8')
  expect(log.length).toBeGreaterThan(0)
  noKeys(log)
}

test('记忆候选显式批准、拒绝、真实排队导出与移动焦点', async ({ page }) => {
  await pair(page)
  if (process.env.WEB_NOCTURNE_FIXTURE === '1') {
    // 已配置远端时，queued 条目本身是 unavailable，整页 degraded 由已交付正文读取失败触发。
    // 先交付一条独立记录，再制造远端不可用，保留原降级导出和未交付纠正禁用断言。
    await createRecord(page, `降级导出的已交付对照 ${randomUUID()}`)
    await delivered(page)
    await remote(page, { available: false })
  }
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  await page.goto('/app/memory')
  await expect(page.getByText(/未按学习区隔离/).first()).toBeVisible()
  await page.getByRole('button', { name: '新建偏好候选', exact: true }).click()
  const content = `我偏好分步骤解释 ${randomUUID()}`
  await page.getByLabel('具体内容', { exact: true }).fill(content)
  await page.getByLabel('保存或纠正理由').fill('稳定偏好，逐条审阅')
  await page.getByRole('button', { name: '创建待审阅候选' }).click()
  await page.getByRole('link', { name: '查看具体候选并审批' }).click()
  const candidateURL = page.url()
  const review = page.getByRole('region', { name: '记忆候选详情' })
  await expect(review).toContainText(content)
  await expect(review).toContainText('待明确批准')
  for (const width of [390, 768, 1280]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }
  await page.getByRole('button', { name: '批准此条记忆', exact: true }).click()
  await expect(page.getByRole('alertdialog').getByRole('button', { name: '取消', exact: true })).toBeFocused()
  await page.getByRole('button', { name: '取消', exact: true }).click()
  await page.reload()
  await expect(review).toContainText('待明确批准')
  await confirm(page, '批准此条记忆')
  await expect(review.getByRole('link', { name: '查看真实记录与交付回执' })).toBeVisible()
  await review.getByRole('link', { name: '查看真实记录与交付回执' }).click()
  await expect(page.getByText('仍在等待，不宣称完成', { exact: false })).toBeVisible()
  await expect(page.getByRole('button', { name: '提出纠正', exact: true })).toBeDisabled()
  const downloaded = page.waitForEvent('download')
  await confirm(page, '导出本页长期记忆')
  const download = await downloaded
  const stream = await download.createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream!) chunks.push(Buffer.from(chunk))
  const exported = JSON.parse(Buffer.concat(chunks).toString())
  expect(exported.scope).toContain('全局')
  expect(exported.degraded).toBe(true)
  expect(JSON.stringify(exported)).not.toContain(content)
  await page.goto('/app/memory')
  await page.getByRole('button', { name: '新建偏好候选', exact: true }).click()
  await page.getByLabel('具体内容', { exact: true }).fill('拒绝的候选不应成为记忆')
  await page.getByLabel('保存或纠正理由').fill('测试拒绝')
  await page.getByRole('button', { name: '创建待审阅候选' }).click()
  await page.getByRole('link', { name: '查看具体候选并审批' }).click()
  await confirm(page, '拒绝此候选')
  await expect(review).not.toContainText('拒绝的候选不应成为记忆')
  await page.goto(candidateURL)
  await expect(page.getByRole('button', { name: '批准此条记忆', exact: true })).toBeDisabled()
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toContain(content)
  expect(errors).toEqual([])
})

test('设备撤销即时失效、隐私清除重新配对核对原回执', async ({ page, browser }) => {
  await pair(page)
  const session = await (await page.request.get('/v1/web/session')).json()
  const saveGoal = async (name: string) => {
    const identity = await (await page.request.get('/v1/web/session')).json()
    const response = await page.request.post('/v1/learning/goals', {
      headers: {
        Origin: 'http://127.0.0.1:32929',
        'X-CSRF-Token': identity.csrf_token,
        'X-Web-Principal-ID': identity.device.id,
        'X-Web-Generation': String(identity.generation),
        'X-Learning-Space-ID': '00000000-0000-4000-8000-000000000001',
      },
      data: {
        operation_id: randomUUID(),
        payload_schema_version: 1,
        aggregate_type: 'goal',
        aggregate_id: randomUUID(),
        expected_version: 0,
        text: name,
        source: '清除后 Studio 验收',
        details: { name },
      },
    })
    expect(response.ok(), await response.text()).toBe(true)
    return (await response.json()).result
  }
  await saveGoal('清除前的旧教学目标')
  const other = await browser.newContext()
  const victim = await other.newPage()
  await pair(victim)
  const victimSession = await (await victim.request.get('/v1/web/session')).json()
  await page.goto('/app/settings/devices')
  const device = page.locator('article').filter({ hasText: victimSession.device.id })
  await device.getByRole('button', { name: '撤销设备', exact: true }).click()
  await page.getByRole('button', { name: '确认撤销设备', exact: true }).click()
  await expect.poll(async () => (await victim.request.get('/v1/web/session')).status()).toBe(401)
  await other.close()
  const grant = execFileSync('../../server/edu-agentd', ['privacy-grant', 'create', '--device', session.device.id], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], env: env() }).trim()
  await page.goto('/app/settings/data')
  await page.getByLabel('一次性清除授权').fill(grant)
  const erased = page.waitForResponse(response => response.url().endsWith('/v1/privacy/erasures') && response.request().method() === 'POST')
  await confirm(page, '发起全局隐私清除')
  await expect(page).toHaveURL(/operation=.*device=/)
  const receiptURL = page.url()
  // 旧会话失效早于本地清除完成；先读完原响应，避免刷新取消其余 owner 的清除。
  const response = await erased
  const receipt = await response.json()
  expect(response.status(), JSON.stringify(receipt)).toBe(202)
  for (const store of ['identity_metadata', 'knowledge_content', 'learning_event_payload']) {
    expect(receipt.steps).toContainEqual(expect.objectContaining({ store, status: 'succeeded' }))
  }
  await expect.poll(async () => (await page.request.get('/v1/web/session')).status()).toBe(401)
  await page.reload()
  await pair(page)
  const repaired = await (await page.request.get('/v1/web/session')).json()
  expect(repaired.device.id).not.toBe(session.device.id)
  expect(repaired.generation).toBe(receipt.learner_generation)
  expect(repaired.generation).toBeGreaterThan(session.generation)
  await page.goto(receiptURL)
  await expect(page.getByRole('region', { name: '隐私清除回执' })).toBeVisible()
  await expect(page.getByRole('region', { name: '隐私清除回执' })).toContainText(receipt.erasure_id)
  await expect(page.getByRole('region', { name: '隐私清除回执' })).toContainText('不在物理擦除保证内')
  expect(await page.content()).not.toContain(grant)
  const goal = await saveGoal('清除后新建的教学目标')
  await page.goto('/app/spaces/00000000-0000-4000-8000-000000000001/studio')
  await page.getByLabel('关联目标').selectOption(goal.goal_id)
  await expect(page.getByLabel('关联目标')).toHaveValue(goal.goal_id)
  await expect(page.getByRole('alert')).toHaveCount(0)
  await expect(page.getByRole('option', { name: '[redacted]', exact: true })).toHaveCount(0)
})

test('导师申请必须在内嵌候选面板批准才可取得回执', async ({ page, context }) => {
  test.skip(process.env.WEB_MENTOR_FIXTURE !== '1', '需本地确定性导师 fixture')
  await pair(page, 'settings')
  await page.goto('/app/settings')
  const model = page.getByRole('region', { name: 'Web 导师模型', exact: true })
  await model.getByRole('button', { name: '配置或替换凭据' }).click()
  await model.getByLabel('端点', { exact: true }).fill('http://127.0.0.1:32930/v1')
  await model.getByLabel('模型名称').fill('browser-memory-fixture')
  await model.getByLabel('鉴权方式').selectOption('none')
  await model.getByLabel('启用Web 导师模型').check()
  await model.getByRole('button', { name: '保存Web 导师模型配置' }).click()
  await expect(page.getByText('配置已保存。', { exact: true })).toBeVisible()
  await context.clearCookies()
  await pair(page)
  // 前一个隐私清除场景会擦除区名；按原区 ID 进入，不能依赖旧的默认名称。
  await page.goto('/app/spaces/00000000-0000-4000-8000-000000000001')
  await page.getByLabel('今天想学会什么？', { exact: true }).fill(`记忆导师 ${randomUUID()}`)
  await page.getByRole('button', { name: '仅保存目标', exact: true }).click()
  await page.getByRole('link', { name: '查看目标', exact: true }).click()
  await page.getByRole('button', { name: '新建对话', exact: true }).click()
  const panel = page.getByRole('region', { name: '目标内导师', exact: true })
  await panel.getByLabel('想和导师讨论什么？').fill('申请保存长期偏好：我长期偏好先看直觉再看公式')
  await panel.getByRole('button', { name: '发起导师交流' }).click()
  await expect(panel.getByRole('region', { name: '记忆候选详情' })).toContainText('我长期偏好先看直觉再看公式')
  await confirm(page, '批准此条记忆')
  await panel.getByRole('button', { name: '已在记忆面板完成审批，核对回执', exact: true }).click()
  await expect(panel.getByLabel('导师输出')).toContainText('浏览器真实导师')
})

test('已交付记忆的纠正审批、正文导出与完整远端删除', async ({ page }, testInfo) => {
  test.skip(process.env.WEB_NOCTURNE_FIXTURE !== '1', '需要确定性 Nocturne 远端')
  await pair(page)
  const errors: string[] = []
  page.on('pageerror', error => errors.push(error.message))
  const original = `我长期偏好先画图再推导 ${randomUUID()}`
  const corrected = `我长期偏好先举例再画图 ${randomUUID()}`
  const accepted = await createRecord(page, original)
  const saved = await delivered(page)
  const id = saved.record.logical_memory_id
  expect(saved.content).toBe(original)
  expect(saved.content_status).toBe('available')
  expect(saved.receipt.verification_method).toBe('nocturne_uri_content_sha256')
  expect(saved.delivery.delivery_id).toBe(accepted.delivery.delivery_id)
  await expect(page.getByRole('button', { name: '提出纠正', exact: true })).toBeEnabled()
  const before = await remote(page)
  const node = before.paths.find((value: { path: string }) => value.path === `edu-agent/${id}`)
  expect(node.content).toBe(original)
  await page.getByRole('button', { name: '提出纠正', exact: true }).click()
  await expect(page.getByRole('textbox', { name: '具体内容', exact: true })).toHaveValue(original)
  await page.getByRole('textbox', { name: '具体内容', exact: true }).fill(corrected)
  await page.getByLabel('保存或纠正理由').fill('审阅原正文后逐条纠正')
  await page.getByRole('button', { name: '创建待审阅候选' }).click()
  const proposed = await remote(page)
  expect(proposed.writes).toBe(before.writes)
  expect(proposed.paths.find((value: { path: string }) => value.path === node.path).content).toBe(original)
  const recordDetail = await (await page.request.get(`/v1/memory/records/${id}`)).json()
  expect(recordDetail.content).toBe(original)
  await page.getByRole('link', { name: '查看具体候选并审批' }).click()
  const review = page.getByRole('region', { name: '记忆候选详情' })
  await expect(review).toContainText('待明确批准')
  await expect(review).toContainText(corrected)
  await expect(review.getByRole('button', { name: '批准此条记忆', exact: true })).toBeEnabled()
  await confirm(page, '批准此条记忆')
  await review.getByRole('link', { name: '查看真实记录与交付回执' }).click()
  const updated = await delivered(page)
  expect(updated.record.logical_memory_id).toBe(id)
  expect(updated.record.revision).toBe(saved.record.revision + 1)
  expect(updated.content).toBe(corrected)
  expect(updated.delivery.kind).toBe('correction')
  expect(updated.receipt.receipt_id).not.toBe(saved.receipt.receipt_id)
  const after = await remote(page)
  expect(after.writes).toBe(before.writes + 1)
  expect(after.paths.find((value: { path: string }) => value.path === node.path)).toMatchObject({ node_uuid: node.node_uuid, content: corrected })
  expect(after.versions.filter((value: { node_uuid: string }) => value.node_uuid === node.node_uuid)).toHaveLength(2)
  const exported = await downloadMemory(page)
  expect(exported.value.degraded).toBe(false)
  expect(exported.value.items.find((item: { record: { logical_memory_id: string } }) => item.record.logical_memory_id === id)).toMatchObject({ content_status: 'available', content: corrected, delivery_status: 'applied', receipt: { receipt_id: updated.receipt.receipt_id, status: 'succeeded' } })
  expect(exported.body.toString()).not.toContain(original)
  noKeys(exported.body.toString())
  await testInfo.attach('已交付记忆正文导出', { body: exported.body, contentType: 'application/json' })
  await confirm(page, '删除此条记忆')
  const removed = await delivered(page, 'deleted')
  expect(removed.content).toBeUndefined()
  expect(removed.content_status).toBe('unavailable')
  expect(removed.receipt.reason).toBe('remote_logical_delete_verified')
  expect(removed.receipt.verification_method).toBe('nocturne_complete_reference_absence')
  await expect(page.getByRole('button', { name: '提出纠正', exact: true })).toBeDisabled()
  await expect(page.getByRole('button', { name: '删除此条记忆', exact: true })).toBeDisabled()
  const deleted = await remote(page)
  expect(deleted.paths.filter((value: { path: string }) => value.path === node.path)).toEqual([])
  expect(deleted.versions.filter((value: { node_uuid: string }) => value.node_uuid === node.node_uuid)).toEqual([])
  await page.reload()
  await expect(page.locator('section').filter({ has: page.getByRole('heading', { name: '已保存记录详情', exact: true }) })).toContainText('已验证删除 · 版本')
  expect(await page.locator('body').innerText()).not.toContain(corrected)
  noKeys(await page.locator('body').innerText())
  checkServerLog()
  expect(errors).toEqual([])
  await testInfo.attach('纠正与删除正式回执', { body: JSON.stringify({ saved, updated, removed, remote: deleted }), contentType: 'application/json' })
})

test('远端一次维护故障后浏览器重放沿用原交付且不重复写入', async ({ page }, testInfo) => {
  test.skip(process.env.WEB_NOCTURNE_FIXTURE !== '1', '需要确定性 Nocturne 远端')
  await pair(page)
  const errors: string[] = []
  page.on('pageerror', error => errors.push(error.message))
  const before = await remote(page)
  const content = `我长期偏好每步核对理解 ${randomUUID()}`
  const accepted = await createRecord(page, content, true)
  const id = accepted.record.logical_memory_id
  await expect.poll(async () => (await remote(page)).failures).toBe(before.failures + 1)
  const failed = await (await page.request.get(`/v1/memory/records/${id}`)).json()
  expect(failed.record.status).toBe('queued')
  expect(failed.receipt.status).toBe('pending')
  expect(failed.delivery.attempt_count).toBe(1)
  const outage = await remote(page)
  expect(outage.failures).toBe(before.failures + 1)
  expect(outage.writes).toBe(before.writes)
  await page.getByRole('button', { name: '刷新交付回执', exact: true }).click()
  await expect(page.getByText(/仍在等待，不宣称完成/)).toBeVisible()
  const replayed = page.waitForResponse(response => response.url().endsWith(`/deliveries/${accepted.delivery.delivery_id}/replays`) && response.request().method() === 'POST')
  await confirm(page, '重放交付')
  const response = await replayed
  expect(response.status()).toBe(202)
  const replay = await response.json()
  expect(replay.record.logical_memory_id).toBe(id)
  expect(replay.delivery.delivery_id).toBe(accepted.delivery.delivery_id)
  const saved = await delivered(page)
  expect(saved.content).toBe(content)
  expect(saved.delivery.delivery_id).toBe(accepted.delivery.delivery_id)
  expect(saved.record.revision).toBe(accepted.record.revision)
  await expect(page.getByRole('button', { name: '重放交付', exact: true })).toBeDisabled()
  // 丢响应后的相同正式请求重发：用原 Cookie/CSRF/操作 ID 核对归档结果，不重复写入。
  const headers = await response.request().allHeaders()
  const duplicate = await page.request.post(response.url(), { headers, data: response.request().postDataJSON() })
  expect(duplicate.status()).toBe(200)
  const duplicateResult = await duplicate.json()
  expect(duplicateResult.replayed).toBe(true)
  expect(duplicateResult.delivery.delivery_id).toBe(saved.delivery.delivery_id)
  const denied = await page.request.post(response.url(), { headers, data: { ...response.request().postDataJSON(), operation_id: randomUUID() } })
  expect(denied.status()).toBe(409)
  noKeys(await denied.text())
  const after = await remote(page)
  expect(after.writes).toBe(before.writes + 1)
  const node = after.paths.find((value: { path: string }) => value.path === `edu-agent/${id}`)
  expect(node.content).toBe(content)
  expect(after.versions.filter((value: { node_uuid: string }) => value.node_uuid === node.node_uuid)).toHaveLength(1)
  const exported = await downloadMemory(page)
  expect(exported.value.degraded).toBe(false)
  expect(exported.body.toString()).toContain(content)
  for (const text of [exported.body.toString(), JSON.stringify(failed), JSON.stringify(replay), JSON.stringify(duplicateResult), await page.locator('body').innerText()]) noKeys(text)
  checkServerLog()
  expect(errors).toEqual([])
  await testInfo.attach('故障与重放原身份核对', { body: JSON.stringify({ failed, replay, saved, duplicateResult, remote: after }), contentType: 'application/json' })
})
