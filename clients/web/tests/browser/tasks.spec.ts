import { test, expect, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { reconnectAndWaitForReads } from './network-recovery'

test.use({ actionTimeout: 15000 })

async function login(page: Page) {
  const code = execFileSync(
    '../../server/edu-agentd',
    ['pairing-code', 'create', '--profile', 'import'],
    {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      env: {
        PATH: process.env.PATH,
        DATABASE_URL: process.env.TEST_DATABASE_URL,
        MIGRATE_ON_START: 'true',
      },
    },
  ).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('link', { name: '任务中心', exact: true })).toBeVisible()
}
async function api(
  page: Page,
  space: string,
  collection: string,
  method: string,
  path: string,
  data?: unknown,
) {
  const session = await (await page.request.get('/v1/web/session')).json()
  return page.request.fetch(path, {
    method,
    data,
    headers: {
      Origin: 'http://127.0.0.1:32929',
      'X-CSRF-Token': session.csrf_token,
      'X-Web-Principal-ID': session.device.id,
      'X-Web-Generation': String(session.generation),
      'X-Learning-Space-ID': space,
      ...(path.startsWith('/v1/knowledge/import-jobs') || path === '/v1/knowledge/revisions/head'
        ? { 'X-Knowledge-Collection-ID': collection }
        : {}),
    },
  })
}
async function setup(page: Page) {
  await login(page)
  const created = await api(
    page,
    '00000000-0000-4000-8000-000000000001',
    '00000000-0000-4000-8000-000000000002',
    'POST',
    '/v1/learning-spaces',
    {
      operation_id: randomUUID(),
      expected_version: 0,
      name: '任务验收学习区',
      description: '',
      status: 'active',
    },
  )
  expect(created.ok(), await created.text()).toBe(true)
  const space = (await created.json()).id as string
  const collection = randomUUID()
  const response = await api(page, space, collection, 'POST', '/v1/knowledge/collections', {
    id: collection,
    action: 'create',
    name: '任务验收资料',
    source: 'browser-task-test',
  })
  expect(response.ok(), await response.text()).toBe(true)
  const job = randomUUID()
  const url = `/app/runs/import/${job}?space=${space}&collection=${collection}&draft=true`
  await page.goto(url)
  return { space, collection, job, url }
}
async function confirm(page: Page) {
  await page.getByRole('button', { name: '预览完整剩余计划', exact: true }).click()
  await expect(page.getByLabel(/确认完整清单和计划版本/)).toBeVisible({ timeout: 60000 })
  await page.getByLabel(/确认完整清单和计划版本/).check()
  await page.getByRole('button', { name: '确认完整计划', exact: true }).click()
  await expect(page.getByRole('button', { name: '核对并继续一批', exact: true })).toBeEnabled()
}

