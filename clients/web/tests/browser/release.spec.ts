import { test, expect } from '@playwright/test'

test('发行页面直达、刷新、真实资产与错误路由边界', async ({ page, request }) => {
  const space = '00000000-0000-4000-8000-000000000001'
  const id = '00000000-0000-4000-8000-000000000009'
  for (const path of ['/app/', `/app/spaces/${space}`, `/app/spaces/${space}/manage`, `/app/spaces/${space}?panel=sources`, `/app/spaces/${space}?conversation=${id}&panel=classroom`, `/app/spaces/${space}/learn/${id}?panel=studio`, `/app/spaces/${space}/goals/${id}`, `/app/spaces/${space}/chat/${id}`, `/app/spaces/${space}/studio`, `/app/content/${id}?space=${space}&version=2`, '/app/progress', '/app/memory', '/app/settings/data', '/app/settings/devices']) {
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
  for (const path of ['/v1/missing', `/v1/learning/conversations/${id}`, `/v1/learning/exercises/${id}`, '/admin/missing', '/internal/missing', '/app/assets/missing.js', '/app/assets/missing.css', '/app/settings/missing']) {
    const response = await request.get(path, { headers: { Accept: 'text/html' } })
    expect(response.status(), path).toBeGreaterThanOrEqual(400)
    expect(response.headers()['content-type'] ?? '', path).not.toContain('text/html')
    expect(await response.text(), path).not.toContain('type="module"')
  }
})
