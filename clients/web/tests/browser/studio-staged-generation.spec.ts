import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, seedSources, webAPI } from './notebook-fixture'

test('Studio 确认后逐段生成、3/8 停止、刷新继续、完成并打开产物', async ({ page }) => {
  test.setTimeout(90_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await configureNotebookModel(page)
  await pairNotebook(page, 'references')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: 'S-B 分段验收',
    description: '',
    status: 'active',
    direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources.getByRole('article')).toHaveCount(2)
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  await studio.getByRole('button', { name: /^笔记 按资料/ }).click()
  await studio.getByLabel('侧重点（可选）', { exact: true }).fill('分段验收')
  const created = page.waitForResponse(
    (r) =>
      new URL(r.url()).pathname === '/v1/learning/studio/jobs' && r.request().method() === 'POST',
  )
  await studio.getByRole('button', { name: '生成大纲', exact: true }).click()
  const job = await (await created).json()
  await expect(studio.getByRole('list', { name: '大纲部分' }).getByRole('listitem')).toHaveCount(8)
  await studio.getByRole('button', { name: '确认大纲', exact: true }).click()
  const progress = studio.getByRole('region', { name: '生成进度', exact: true })
  await expect(progress.getByRole('status')).toHaveText('已完成 3/8', { timeout: 20_000 })
  await progress.getByRole('button', { name: '停止生成', exact: true }).click()
  await expect(studio.getByText('已停止', { exact: true })).toBeVisible()
  expect(
    (await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)).items,
  ).toEqual([])
  const before = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}/units`,
    undefined,
    space.id,
  )
  const committed = before.items.filter((u: any) => u.status === 'succeeded')
  expect(committed).toHaveLength(3)
  await page.reload()
  await studio
    .getByRole('region', { name: '生成任务', exact: true })
    .getByRole('button', { name: /偶数学习大纲.*已停止/ })
    .click()
  await expect(progress.getByRole('status')).toHaveText('已完成 3/8')
  await progress.getByRole('button', { name: '继续生成', exact: true }).click()
  await expect(progress.getByRole('status')).toHaveText('已完成 8/8', { timeout: 30_000 })
  await expect(progress.getByRole('button', { name: '打开已保存产物', exact: true })).toBeVisible()
  const finished = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}`,
    undefined,
    space.id,
  )
  expect(finished.status).toBe('completed')
  const after = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}/units`,
    undefined,
    space.id,
  )
  expect(after.items.slice(0, 3)).toEqual(committed)
  const legacy = await page.request.get(
    `/v1/learning/studio/artifacts/${finished.result.artifact_id}`,
    { headers: { 'X-Learning-Space-ID': space.id } },
  )
  expect(legacy.status()).toBe(422)
  expect(legacy.headers()['x-studio-supported-body-schemas']).toBe('1,2')
  await progress.getByRole('button', { name: '打开已保存产物', exact: true }).click()
  await expect(studio.getByRole('heading', { name: '偶数学习大纲', exact: true })).toBeVisible()
  const saved = studio.getByRole('region', { name: '已保存段落', exact: true })
  await expect(
    saved.getByRole('heading', { name: '1. 偶数学习第1部分', exact: true }),
  ).toBeVisible()
  await saved.getByRole('button', { name: '编辑本段', exact: true }).first().click()
  await saved.getByLabel('本段笔记').fill('我的整除复习笔记。')
  await saved.getByRole('button', { name: '保存本段为新版本', exact: true }).click()
  await expect(saved.getByText('我的整除复习笔记。', { exact: true })).toBeVisible()
  await expect(
    saved.getByText('用户笔记 · 手工修改内容未经模型引用核验', { exact: true }),
  ).toBeVisible()
})