test('大清单分段、刷新重启、来源变化与提交响应丢失按原任务恢复', async ({ page, context }) => {
  test.setTimeout(240000)
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const { space, collection, job, url } = await setup(page)
  const files = Array.from({ length: 5 }, (_, i) => ({
    name: `${i}.md`,
    mimeType: 'text/markdown',
    buffer: Buffer.from(`# 文件 ${i}\n${`内容${i} `.repeat(450000)}`),
  }))
  expect(files.reduce((n, f) => n + f.buffer.length, 0)).toBeGreaterThan(16 << 20)
  let lostUpload = false,
    uploadCalls = 0
  await page.route('**/v1/knowledge/import-jobs', async (route) => {
    const body = route.request().postDataJSON()
    if (body?.action === 'upload') {
      uploadCalls++
      expect(route.request().postDataBuffer()!.length).toBeLessThan(16 << 20)
      if (!lostUpload) {
        lostUpload = true
        const response = await route.fetch()
        expect(response.ok()).toBe(true)
        await route.abort('failed')
        return
      }
    }
    await route.continue()
  })
  await page.getByLabel('选择文件', { exact: true }).setInputFiles(files)
  await expect(page.getByText(/已选择 5 \/ 5 项/)).toBeVisible()
  await page.getByRole('button', { name: '创建任务并分段上传' }).click()
  await expect(page.getByText(/请求结果尚未核实/)).toBeVisible()
  expect(uploadCalls).toBe(1)
  await page.unroute('**/v1/knowledge/import-jobs')
  await page.reload()
  await expect(page.getByRole('status')).toContainText('已上传 1')
  await expect(page.getByRole('button', { name: '核对来源并补传缺失部分' })).toBeDisabled()
  const supervisor = JSON.parse(await readFile('/tmp/edu-web-test-server-32929.json', 'utf8'))
  process.kill(supervisor.supervisor, 'SIGUSR2')
  await expect
    .poll(
      async () => JSON.parse(await readFile('/tmp/edu-web-test-server-32929.json', 'utf8')).child,
    )
    .not.toBe(supervisor.child)
  await expect
    .poll(async () => {
      try {
        return (await page.request.get('/livez')).status()
      } catch {
        return 0
      }
    })
    .toBe(200)
  await page.goto(url)
  await expect(page.getByRole('status')).toContainText('已上传 1')
  await page
    .getByLabel('选择文件', { exact: true })
    .setInputFiles([{ ...files[0], buffer: Buffer.from('# 已变化') }, ...files.slice(1)])
  await expect(page.getByRole('alert')).toContainText('摘要变化')
  await expect(page.getByRole('button', { name: '核对来源并补传缺失部分' })).toBeDisabled()
  await page.getByLabel('选择文件', { exact: true }).setInputFiles(files)
  await expect(page.getByRole('button', { name: '核对来源并补传缺失部分' })).toBeEnabled()
  await page.getByRole('button', { name: '核对来源并补传缺失部分' }).click()
  await expect(page.getByRole('status')).toContainText('已上传 5', { timeout: 60000 })
  await confirm(page)
  const before = await api(page, space, collection, 'GET', '/v1/knowledge/revisions/head')
  expect(before.status()).toBe(404)
  let lostCommit = false
  await page.route('**/v1/knowledge/import-jobs', async (route) => {
    if (route.request().postDataJSON()?.action === 'continue' && !lostCommit) {
      lostCommit = true
      const response = await route.fetch()
      expect(response.ok()).toBe(true)
      await route.abort('failed')
    } else await route.continue()
  })
  await page.getByRole('button', { name: '核对并逐批继续', exact: true }).click()
  await expect(page.getByText(/请求结果尚未核实/)).toBeVisible()
  await page.unroute('**/v1/knowledge/import-jobs')
  await page.getByRole('button', { name: '核对原操作', exact: true }).click()
  await expect(page.getByRole('status')).toContainText('已发布 1')
  await page.getByRole('button', { name: '核对并逐批继续', exact: true }).click()
  await expect(page.getByRole('status')).toContainText('已发布 5 · 未处理 0', { timeout: 60000 })
  const final = await (
    await api(page, space, collection, 'GET', `/v1/knowledge/import-jobs/${job}`)
  ).json()
  expect(final.status).toBe('completed')
  expect(new Set(final.batches.map((b: { operation_id: string }) => b.operation_id)).size).toBe(5)
  const head = await (
    await api(page, space, collection, 'GET', '/v1/knowledge/revisions/head')
  ).json()
  expect(head.revision.revision_no).toBe(5)
  await reconnectAndWaitForReads(page, context, `/v1/knowledge/import-jobs/${job}`)
  await page.reload()
  await expect(page.getByRole('status')).toContainText('已发布 5')
  await page.getByRole('link', { name: '返回任务中心' }).click()
  await expect(page.getByRole('article').filter({ hasText: job })).toContainText('已发布 5')
  for (const width of [390, 768, 1280]) {
    await page.setViewportSize({ width, height: 1000 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }
  expect(
    await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage })),
  ).not.toContain('内容0')
  expect(errors).toEqual([])
})

test('取消保留已发布、归档阻止继续、另一设备不能接管原任务', async ({ page, browser }) => {
  const { space, collection, job, url } = await setup(page)
  const files = [0, 1].map((i) => ({
    name: `${i}.md`,
    mimeType: 'text/markdown',
    buffer: Buffer.from(`# 资料${i}\n不同主题${i}`),
  }))
  await page.getByLabel('选择文件', { exact: true }).setInputFiles(files)
  await expect(page.getByText(/已选择 2 \/ 2 项/)).toBeVisible()
  await page.getByRole('button', { name: '创建任务并分段上传' }).click()
  await expect(page.getByRole('status')).toContainText('已上传 2')
  await confirm(page)
  await page.getByRole('button', { name: '核对并继续一批', exact: true }).click()
  await expect(page.getByRole('status')).toContainText('已发布 1')
  const other = await browser.newContext({ baseURL: 'http://127.0.0.1:32929' })
  const otherPage = await other.newPage()
  await login(otherPage)
  const denied = await api(otherPage, space, collection, 'POST', '/v1/knowledge/import-jobs', {
    id: job,
    action: 'continue',
  })
  expect(denied.status()).toBe(404)
  await other.close()
  const info = await (
    await api(page, space, collection, 'GET', `/v1/learning-spaces/${space}`)
  ).json()
  const archived = await api(page, space, collection, 'PUT', `/v1/learning-spaces/${space}`, {
    operation_id: randomUUID(),
    expected_version: info.version,
    name: info.name,
    description: info.description,
    status: 'archived',
  })
  expect(archived.ok(), await archived.text()).toBe(true)
  await page.goto(url)
  await expect(page.getByRole('button', { name: '核对并继续一批', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: '取消后续批次', exact: true }).click()
  await page.getByRole('button', { name: '确认取消后续批次', exact: true }).click()
  await expect(page.getByRole('heading', { name: '已取消后续批次', exact: true })).toBeVisible()
  await expect(page.getByRole('status')).toContainText('已发布 1 · 未处理 1')
  const final = await (
    await api(page, space, collection, 'GET', `/v1/knowledge/import-jobs/${job}`)
  ).json()
  expect(final.batches.filter((b: { status: string }) => b.status === 'completed')).toHaveLength(1)
  expect(final.cleanup_pending).toBe(false)
})
