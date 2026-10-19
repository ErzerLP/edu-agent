import { expect, test } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, seedSources, webAPI } from './notebook-fixture'

test('S-C 题目规划、修改、确认、逐题生成、发布和第 1 题反馈', async ({ page }) => {
  test.setTimeout(90_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: 'S-C 测验验收',
    description: '',
    status: 'active',
    direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  await studio.getByRole('button', { name: /^测验/ }).click()
  await expect(studio.getByRole('combobox', { name: '题数', exact: true })).toHaveValue('3')
  await studio.getByRole('combobox', { name: '难度（可选）', exact: true }).selectOption('medium')
  const created = page.waitForResponse(
    (r) =>
      new URL(r.url()).pathname === '/v1/learning/studio/jobs' && r.request().method() === 'POST',
  )
  await studio.getByRole('button', { name: '生成题目规划', exact: true }).click()
  const job = await (await created).json()
  const questions = studio.getByRole('list', { name: '题目规划', exact: true })
  await expect(questions.getByRole('listitem')).toHaveCount(3)
  const before = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}`,
    undefined,
    space.id,
  )
  await studio.getByLabel('修改题目规划', { exact: true }).fill('更难一点，保留题目顺序')
  await studio.getByRole('button', { name: '发送修改', exact: true }).click()
  await expect(questions.getByText('解释 · 较难', { exact: true })).toHaveCount(3)
  const edited = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/jobs/${job.id}`,
    undefined,
    space.id,
  )
  expect(edited.plan.sections.map((q: { id: string }) => q.id)).toEqual(
    before.plan.sections.map((q: { id: string }) => q.id),
  )
  await questions.getByRole('button', { name: '下移：偶数知识点1', exact: true }).click()
  await expect(questions.getByRole('listitem').first()).toContainText('偶数知识点2')
  await questions.getByRole('button', { name: '删除：偶数知识点3', exact: true }).click()
  await expect(questions.getByRole('listitem')).toHaveCount(2)
  await studio.getByRole('button', { name: '确认题目规划', exact: true }).click()
  const progress = studio.getByRole('region', { name: '生成进度', exact: true })
  await expect(progress.getByRole('status')).toHaveText('已完成 1/2 题', { timeout: 20_000 })
  await expect(studio.getByRole('button', { name: '开始作答', exact: true })).toHaveCount(0)
  await expect(progress.getByRole('status')).toHaveText('已完成 2/2 题', { timeout: 20_000 })
  await progress.getByRole('button', { name: '打开已保存产物', exact: true }).click()
  await expect(studio.getByRole('region', { name: '练习覆盖与顺序', exact: true })).toContainText(
    '偶数知识点2',
  )
  await expect(
    studio
      .getByRole('region', { name: '练习覆盖与顺序', exact: true })
      .getByRole('list')
      .getByRole('listitem'),
  ).toHaveCount(2)
  await studio.getByRole('button', { name: '开始作答', exact: true }).click()
  const first = studio.getByRole('region', { name: '第 1 题作答', exact: true })
  await first.getByLabel('练习答案').fill('偶数能被 2 整除，例如 4。')
  await first.getByLabel('我得到的帮助').selectOption('none')
  await first.getByRole('button', { name: '提交答案', exact: true }).click()
  await expect(first.getByRole('article', { name: '练习反馈' })).toContainText('你找到了关键点', {
    timeout: 20_000,
  })
})
