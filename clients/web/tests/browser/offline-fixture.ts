import { expect, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'

export const offlineOrigin = 'http://127.0.0.1:32929'
export const defaultSpace = '00000000-0000-4000-8000-000000000001'
export const offlinePassword = '离线验收使用的独立长口令-123456'
export const serverCommand = (args: string[]) =>
  execFileSync('../../server/edu-agentd', args, {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env: {
      PATH: process.env.PATH,
      DATABASE_URL: process.env.TEST_DATABASE_URL,
      MIGRATE_ON_START: 'true',
    },
  }).trim()

export async function offlineFixture(request: APIRequestContext) {
  const paired = await request.post(`${offlineOrigin}/v1/pairings/exchange`, {
    data: {
      code: serverCommand(['pairing-code', 'create']),
      display_name: '离线浏览器验收资料准备',
    },
  })
  expect(paired.ok(), await paired.text()).toBe(true)
  const identity = await paired.json()
  const headers = { Authorization: `Bearer ${identity.token}`, 'X-Learning-Space-ID': defaultSpace }
  const call = async (method: string, path: string, data?: unknown) => {
    const response = await request.fetch(`${offlineOrigin}${path}`, { method, headers, data })
    expect(response.ok(), `${path}: ${await response.text()}`).toBe(true)
    return response.json()
  }
  const head = await request.get(`${offlineOrigin}/v1/knowledge/revisions/head`, { headers })
  const parent = head.ok() ? (await head.json()).revision.revision_id : null
  const retained = parent
    ? (await call('GET', `/v1/knowledge/revisions/${parent}/export`)).documents
    : []
  const documents = [
    { path: 'offline-even.md', markdown: '# 偶数\n\n偶数可以被 2 整除。2 是偶数，3 是奇数。\n' },
    {
      path: 'offline-check.md',
      markdown: '# 偶数核对\n\n偶数的答案要核对原资料。离线答案须保存后再同步。\n',
    },
  ]
  await call('POST', '/v1/knowledge/imports', {
    operation_id: randomUUID(),
    expected_parent_revision_id: parent,
    source: '离线浏览器验收',
    documents: [
      ...retained.map((d: { path: string; markdown: string }) => ({
        path: d.path,
        markdown: d.markdown,
      })),
      ...documents.filter((d) => !retained.some((old: { path: string }) => old.path === d.path)),
    ],
  })
  const revision = (await call('GET', '/v1/knowledge/revisions/head')).revision.revision_id
  const goal = (
    await call('POST', '/v1/learning/goals', {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'goal',
      aggregate_id: randomUUID(),
      expected_version: 0,
      text: '理解偶数并核对资料',
      source: '离线浏览器验收',
    })
  ).result
  const id = randomUUID()
  await call('POST', '/v1/tutoring/sessions', {
    operation_id: randomUUID(),
    payload_schema_version: 1,
    aggregate_type: 'session',
    aggregate_id: id,
    expected_version: 0,
    goal_revision_id: goal.goal_revision_id,
  })
  const get = () => call('GET', `/v1/tutoring/sessions/${id}`)
  const action = async (action: string, extra = {}) =>
    call('POST', `/v1/tutoring/sessions/${id}/actions`, {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'session',
      aggregate_id: id,
      expected_version: (await get()).session.aggregate_version,
      action,
      ...extra,
    })
  await action('start_diagnostic')
  for (const kind of ['route', 'activity']) {
    const view = await get()
    const retrieval = await call('POST', '/v1/knowledge/retrievals', {
      query: '偶数',
      knowledge_revision_id: revision,
      query_context_schema_version: 'query-context-v1',
      context: { session_id: id },
    })
    const hits = retrieval.hits.map((hit: Record<string, unknown>) => ({
      knowledge_revision_id: revision,
      node_id: hit.node_id,
      node_revision_id: hit.node_revision_id,
      document_revision_id: hit.document_revision_id,
      range: hit.section_range,
      slice: hit.canonical_slice,
      slice_sha256: hit.slice_sha256,
    }))
    const proposal = await call('POST', '/v1/tutoring/proposals', {
      request_id: randomUUID(),
      proposal_type: kind,
      aggregate_type: 'session',
      aggregate_id: id,
      aggregate_version: view.session.aggregate_version,
      goal_revision_id: goal.goal_revision_id,
      ...view.session.focus,
      tutoring_state: view.session.state,
      knowledge_revision_id: revision,
      node_revision_ids: [
        ...new Set(hits.map((hit: { node_revision_id: string }) => hit.node_revision_id)),
      ],
      input: {
        schema_version: 'go-cli-context-v1',
        work_item: view.work_item,
        retrieval: { knowledge_revision_id: revision, hits },
      },
    })
    await action(kind === 'route' ? 'apply_route' : 'issue_activity', {
      proposal_id: proposal.proposal_id,
    })
  }
  await action('present_activity')
  const view = await get()
  return {
    id,
    goal,
    view,
    get,
    call,
    identity,
    path: `/app/offline?${new URLSearchParams({ space: defaultSpace, goal: goal.goal_id, session: id, version: String(view.session.aggregate_version) })}`,
  }
}
export async function pairOffline(page: Page) {
  await page.goto(`${offlineOrigin}/app/`)
  await page.getByLabel('配对码', { exact: true }).fill(serverCommand(['pairing-code', 'create']))
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
}
export async function createVault(page: Page, path: string) {
  await page.goto(`${offlineOrigin}${path}`)
  await page.getByLabel('解锁口令', { exact: true }).fill(offlinePassword)
  await page.getByLabel('再次输入口令').fill(offlinePassword)
  await page.getByRole('checkbox').check()
  await expect(page.getByRole('button', { name: '授权并创建加密离线库' })).toBeEnabled()
  await page.getByRole('button', { name: '授权并创建加密离线库' }).click()
  await expect(page.getByRole('heading', { name: '离线包列表' })).toBeVisible()
}
export async function unlockVault(page: Page) {
  await page.getByLabel('解锁口令', { exact: true }).fill(offlinePassword)
  await page.getByRole('button', { name: '解锁', exact: true }).click()
  await expect(page.getByRole('heading', { name: '离线包列表' })).toBeVisible()
}

export async function reconnectOffline(context: BrowserContext) {
  const pages = context.pages()
  // Playwright 先恢复页面，再恢复 Service Worker；待两者完成后才交付重连事件，
  // 避免页面已 online 而身份查询仍被模拟网络以 ERR_INTERNET_DISCONNECTED 拒绝。
  await Promise.all(
    pages.map((page) =>
      page.evaluate(() => {
        window.addEventListener('online', (event) => event.stopImmediatePropagation(), {
          capture: true,
          once: true,
        })
      }),
    ),
  )
  await context.setOffline(false)
  await Promise.all(
    pages.map((page) => page.evaluate(() => window.dispatchEvent(new Event('online')))),
  )
}
