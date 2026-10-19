import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, seedSources, webAPI } from './notebook-fixture'

test('对话选择部分话题，规划笔记并确认生成完成', async ({ page }) => {
  test.setTimeout(90_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await configureNotebookModel(page)
  await pairNotebook(page, 'references')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: 'S-D 对话整理',
    description: '',
    status: 'active',
    direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources.getByRole('article')).toHaveCount(2)
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  await page.getByLabel('向导师提问', { exact: true }).fill('整理偶数的定义与例子')
  await page.getByRole('button', { name: '发送提问', exact: true }).click()
  const organize = page.getByRole('button', { name: '整理本次对话', exact: true })
  await expect(organize).toBeEnabled()
  // The same entry guides a source-less user back to their materials.
  await sources.getByRole('checkbox', { name: /even.md/ }).uncheck()
  await organize.click()
  const chooser = page.getByRole('region', { name: '整理本次对话', exact: true })
  await expect(chooser).toContainText('请先选择至少一份资料')
  await chooser.getByRole('button', { name: '选择资料', exact: true }).click()
  await sources.getByRole('checkbox', { name: /even.md/ }).check()
  await organize.click()
  await expect(chooser.getByRole('checkbox', { name: '偶数的定义', exact: true })).toBeChecked()
  await chooser.getByRole('checkbox', { name: '偶数的例子', exact: true }).uncheck()
  const created = page.waitForResponse(
    (r) =>
      new URL(r.url()).pathname === '/v1/learning/studio/jobs' && r.request().method() === 'POST',
  )
  await chooser.getByRole('button', { name: '生成笔记大纲', exact: true }).click()
  const response = await created
  expect(response.ok(), await response.text()).toBe(true)
  const request = response.request().postDataJSON()
  expect(request.conversation_scope.topic_ids).toHaveLength(1)
  expect(request.conversation_scope.index_revision).toBeGreaterThan(0)
  expect(request.conversation_scope.through_message).toBeTruthy()
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const parts = studio.getByRole('list', { name: '大纲部分' })
  await expect(parts.getByRole('listitem')).toHaveCount(1)
  await expect(parts).toContainText('偶数的定义')
  await expect(parts).not.toContainText('偶数的例子')
  await studio.getByRole('button', { name: '确认大纲', exact: true }).click()
  await expect(studio.getByRole('button', { name: '打开已保存产物', exact: true })).toBeVisible({
    timeout: 30_000,
  })
  const job = await response.json()
  const completed = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}`,
    undefined,
    space.id,
  )
  expect(completed.status).toBe('completed')
  expect(completed.plan.sections[0].topic_ids).toEqual(request.conversation_scope.topic_ids)
})
