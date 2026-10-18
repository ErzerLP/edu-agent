import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, seedSources, webAPI } from './notebook-fixture'

test('Studio 大纲生成、多轮修改、拖动排序、确认与刷新恢复', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await configureNotebookModel(page)
  await pairNotebook(page, 'references')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: 'Studio 大纲验收',
    description: '',
    status: 'active',
    direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  const outgoing: any[] = []
  page.on('request', (request) => {
    if (
      new URL(request.url()).pathname === '/v1/learning/studio/jobs' &&
      request.method() === 'POST'
    )
      outgoing.push(request.postDataJSON())
  })
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources.getByRole('article')).toHaveCount(2)
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  await studio.getByRole('button', { name: /^学习指南/ }).click()
  await studio.getByLabel('侧重点（可选）', { exact: true }).fill('复习整除')
  const createdResponse = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === '/v1/learning/studio/jobs' &&
      response.request().method() === 'POST',
  )
  await studio.getByRole('button', { name: '生成大纲', exact: true }).click()
  const created = await createdResponse
  expect(created.status()).toBe(202)
  const job = await created.json()
  expect(outgoing).toHaveLength(1)
  expect(outgoing[0]).toMatchObject({ kind: 'guide', direction_version: 1, focus: '复习整除' })
  expect(outgoing[0].source_selection.items).toHaveLength(1)
  await expect(studio.getByRole('heading', { name: '偶数学习大纲', exact: true })).toBeVisible()
  for (const text of ['先从整除讲起', '定义部分再简洁一点']) {
    await studio.getByLabel('修改大纲').fill(text)
    await studio.getByRole('button', { name: '发送修改', exact: true }).click()
    await expect(studio.getByLabel('修改大纲')).toHaveValue('')
    await expect(studio.getByText(text, { exact: true })).toBeVisible()
  }
  const outline = studio.getByRole('list', { name: '大纲部分' })
  const handle = outline.getByRole('button', { name: '拖动排序：先理解整除', exact: true })
  const target = outline.getByRole('listitem').nth(1)
  await outline.scrollIntoViewIfNeeded()
  const from = await handle.boundingBox(),
    to = await target.boundingBox()
  expect(from).not.toBeNull()
  expect(to).not.toBeNull()
  await page.mouse.move(from!.x + from!.width / 2, from!.y + from!.height / 2)
  await page.mouse.down()
  await page.mouse.move(to!.x + to!.width / 2, to!.y + to!.height / 2, { steps: 8 })
  await page.mouse.up()
  await expect(outline.getByRole('listitem').first()).toContainText('偶数的例子')
  await studio.getByRole('button', { name: '确认大纲', exact: true }).click()
  await expect(studio.getByText('待生成', { exact: true })).toBeVisible()
  await expect(studio.getByRole('button', { name: '打开已保存产物' })).toHaveCount(0)
  const confirmed = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}`,
    undefined,
    space.id,
  )
  expect(confirmed.status).toBe('queued')
  expect(confirmed.confirmed_plan_revision).toBe(confirmed.active_plan_revision)
  expect(confirmed.plan.sections.map((section: any) => section.id)).toEqual([
    'examples',
    'definition',
  ])
  expect(confirmed.messages.map((message: any) => message.text)).toEqual([
    '先从整除讲起',
    '定义部分再简洁一点',
  ])
  const artifacts = await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)
  expect(artifacts.items).toEqual([])
  await page.reload()
  await studio
    .getByRole('region', { name: '生成任务', exact: true })
    .getByRole('button', { name: /偶数学习大纲.*待生成/ })
    .click()
  await expect(studio.getByText('待生成', { exact: true })).toBeVisible()
  await expect(outline.getByRole('listitem').first()).toContainText('偶数的例子')
  await expect(studio.getByText('定义部分再简洁一点', { exact: true })).toBeVisible()
})
