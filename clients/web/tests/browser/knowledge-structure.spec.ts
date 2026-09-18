import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'

const space = '00000000-0000-4000-8000-000000000001'
async function api(page: Page, path: string, body?: unknown) {
  const session = await (await page.request.get('/v1/web/session')).json()
  const response = await page.request.fetch(path, { method: body ? 'POST' : 'GET', data: body, headers: { Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token, 'X-Learning-Space-ID': space } })
  expect(response.ok(), await response.text()).toBeTruthy()
  return response.json()
}
test('真实知识列表、局部图、审阅、未知结果恢复和键盘替代', async ({ page }) => {
  test.setTimeout(120_000)
  const errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  const code = execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', 'references'], { encoding: 'utf8', env: { PATH: process.env.PATH, DATABASE_URL: process.env.TEST_DATABASE_URL, MIGRATE_ON_START: 'true' }, stdio: ['ignore', 'pipe', 'pipe'] }).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
  const goal = randomUUID()
  await api(page, '/v1/learning/goals', { operation_id: randomUUID(), payload_schema_version: 1, aggregate_type: 'goal', aggregate_id: goal, expected_version: 0, text: '知识结构浏览器验收', source: '浏览器验收', details: { name: '知识结构浏览器验收' } })
  await page.goto(`/app/spaces/${space}/knowledge?goal=${goal}`)
  const panel = page.getByRole('region', { name: '动态知识结构', exact: true })
  await panel.getByRole('button', { name: '新增独立概念' }).click()
  const title = `独立概念 ${randomUUID().slice(0, 8)}`
  await panel.getByLabel('概念名称', { exact: true }).fill(title)
  await panel.getByLabel('含义、纠正说明与适用范围').fill('同名也保留独立身份')
  await panel.getByLabel('维护理由与语义依据').fill('明确的可审阅概念提案')
  let lost = false
  await page.route('**/v1/knowledge/structure/proposals', async route => {
    if (route.request().method() === 'POST' && !lost) { lost = true; await route.fetch(); await route.abort('failed'); return }
    await route.continue()
  })
  await panel.getByRole('button', { name: '提交维护提案' }).click()
  await expect(panel.getByText('提案提交结果未知，已保留原操作。请核对后再继续。')).toBeVisible()
  await panel.getByRole('button', { name: '核对并重试原提案' }).click()
  const review = panel.getByRole('article').filter({ has: page.getByRole('heading', { name: '明确的可审阅概念提案', exact: true }) })
  await review.getByLabel('审阅 / 补偿理由').fill('核对通过，来源状态和学习状态独立')
  await review.getByRole('button', { name: '批准知识维护', exact: true }).click()
  await page.getByRole('button', { name: '确认批准知识维护', exact: true }).click()
  await expect(panel.getByRole('button', { name: title, exact: true })).toBeVisible()
  await expect(panel.getByText('来源：候选；学习：未接触', { exact: true })).toBeVisible()
  await panel.getByRole('button', { name: '显示局部关系图' }).click()
  await expect(panel.getByRole('img', { name: '当前页概念关系，无掌握颜色' })).toBeVisible()
  await panel.getByRole('button', { name: `图节点：${title}`, exact: true }).focus()
  await page.keyboard.press('Enter')
  await expect(panel.getByRole('article', { name: '概念详情' })).toBeVisible()
  const list = await api(page, `/v1/knowledge/structure?goal_id=${goal}`)
  expect(list.items.filter((n: { name: string }) => n.name === title)).toHaveLength(1)
  expect((await api(page, '/v1/knowledge/structure/proposals')).items.filter((p: { reason: string }) => p.reason === '明确的可审阅概念提案')).toHaveLength(1)
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy()
  const audit = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()
  expect(audit.violations.map(v => v.id)).toEqual([])
  expect(errors).toEqual([])
})
