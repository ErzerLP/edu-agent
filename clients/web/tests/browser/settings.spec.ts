import { test, expect } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { readFile } from 'node:fs/promises'

test('设置真实保存恢复、Key 不回显、显式探测与权限隔离', async ({ page, context }) => {
  const pair = async (profile: string) => {
    const code = execFileSync(
      '../../server/edu-agentd',
      ['pairing-code', 'create', '--profile', profile],
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
    await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
    await page.goto('/app/settings')
    await expect(page.getByRole('heading', { name: '模型与搜索', exact: true })).toBeVisible()
  }
  await pair('settings')
  const mentor = page.getByRole('region', { name: 'Web 导师模型', exact: true })
  await expect(mentor.getByRole('button', { name: '配置或替换凭据' })).toBeEnabled()
  const card = page.getByRole('region', { name: '教学模型', exact: true })
  await card.getByRole('button', { name: '配置或替换凭据' }).click()
  await card.getByLabel('端点', { exact: true }).fill('http://127.0.0.1:1/v1')
  await card.getByLabel('模型名称').fill('browser-test-model')
  const secret = 'browser-settings-key-sentinel-29473'
  await card.getByLabel('新 Key（留空保留同端点旧值）').fill(secret)
  await card.getByLabel('启用教学模型').check()
  await card.getByRole('button', { name: '保存教学模型配置' }).click()
  await expect(page.getByText('配置已保存。', { exact: true })).toBeVisible()
  await expect(card.getByText('凭据：已安全保存，不能查看旧值')).toBeVisible()
  await expect(card.getByText(/尚未测试连接/)).toBeVisible()
  expect(await page.content()).not.toContain(secret)
  expect(
    await page.evaluate(() =>
      JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }),
    ),
  ).not.toContain(secret)
  expect(await (await page.request.get('/v1/settings')).text()).not.toContain(secret)
  await page.reload()
  await expect(mentor.getByRole('button', { name: '配置或替换凭据' })).toBeEnabled()
  await expect(card.getByText('模型：browser-test-model')).toBeVisible()
  await card.getByRole('button', { name: '测试连接', exact: true }).click()
  await expect(page.getByRole('alertdialog')).toContainText('可能消耗额度或产生费用')
  await page.getByRole('button', { name: '确认测试连接', exact: true }).click()
  await expect(card.getByText(/端点不可达或被网络策略拒绝/)).toBeVisible()
  await expect(card.getByText(/上次测试/)).toBeVisible()
  await page.getByLabel('并发上限', { exact: true }).fill('3')
  await page.getByRole('button', { name: '保存预算与外发策略' }).click()
  await expect(page.getByText('配置已保存。', { exact: true })).toBeVisible()
  await page.reload()
  await expect(page.getByLabel('并发上限', { exact: true })).toHaveValue('3')
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
        const response = await page.request.get('/livez')
        return response.status()
      } catch {
        return 0
      }
    })
    .toBe(200)
  await page.reload()
  await expect(card.getByText('模型：browser-test-model')).toBeVisible()
  for (const width of [390, 768, 1280]) {
    await page.setViewportSize({ width, height: 1000 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }
  await context.clearCookies()
  await pair('user')
  await expect(mentor.getByRole('button', { name: '配置或替换凭据' })).toBeDisabled()
  await expect(card.getByRole('button', { name: '配置或替换凭据' })).toBeDisabled()
  await expect(card.getByRole('button', { name: '测试连接', exact: true })).toBeDisabled()
  await expect(page.getByRole('button', { name: '保存预算与外发策略' })).toBeDisabled()
})
