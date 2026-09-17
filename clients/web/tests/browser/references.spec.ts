import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'

const space = '00000000-0000-4000-8000-000000000001'
async function pairReferences(page: Page) {
  const code = execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', 'references'], { encoding: 'utf8', env: { PATH: process.env.PATH, DATABASE_URL: process.env.TEST_DATABASE_URL, MIGRATE_ON_START: 'true' }, stdio: ['ignore', 'pipe', 'pipe'] }).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
}
async function api(page: Page, path: string, body?: unknown) {
  const session = await (await page.request.get('/v1/web/session')).json()
  const response = await page.request.fetch(path, { method: body ? 'POST' : 'GET', data: body, headers: { Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token, 'X-Learning-Space-ID': space } })
  expect(response.ok(), await response.text()).toBeTruthy()
  return response.json()
}

test('真实文件单批、结果未知核对、章节采用及移动端键盘', async ({ page }) => {
  test.setTimeout(120_000)
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  await pairReferences(page)
  const goal = randomUUID()
  await api(page, '/v1/learning/goals', { operation_id: randomUUID(), payload_schema_version: 1, aggregate_type: 'goal', aggregate_id: goal, expected_version: 0, text: '浏览器参考闭环', source: '浏览器验收', details: { name: '浏览器参考闭环' } })
  await page.goto(`/app/spaces/${space}/knowledge?goal=${goal}`)
  const name = `参考集合 ${randomUUID().slice(0, 8)}`
  await page.getByLabel('集合名称', { exact: true }).fill(name)
  await page.getByRole('button', { name: '创建私有集合' }).click()
  await expect(page.getByRole('heading', { name: `单批导入到「${name}」` })).toBeVisible()
  await page.getByLabel('选择 Markdown / UTF-8 文件（可多选）').setInputFiles([
    { name: '笔记.md', mimeType: 'text/markdown', buffer: Buffer.from('# 原始资料\n\n## 第一章\n\n不在限制范围内的正文\n\n## 第二章\n\n获准的章节正文\n' + '长内容可滚动检索。'.repeat(200)) },
    { name: '文字.txt', mimeType: 'text/plain', buffer: Buffer.from('UTF-8 实际文件正文') },
    { name: '错误编码.txt', mimeType: 'text/plain', buffer: Buffer.from([0xff, 0xfe]) },
    { name: '不支持.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF 不应读取') },
  ])
  await expect(page.getByText('待导入 2 项 · 错误 1 · 不支持 1 · 被排除 0')).toBeVisible()
  await page.getByLabel('粘贴资料名称').fill('粘贴示例')
  await page.getByLabel('粘贴原文', { exact: true }).fill('粘贴的真实原文')
  await page.getByRole('button', { name: '加入粘贴文本' }).click()
  await page.getByRole('checkbox', { name: '文字.txt', exact: true }).uncheck()
  await page.getByRole('button', { name: '生成服务端预览' }).click()
  await expect(page.getByRole('heading', { name: '尚未正式导入' })).toBeVisible()
  const before = await api(page, '/v1/knowledge/collections')
  const collection = before.items.find((c: { name: string }) => c.name === name)
  expect(collection.head_revision_id).toBeNull()
  let operation = ''
  let dropped!: () => void
  const responseDropped = new Promise<void>(resolve => { dropped = resolve })
  await page.route('**/v1/knowledge/imports/confirm', async route => {
    operation = route.request().postDataJSON().request.operation_id
    await route.fetch()
    await route.abort('failed')
    dropped()
  })
  await page.getByRole('button', { name: '确认正式导入这 2 项资料' }).click()
  // 页面在发请求前就显示未知；必须等实际提交响应丢失后才能解除拦截。
  await responseDropped
  await expect(page.getByText('提交结果未知。先核对原操作，当前草稿与操作编号已保留。')).toBeVisible()
  await page.unroute('**/v1/knowledge/imports/confirm')
  await page.getByRole('button', { name: '核对原操作结果', exact: true }).click()
  await expect(page.getByRole('heading', { name: '资料已导入，尚未自动用于目标' })).toBeVisible()
  expect(operation).not.toBe('')
  await page.getByLabel('新选择的角色').selectOption('restrict')
  await page.getByRole('button', { name: '选择章节「第二章」', exact: true }).click()
  await page.getByRole('button', { name: '预览参考角色与范围变更' }).click()
  await expect(page.getByRole('heading', { name: '具体变更：仅此目标' })).toBeVisible()
  await expect(page.getByRole('button', { name: '确认此目标／会话的新限制范围' })).toBeDisabled()
  await page.getByRole('checkbox', { name: '我确认上面列出的旧限制范围与新限制范围' }).check()
  await page.getByRole('button', { name: '确认此目标／会话的新限制范围' }).click()
  await expect(page.getByText('已采用，供后续准备使用；当前题目保留原引用。')).toBeVisible()
  const state = await api(page, `/v1/learning/goals/${goal}/references`)
  const frozen = await api(page, `/v1/knowledge/scopes/${state.scope_snapshot_id}/export`)
  expect(frozen.documents).toHaveLength(1)
  expect(frozen.documents[0].markdown).toContain('获准的章节正文')
  expect(frozen.documents[0].markdown).not.toContain('不在限制范围内')
  const currentGoal = await api(page, `/v1/learning/goals/${goal}`)
  expect(currentGoal.revision).toBe(1)
  await page.getByRole('button', { name: '查看旧冻结范围与更新提示' }).click()
  await page.getByLabel('搜索文档、章节和正文').fill('获准的章节正文')
  await page.getByLabel('搜索文档、章节和正文').press('Tab')
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy()
  const audit = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()
  expect(audit.violations.map(v => v.id)).toEqual([])
  expect(errors).toEqual([])
})
