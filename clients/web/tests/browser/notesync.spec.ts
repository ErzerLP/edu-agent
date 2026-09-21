import { test, expect, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'

const space = '00000000-0000-4000-8000-000000000001'
const collection = '00000000-0000-4000-8000-000000000002'
const fixture = 'http://127.0.0.1:32931/fixture'
test.skip(process.env.WEB_NOTESYNC_FIXTURE !== '1', '需要显式启用固定 NoteSync 合同 fixture')

async function login(page: Page, profile = 'import') {
  const code = execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', profile], {
    encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    env: { PATH: process.env.PATH, DATABASE_URL: process.env.TEST_DATABASE_URL, MIGRATE_ON_START: 'true' },
  }).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('link', { name: '任务中心', exact: true })).toBeVisible()
}
async function api(page: Page, method: string, path: string, data?: unknown, scope = space, source = collection) {
  const session = await (await page.request.get('/v1/web/session')).json()
  return page.request.fetch(path, { method, data, headers: {
    Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token,
    'X-Web-Principal-ID': session.device.id, 'X-Web-Generation': String(session.generation),
    'X-Learning-Space-ID': scope,
    ...(path.startsWith('/v1/knowledge/notesync/') || path.startsWith('/v1/knowledge/revisions/') || path === '/v1/knowledge/imports' ? { 'X-Knowledge-Collection-ID': source } : {}),
  } })
}

