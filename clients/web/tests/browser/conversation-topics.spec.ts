import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairingCode, webAPI } from './notebook-fixture'

test('话题列表可跳转和上下处导航，键盘与移动布局保留输入草稿', async ({ page }) => {
  await configureNotebookModel(page)
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(pairingCode('learner'))
  const paired = page.waitForResponse(response => response.url().endsWith('/v1/web/pairings') && response.request().method() === 'POST')
  await page.getByRole('button', { name: '配对并进入' }).click()
  const pairing = await paired
  expect(pairing.ok(), await pairing.text()).toBe(true)
  await expect(page.getByRole('heading', { name: '你的学习区', exact: true, level: 1 })).toBeVisible()
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(), expected_version: 0, name: '话题导航验收', description: '', status: 'active', direction: '理解网络概念',
  })
  const conversation = await webAPI(page, 'POST', '/v1/learning/conversations', {
    id: randomUUID(), mode: 'study', saved: true,
  }, space.id)
  let version = 1
  for (const prompt of ['TCP 的含义', '子网的含义', '再比较 TCP 和子网']) {
    const receipt = await webAPI(page, 'POST', `/v1/learning/conversations/${conversation.id}/turns`, {
      operation_id: randomUUID(), expected_version: version++, prompt, direction_version: 1, source_selection: { mode: 'none', items: [] },
    }, space.id)
    await expect.poll(async () => (await webAPI(page, 'GET', `/v1/learning/runs/${receipt.run_id}`, undefined, space.id)).status).toBe('succeeded')
  }
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto(`/app/spaces/${space.id}?conversation=${conversation.id}`)
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  const input = page.getByLabel('向导师提问', { exact: true })
  await input.fill('这段草稿不能丢失')
  const trigger = page.getByRole('button', { name: '查看话题', exact: true })
  await trigger.focus(); await page.keyboard.press('Enter')
  const panel = page.getByRole('region', { name: '对话话题', exact: true })
  await expect(panel).toContainText('尚未整理话题')
  await panel.getByRole('button', { name: '整理话题', exact: true }).click()
  const tcp = panel.getByRole('button', { name: /TCP.*处/ })
  await expect(tcp).toBeVisible()
  await expect(panel).toContainText('已整理 6 / 6 条消息')
  await tcp.focus(); await page.keyboard.press('Enter')
  const messages = page.locator('[data-message-id]')
  await expect(messages.first()).toBeFocused()
  await expect(input).toHaveValue('这段草稿不能丢失')
  const navigation = page.getByRole('navigation', { name: '话题位置' })
  await navigation.getByRole('button', { name: '下一处', exact: true }).click()
  await expect(messages.nth(1)).toBeFocused()
  await navigation.getByRole('button', { name: '上一处', exact: true }).click()
  await expect(messages.first()).toBeFocused()
  await navigation.getByRole('button', { name: '返回话题列表', exact: true }).click()
  await expect(panel).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(trigger).toBeFocused()
  await expect(input).toHaveValue('这段草稿不能丢失')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})
