import { test, expect } from '@playwright/test'

test('发行页面直达、刷新、真实资产与错误路由边界', async ({ page, request }) => {
  for (const path of ['/app/', '/app/progress', '/app/memory', '/app/settings/data', '/app/settings/devices']) {
    const response = await page.goto(path)
    expect(response?.status(), path).toBe(200)
    expect(response?.headers()['cache-control']).toBe('no-store')
    await expect(page.getByLabel('配对码', { exact: true })).toBeVisible()
    await page.reload()
    await expect(page.getByLabel('配对码', { exact: true })).toBeVisible()
  }
  const asset = await page.locator('script[type="module"]').getAttribute('src')
  expect(asset).toMatch(/^\/app\/assets\/.+\.js$/)
  const javascript = await request.get(asset!)
  expect(javascript.status()).toBe(200)
  expect(javascript.headers()['content-type']).toContain('javascript')
  for (const path of ['/v1/missing', '/admin/missing', '/internal/missing', '/app/assets/missing.js', '/app/settings/missing']) {
    const response = await request.get(path)
    expect(response.status(), path).toBeGreaterThanOrEqual(400)
    expect(await response.text(), path).not.toContain('type="module"')
  }
})
