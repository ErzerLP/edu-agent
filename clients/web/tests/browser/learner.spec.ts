import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { pairNotebook, createSpace, webAPI } from './notebook-fixture'

test('学习端全权一次配对完成文本导入、Studio 笔记与记忆候选，设置和清除权限独立', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', error => errors.push(error.message))
  let pairings = 0
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/v1/web/pairings')) pairings++
  })
  await pairNotebook(page, 'learner')
  const session = await (await page.request.get('/v1/web/session')).json()
  expect(session.capabilities).toMatchObject({ references: true, import_jobs: true, save_goal: true })
  const space = await createSpace(page, '一次配对学习区')
  await page.goto(`/app/spaces/${space.id}?panel=sources`)
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await sources.getByRole('button', { name: '＋ 添加来源', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '添加来源', exact: true })
  await dialog.getByLabel('新集合名称').fill('学习端全权文本来源')
  await dialog.getByRole('button', { name: '创建并选用集合' }).click()
  await dialog.getByRole('button', { name: '粘贴文本', exact: true }).click()
  await dialog.getByLabel('粘贴资料名称').fill('偶数资料')
  await dialog.getByLabel('粘贴原文', { exact: true }).fill('# 偶数\n\n偶数可以被 2 整除，2 和 4 都是偶数。')
  await dialog.getByRole('button', { name: '加入粘贴文本' }).click()
  await dialog.getByRole('button', { name: '生成服务端预览' }).click()
  await expect(dialog.getByRole('heading', { name: '尚未正式导入' })).toBeVisible()
  expect((await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)).items).toHaveLength(0)
  await dialog.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(dialog.getByRole('heading', { name: '资料已保存' })).toBeVisible()
  await dialog.getByRole('button', { name: '关闭添加来源' }).click()
  expect((await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)).items).toHaveLength(1)

  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await studio.getByRole('button', { name: '＋ 添加笔记', exact: true }).click()
  await studio.getByLabel('笔记标题', { exact: true }).fill('偶数学习笔记')
  const note = '# 我的理解\n\n偶数可以被 2 整除。'
  await studio.getByRole('textbox', { name: '笔记正文', exact: true }).fill(note)
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/已保存第 1 版/).first()).toBeVisible()
  await page.reload()
  await studio.getByRole('button', { name: /偶数学习笔记.*已保存第 1 版/ }).click()
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(note)

  await page.goto('/app/memory')
  await page.getByRole('button', { name: '新建偏好候选', exact: true }).click()
  const preference = `我长期偏好先看例子再看定义 ${randomUUID()}`
  await page.getByLabel('具体内容', { exact: true }).fill(preference)
  await page.getByLabel('保存或纠正理由').fill('稳定的学习偏好，明确审阅后保存')
  await page.getByRole('button', { name: '创建待审阅候选' }).click()
  await page.getByRole('link', { name: '查看具体候选并审批' }).click()
  const review = page.getByRole('region', { name: '记忆候选详情' })
  await expect(review).toContainText(preference)
  await expect(review).toContainText('待明确批准')
  await expect(review.getByRole('button', { name: '批准此条记忆', exact: true })).toBeEnabled()
  await expect(review.getByRole('button', { name: '拒绝此候选', exact: true })).toBeEnabled()

  await page.goto('/app/settings')
  await expect(page.getByText(/写入和测试需要操作者使用 settings 配对档案/)).toBeVisible()
  const headers = { Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token }
  for (const [method, path] of [['PUT', '/v1/settings'], ['POST', '/v1/settings/probes'], ['POST', '/admin/api/pairing-codes']]) {
    const denied = await page.request.fetch(path, { method, headers, data: {} })
    expect(denied.status(), `${path}: ${await denied.text()}`).toBe(403)
    expect((await denied.json()).error.code).toBe('forbidden')
  }
  const erased = await page.request.post('/v1/privacy/erasures', {
    headers,
    data: { operation_id: randomUUID(), payload_schema_version: 1, expected_current_learner_generation: session.generation, reason_code: 'learner_request', explicit_confirmation: true },
  })
  expect(erased.status(), await erased.text()).toBe(403)
  expect((await erased.json()).error.code).toBe('forbidden')
  const current = await (await page.request.get('/v1/web/session')).json()
  expect(current.device.id).toBe(session.device.id)
  expect(current.generation).toBe(session.generation)
  expect(pairings).toBe(1)
  expect(errors).toEqual([])
})