test('真实学习身份、预览差异、过期、未知结果核对和解除引用', async ({ page, browser }) => {
  test.setTimeout(120000)
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  await login(page)
  await api(page, 'POST', '/v1/knowledge/collections', { id: collection, action: 'link' })
  const initialHead = await api(page, 'GET', '/v1/knowledge/revisions/head')
  expect([200, 404]).toContain(initialHead.status())
  const parent = initialHead.ok() ? (await initialHead.json()).revision.revision_id : null
  const body = `# 同步资料\nshared lesson body uses enough stable words for identity continuity alpha state\n验收批次 ${randomUUID()}\n`
  // 模拟前一引擎留下的相似正文；随机路径本身不构成新身份授权。
  const previousPath = `历史同步验收-${randomUUID()}.md`
  const previous = await api(page, 'POST', '/v1/knowledge/imports', {
    operation_id: randomUUID(), expected_parent_revision_id: parent, source: '浏览器同步历史准备',
    documents: [{ path: previousPath, markdown: body.replace('alpha state', 'gamma state'), as_new: true }],
  })
  expect(previous.ok(), await previous.text()).toBe(true)
  const previousRevision = (await previous.json()).revision
  const path = `同步验收-${randomUUID()}.md`
  const ambiguous = await api(page, 'POST', '/v1/knowledge/imports', {
    operation_id: randomUUID(), expected_parent_revision_id: previousRevision.revision_id, source: '浏览器同步身份保护验收',
    documents: [{ path, markdown: body }],
  })
  expect(ambiguous.status()).toBe(409)
  expect(await ambiguous.json()).toMatchObject({ error: { code: 'identity_review_required' }, identity_review: {
    document_reviews: expect.arrayContaining([expect.objectContaining({ path, reason_code: 'document_match_ambiguous' })]),
  } })
  const imported = await api(page, 'POST', '/v1/knowledge/imports', {
    operation_id: randomUUID(), expected_parent_revision_id: previousRevision.revision_id, source: '浏览器同步验收',
    documents: [{ path, markdown: body, as_new: true }],
  })
  expect(imported.ok(), await imported.text()).toBe(true)
  const currentRevision = (await imported.json()).revision
  const document = (documentPath: string) => currentRevision.documents.find((d: { path: string }) => d.path === documentPath).document
  expect(document(path).document_id).not.toBe(document(previousPath).document_id)
  expect(document(previousPath).document_id).toBe(previousRevision.documents.find((d: { path: string }) => d.path === previousPath).document.document_id)
  const revision = currentRevision.revision_id
  const exported = await api(page, 'GET', `/v1/knowledge/revisions/${revision}/export`)
  expect(exported.ok()).toBe(true)
  const markdown = (await exported.json()).documents.find((d: { path: string }) => d.path === path).markdown as string
  const remotePath = `edu-agent/${path}`
  const remote = markdown.replace('alpha state', 'beta state')
  await page.request.post(fixture, { data: { path: remotePath, content: remote } })
  const initialWrites = (await (await page.request.get(fixture)).json()).data.writes
  await page.goto(`/app/spaces/${space}/notesync?collection=${collection}`)
  await expect(page.getByText('连接可用', { exact: false })).toBeVisible()
  await expect(page.getByText('服务端环境配置', { exact: false })).toBeVisible()
  await page.getByLabel('远端路径（留空分页扫描）').fill(remotePath)
  await page.getByRole('button', { name: '生成同步预览', exact: true }).click()
  await page.getByRole('link', { name: '打开预览审阅', exact: true }).click()
  await expect(page.getByRole('heading', { name: remotePath, exact: true })).toBeVisible()
  const firstURL = page.url()
  await expect(page.locator('#sync-local pre')).toContainText('alpha state')
  await expect(page.locator('#sync-remote pre')).toContainText('beta state')
  await page.getByRole('button', { name: '预览实际影响与身份', exact: true }).click()
  await expect(page.getByRole('button', { name: '确认正式解决', exact: true })).toBeEnabled()
  expect((await (await page.request.get(fixture)).json()).data.writes).toBe(initialWrites)
  const head = await api(page, 'GET', '/v1/knowledge/revisions/head')
  expect((await head.json()).revision.revision_id).toBe(revision)

  // 原远端版本已变化，旧批准只能失败，原差异仍可对照。
  await page.request.post(fixture, { data: { path: remotePath, content: markdown.replace('alpha state', 'gamma state') } })
  await page.getByRole('button', { name: '确认正式解决', exact: true }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: '确认确认正式解决', exact: true }).click()
  await expect(page.getByText('审阅已过期', { exact: false })).toBeVisible()
  await expect(page.locator('#sync-remote pre')).toContainText('beta state')
  await page.getByRole('button', { name: '重新预览当前路径', exact: true }).click()
  await page.getByRole('link', { name: '打开预览审阅', exact: true }).click()
  expect(page.url()).not.toBe(firstURL)
  const reviewURL = page.url()
  const reviewID = new URL(reviewURL).pathname.split('/').at(-1)!
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('tab', { name: '远端正文', exact: true }).click()
  await expect(page.locator('#sync-remote pre')).toBeVisible()
  await expect(page.locator('#sync-local')).not.toBeVisible()
  await page.screenshot({ path: 'test-results/notesync-mobile.png', fullPage: true })
  await page.setViewportSize({ width: 1440, height: 1000 })
  await expect(page.locator('#sync-local pre')).toBeVisible()
  await page.screenshot({ path: 'test-results/notesync-desktop.png', fullPage: true })

  const readerContext = await browser.newContext()
  const reader = await readerContext.newPage()
  await login(reader, 'user')
  await reader.goto(reviewURL)
  await expect(reader.getByText('当前身份可以查看和预览', { exact: false })).toBeVisible()
  await expect(reader.getByRole('button', { name: '确认正式解决', exact: true })).toHaveCount(0)
  expect((await api(reader, 'POST', `/v1/knowledge/notesync/reviews/${reviewID}/resolutions`, {})).status()).toBe(403)
  await readerContext.close()

  await page.getByRole('button', { name: '预览实际影响与身份', exact: true }).click()
  await expect(page.getByRole('button', { name: '确认正式解决', exact: true })).toBeEnabled()
  let resolves = 0
  await page.route('**/notesync/reviews/*/resolutions', async route => {
    resolves++
    const response = await route.fetch()
    expect(response.ok(), await response.text()).toBe(true)
    await route.abort('failed')
  })
  await page.getByRole('button', { name: '确认正式解决', exact: true }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: '确认确认正式解决', exact: true }).click()
  await expect(page.getByText('解决结果未知', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: '核对原操作结果', exact: true }).click()
  await expect(page.getByText('原操作已核对', { exact: false })).toBeVisible()
  expect(resolves).toBe(1)
  await page.reload()
  await expect(page.getByText('原操作已核对', { exact: false })).toBeVisible()
  expect(resolves).toBe(1)
  const old = await api(page, 'GET', `/v1/knowledge/revisions/${revision}/export`)
  expect((await old.json()).documents.find((d: { path: string }) => d.path === path).markdown).toBe(markdown)
  await page.goto(`/app/runs?space=${space}`)
  await page.getByLabel('任务类型').selectOption('notesync')
  // 按原审阅身份核对卡片，避免命中状态筛选选项或其他审阅。
  const resolvedTask = page.getByRole('region', { name: '同步审阅列表', exact: true })
    .getByRole('article').filter({ has: page.locator(`a[href*="/notesync/${reviewID}"]`) })
  await expect(resolvedTask.getByText('已解决（resolved）', { exact: false })).toBeVisible()
  await resolvedTask.getByRole('link', { name: '查看同一同步审阅', exact: true }).click()
  await expect(page).toHaveURL(reviewURL)
  await expect(page.locator('#sync-remote pre')).toContainText('gamma state')
  const unlinked = await api(page, 'POST', '/v1/knowledge/collections', { id: collection, action: 'unlink' })
  expect(unlinked.ok(), await unlinked.text()).toBe(true)
  await page.getByRole('button', { name: '刷新同步状态', exact: true }).click()
  await expect(page.getByText('映射、引用或审阅不可用', { exact: false })).toBeVisible()
  await expect(page.locator('#sync-remote')).toHaveCount(0)
  expect((await api(page, 'GET', `/v1/knowledge/notesync/reviews/${reviewID}`)).status()).toBe(404)
  expect((await api(page, 'GET', '/v1/knowledge/notesync/status', undefined, space, randomUUID())).status()).toBe(404)
  await page.reload()
  await expect(page.locator('#sync-remote')).toHaveCount(0)
  expect(errors).toEqual([])
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toContain('gamma state')
  await api(page, 'POST', '/v1/knowledge/collections', { id: collection, action: 'link' })
})
