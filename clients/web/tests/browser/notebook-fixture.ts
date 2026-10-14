import { expect, request, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'

export const defaultSpace = '00000000-0000-4000-8000-000000000001'

export function pairingCode(profile = 'user') {
  return execFileSync('../../server/edu-agentd', ['pairing-code', 'create', '--profile', profile], {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env: {
      PATH: process.env.PATH,
      DATABASE_URL: process.env.TEST_DATABASE_URL,
      MIGRATE_ON_START: 'true',
    },
  }).trim()
}

export async function pairNotebook(page: Page, profile = 'user') {
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(pairingCode(profile))
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(
    page.getByRole('heading', { name: '你的学习区', exact: true, level: 1 }),
  ).toBeVisible()
}

export async function configureNotebookModel(page: Page) {
  await pairNotebook(page, 'settings')
  await page.goto('/app/settings')
  const model = page.getByRole('region', { name: 'Web 导师模型', exact: true })
  await model.getByRole('button', { name: '配置或替换凭据' }).click()
  await model.getByLabel('端点', { exact: true }).fill('http://127.0.0.1:32930/v1')
  await model.getByLabel('模型名称').fill('notebook-study-fixture')
  await model.getByLabel('鉴权方式').selectOption('none')
  await model.getByLabel('启用Web 导师模型').check()
  await model.getByRole('button', { name: '保存Web 导师模型配置' }).click()
  await expect(page.getByText('配置已保存。', { exact: true })).toBeVisible()
  const session = await (await page.request.get('/v1/web/session')).json()
  const logout = await page.request.post('/v1/web/logout', { headers: { Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token } })
  expect(logout.ok()).toBe(true)
}

export async function openNavigation(page: Page) {
  await page.getByRole('button', { name: '打开侧边菜单', exact: true }).click()
  await expect(page.getByRole('dialog', { name: '侧边菜单', exact: true })).toBeVisible()
}

export async function setNotebookTheme(page: Page, theme: 'light' | 'dark') {
  if (await page.evaluate(() => (document.documentElement.dataset.theme ?? 'light')) === theme) return
  await openNavigation(page)
  await page
    .getByRole('button', { name: theme === 'dark' ? '切换深色主题' : '切换浅色主题' })
    .click()
  await page.getByRole('button', { name: '关闭侧边菜单', exact: true }).click()
}

export async function navigateFromMenu(page: Page, name: string) {
  await openNavigation(page)
  await page
    .getByRole('dialog', { name: '侧边菜单', exact: true })
    .getByRole('link', { name, exact: true })
    .click()
}

export async function webAPI(
  page: Page,
  method: string,
  path: string,
  data?: unknown,
  spaceId?: string,
) {
  const session = await (await page.request.get('/v1/web/session')).json()
  const response = await page.request.fetch(path, {
    method,
    data,
    headers: {
      Origin: 'http://127.0.0.1:32929',
      'X-CSRF-Token': session.csrf_token,
      ...(spaceId ? { 'X-Learning-Space-ID': spaceId } : {}),
    },
  })
  expect(response.ok(), `${path}: ${await response.text()}`).toBe(true)
  return response.json()
}

export async function createSpace(page: Page, name: string) {
  return webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name,
    description: '',
    status: 'active',
  })
}

// 只准备来源，教学会话、正式答案与反馈仍由真实 UI 和服务端领域路径执行。
export async function seedSources(page: Page, spaceId: string, name = '偶数资料', markdown = '# 偶数\n\n偶数可以被 2 整除。2 是偶数，3 是奇数。\n') {
  const legacy = await request.newContext({ baseURL: 'http://127.0.0.1:32929' })
  try {
    const pair = await legacy.post('/v1/pairings/exchange', {
      data: { code: pairingCode(), display_name: 'Notebook 来源夹具' },
    })
    expect(pair.ok(), await pair.text()).toBe(true)
    const token = (await pair.json()).token
    const collectionId = randomUUID()
    const call = async (method: string, path: string, data?: unknown) => {
      const response = await legacy.fetch(path, {
        method,
        data,
        headers: {
          Authorization: `Bearer ${token}`,
          'X-Learning-Space-ID': spaceId,
          ...(['/v1/knowledge/imports', '/v1/knowledge/revisions/head'].includes(path)
            ? { 'X-Knowledge-Collection-ID': collectionId }
            : {}),
        },
      })
      expect(response.ok(), `${path}: ${await response.text()}`).toBe(true)
      return response.json()
    }
    await call('POST', '/v1/knowledge/collections', {
      id: collectionId,
      action: 'create',
      name,
      source: 'Notebook 浏览器验收',
    })
    await call('POST', '/v1/knowledge/imports', {
      operation_id: randomUUID(),
      expected_parent_revision_id: null,
      source: 'Notebook 浏览器验收',
      documents: [
        { path: 'even.md', markdown },
        {
          path: 'examples.md',
          markdown: '# 偶数练习\n\n用原资料核对偶数，完成活动后查看反馈并继续学习。\n',
        },
      ],
    })
    const head = await call('GET', '/v1/knowledge/revisions/head')
    const scope = await call('POST', '/v1/knowledge/scopes', {
      id: randomUUID(),
      entries: [{ collection_id: collectionId, revision_id: head.revision.revision_id }],
    })
    return { collectionId, scopeId: scope.id }
  } finally {
    await legacy.dispose()
  }
}
