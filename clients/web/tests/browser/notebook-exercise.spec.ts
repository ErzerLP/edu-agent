import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, seedSources, webAPI } from './notebook-fixture'
import { captureNotebook } from './notebook-visual'

test('C3b 出题、帮助、丢响应核对、自动反馈、引用、收起恢复与进度', async ({ page }) => {
  test.setTimeout(120000)
  page.setDefaultTimeout(15000)
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '对话练习闭环',
    description: '',
    status: 'active',
    direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  const chat = page.getByRole('region', { name: '对话', exact: true })
  await chat.getByRole('button', { name: '出一道题', exact: true }).click()
  const card = chat.getByRole('article', { name: '对话练习' })
  await expect(card).toBeVisible()
  await expect(card.getByRole('button', { name: '提交答案', exact: true })).toBeDisabled()
  await captureNotebook(page, 'exercise', '对话')
  await card.getByRole('button', { name: '给我一个提示' }).click()
  await expect(card).toContainText('先找出题目中的关键概念')
  await expect(card.getByLabel('我得到的帮助').locator('option[value="none"]')).toHaveCount(0)
  await card.getByLabel('练习答案').fill('能被 2 整除的数是偶数，例如 4。')
  await card.getByLabel('我得到的帮助').selectOption('hint')
  let lost = false
  await page.route('**/v1/learning/content/*/answers', async (route) => {
    if (!lost) {
      lost = true
      await route.fetch()
      await route.abort('failed')
    } else await route.continue()
  })
  await card.getByRole('button', { name: '提交答案', exact: true }).click()
  // Polling may settle the committed operation before the user needs its receipt.
  const feedback = chat.getByRole('article', { name: '练习反馈' })
  await expect(feedback).toContainText('你找到了关键点')
  await expect(card).toContainText('已提交')
  await captureNotebook(page, 'feedback', '对话')
  await feedback.getByRole('button', { name: '查看依据 1' }).click()
  const citation = page.getByRole('dialog', { name: '反馈依据', exact: true })
  await expect(citation.locator('mark')).toContainText('偶数')
  await citation.getByRole('button', { name: '关闭来源', exact: true }).click()
  const conversation = new URL(page.url()).searchParams.get('conversation')!
  const turns = await webAPI(
    page,
    'GET',
    `/v1/learning/conversations/${conversation}`,
    undefined,
    space.id,
  )
  const first = await webAPI(
    page,
    'GET',
    `/v1/learning/exercises/${turns.items[0].exercise_id}`,
    undefined,
    space.id,
  )
  expect(first.feedback.evidence).toHaveLength(1)
  expect(first.feedback.attempt.help).toBe('hint')
  await feedback.getByRole('button', { name: '继续学习', exact: true }).click()
  await expect(feedback.getByRole('button', { name: '继续学习', exact: true })).toHaveCount(0)
  await expect(feedback).toContainText('你找到了关键点')
  await chat.getByRole('button', { name: '出一道题', exact: true }).click()
  await expect(card).toHaveCount(2)
  await card.last().getByRole('button', { name: '稍后再做', exact: true }).click()
  await expect(card.last().getByRole('button', { name: '继续这道题' })).toBeVisible()
  await page.reload()
  await expect(card.last().getByRole('button', { name: '继续这道题' })).toBeVisible()
  await card.last().getByRole('button', { name: '继续这道题' }).click()
  await expect(card.last().getByLabel('练习答案')).toHaveValue('')
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  await expect(card.last().getByLabel('练习答案')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await captureNotebook(page, 'exercise-mobile', '对话')
  const history = await webAPI(
    page,
    'GET',
    `/v1/learning/conversations/${conversation}`,
    undefined,
    space.id,
  )
  const second = await webAPI(
    page,
    'GET',
    `/v1/learning/exercises/${history.items[1].exercise_id}`,
    undefined,
    space.id,
  )
  expect(second.attempt_id).toBeUndefined()
  await page.goto(`/app/progress?space=${space.id}&status=all`)
  await expect(page.getByRole('heading', { name: /进度/ }).first()).toBeVisible()
  await expect(page.getByText('理解偶数与整除', { exact: false }).first()).toBeVisible()
})

test('C3b 零来源只给入口，待复核不显示通过', async ({ page }) => {
  test.setTimeout(90000)
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '来源与复核',
    description: '',
    status: 'active',
    direction: '理解偶数',
  })
  await page.goto(`/app/spaces/${space.id}`)
  await page.getByRole('button', { name: '出一道题', exact: true }).click()
  await expect(page.getByRole('button', { name: '添加或选择来源', exact: true })).toBeVisible()
  await expect(page.getByRole('article', { name: '对话练习' })).toHaveCount(0)
  const conversations = await webAPI(
    page,
    'GET',
    '/v1/learning/conversations?mode=study',
    undefined,
    space.id,
  )
  expect(conversations.items).toHaveLength(0)
  await seedSources(page, space.id)
  await page.reload()
  await page.getByRole('button', { name: '出一道题', exact: true }).click()
  const card = page.getByRole('article', { name: '对话练习' })
  await card.getByLabel('练习答案').fill('待复核夹具：偶数能被 2 整除，例如 4。')
  await card.getByLabel('我得到的帮助').selectOption('none')
  await card.getByRole('button', { name: '提交答案', exact: true }).click()
  const feedback = page.getByRole('article', { name: '练习反馈' })
  await expect(feedback.getByRole('heading', { name: '待复核', exact: true })).toBeVisible()
  await expect(feedback).not.toContainText('你找到了关键点')
})
