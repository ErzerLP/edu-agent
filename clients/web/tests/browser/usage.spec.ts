import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, navigateFromMenu, webAPI } from './notebook-fixture'

test('一次导师对话的真实用量可查看区累计、全局累计与明细', async ({ page }) => {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '用量验收',
    description: '',
    status: 'active',
    direction: '理解偶数',
  })
  await page.goto(`/app/spaces/${space.id}`)
  await page.getByLabel('向导师提问', { exact: true }).fill('解释一下偶数')
  await page.getByRole('button', { name: '发送提问', exact: true }).click()
  await expect(
    page.getByText('围绕理解偶数，我们可以先理解核心概念。', { exact: true }),
  ).toBeVisible()
  await navigateFromMenu(page, '用量')
  await expect(page.getByRole('heading', { name: '用量', exact: true })).toBeVisible()
  await expect(page.getByText(/当前仅统计导师对话/)).toBeVisible()
  await expect(page.getByText(/统计起始时间/)).toBeVisible()
  await page.getByLabel('学习区范围').selectOption(space.id)
  const summary = page.getByRole('region', { name: '所选范围用量' })
  await expect(summary.getByText('200 Token', { exact: true })).toBeVisible()
  await expect(summary.getByText(/未知：1 次调用/).first()).toBeVisible()
  await page.getByLabel('时间分组').selectOption('week')
  await page.getByLabel('模型筛选').selectOption('notebook-study-fixture')
  await page.getByRole('button', { name: '查看调用明细' }).first().click()
  const detail = page.getByRole('region', { name: '调用明细' })
  await expect(detail.getByText('notebook-study-fixture', { exact: true })).toBeVisible()
  await expect(detail.getByText('200', { exact: true })).toBeVisible()
  await expect(detail.getByText('未知', { exact: true })).toBeVisible()
  await page.getByLabel('学习区范围').selectOption('')
  await expect(page.getByRole('heading', { name: '全局累计' })).toBeVisible()
  await expect(summary.getByText('200 Token', { exact: true })).toBeVisible()
})
