import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { pairNotebook, createSpace, webAPI } from './notebook-fixture'

test('Studio 拒绝缺失 CSRF 的写入，读取撤权后清除尚未保存的草稿', async ({ page }) => {
  await pairNotebook(page)
  const space = await createSpace(page, 'Studio 权限隔离')
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await studio.getByRole('button', { name: '＋ 添加笔记' }).click()
  await studio.getByRole('textbox', { name: '笔记正文', exact: true }).fill('REVOKED_DRAFT')
  const denied = await page.request.post('/v1/learning/studio/artifacts', { headers: { Origin: 'http://127.0.0.1:32929', 'X-Learning-Space-ID': space.id }, data: { operation_id: randomUUID(), expected_version: 0, privacy_generation: 1, kind: 'note', goal_id: null, title: '伪造保存', markdown: 'no csrf' } })
  expect(denied.status()).toBe(403)
  await page.route('**/v1/learning/studio/artifacts', route => route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ error: { code: 'learning_content_forbidden' } }) }))
  await expect(studio.getByText(/已隐藏正文并清除本区笔记草稿/)).toBeVisible({ timeout: 10000 })
  await expect(studio.getByRole('textbox')).toHaveCount(0)
  await page.unroute('**/v1/learning/studio/artifacts')
  await studio.getByRole('button', { name: '＋ 添加笔记' }).click({ timeout: 10000 })
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('')
})

test('笔记保存重开、冲突保留草稿，已保存版本转来源后不随笔记修改', async ({ page }) => {
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'Studio 无目标学习区')
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await expect(studio.getByText('尚未交付', { exact: true })).toHaveCount(3)
  await studio.getByRole('button', { name: '＋ 添加笔记', exact: true }).click()
  await studio.getByLabel('笔记标题', { exact: true }).fill('矩阵秩笔记')
  await studio
    .getByRole('textbox', { name: '笔记正文', exact: true })
    .fill('# 秩的理解\n\nNOTE_SAVED_V1：向量排成矩阵后比较秩。')
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/已保存第 1 版/).first()).toBeVisible()
  await page.reload()
  await studio.getByRole('button', { name: /矩阵秩笔记.*已保存第 1 版/ }).click()
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(
    /NOTE_SAVED_V1/,
  )
  const list = await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)
  const id = list.items[0].artifact_id
  const saved = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/artifacts/${id}`,
    undefined,
    space.id,
  )
  await studio
    .getByRole('textbox', { name: '笔记正文', exact: true })
    .fill('LOCAL_DRAFT：我的修改必须保留。')
  await webAPI(
    page,
    'PUT',
    `/v1/learning/studio/artifacts/${id}`,
    {
      operation_id: randomUUID(),
      expected_version: 1,
      privacy_generation: saved.privacy_generation,
      kind: 'note',
      goal_id: null,
      title: '矩阵秩笔记',
      markdown: 'REMOTE_V2：另一标签页已保存。',
    },
    space.id,
  )
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(
    studio.getByText('保存版本冲突，本地草稿已保留。请读取最新版本并比较后再保存。'),
  ).toBeVisible()
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(
    'LOCAL_DRAFT：我的修改必须保留。',
  )
  await studio.getByRole('button', { name: '读取最新版本并保留草稿' }).click()
  await expect(studio.getByText('REMOTE_V2：另一标签页已保存。', { exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '以已核对版本保存草稿' }).click()
  await expect(studio.getByText(/已保存第 3 版/).first()).toBeVisible()
  await studio.getByLabel('转为来源的已保存版本').selectOption('1')
  await studio.getByRole('button', { name: '转为来源', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '笔记转为来源', exact: true })
  await expect(dialog).toContainText('第 1 版')
  await dialog.getByLabel('新集合名称').fill('笔记快照来源')
  await dialog.getByRole('button', { name: '创建并选用集合' }).click()
  await dialog.getByRole('button', { name: '生成服务端预览' }).click()
  await expect(dialog.getByText('尚未正式导入', { exact: true })).toBeVisible()
  expect((await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)).items).toEqual(
    [],
  )
  await dialog.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(dialog.getByText('资料已导入，尚未自动用于目标')).toBeVisible()
  await dialog.getByRole('button', { name: '关闭笔记导入' }).click()
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources).toContainText('笔记第 1 版快照')
  const source = (await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)).items[0]
  expect(source.metadata.derived_from).toEqual({ artifact_id: id, version: 1 })
  await sources.getByRole('button', { name: `查看来源：${source.path}`, exact: true }).click()
  const viewer = page.getByRole('dialog', { name: '来源查看器', exact: true })
  await viewer.getByText(/原文片段 1/).click()
  await expect(viewer).toContainText('NOTE_SAVED_V1')
  await expect(viewer).not.toContainText('LOCAL_DRAFT')
  await viewer.getByRole('button', { name: /关闭/ }).click()
  await studio
    .getByRole('textbox', { name: '笔记正文', exact: true })
    .fill('NOTE_V4：此修改不会同步到来源。')
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/已保存第 4 版/).first()).toBeVisible()
  await sources.getByRole('button', { name: `查看来源：${source.path}`, exact: true }).click()
  await viewer.getByText(/原文片段 1/).click()
  await expect(viewer).toContainText('NOTE_SAVED_V1')
  await expect(viewer).not.toContainText('NOTE_V4')
})

test('笔记草稿在标签页和学习区之间隔离，保存丢响应后核对原操作', async ({ page }) => {
  await pairNotebook(page)
  const space = await createSpace(page, 'Studio 草稿隔离')
  const other = await createSpace(page, 'Studio 另一区')
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await studio.getByRole('button', { name: '＋ 添加笔记' }).click()
  await studio.getByLabel('笔记标题').fill('独立草稿')
  await studio.getByRole('textbox', { name: '笔记正文', exact: true }).fill('TAB_ONLY_DRAFT')
  const second = await page.context().newPage()
  await second.goto(`/app/spaces/${space.id}?panel=studio`)
  await second.getByRole('button', { name: '＋ 添加笔记' }).click()
  await expect(second.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('')
  await second.close()
  await page.getByRole('link', { name: '全部学习区', exact: true }).click()
  await page.getByRole('link', { name: 'Studio 另一区', exact: true }).click()
  await page.getByRole('button', { name: '＋ 添加笔记' }).click()
  await expect(page.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('')
  await page.getByRole('link', { name: '全部学习区', exact: true }).click()
  await page.getByRole('link', { name: 'Studio 草稿隔离', exact: true }).click()
  await page.getByRole('button', { name: '＋ 添加笔记' }).click()
  await expect(page.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(
    'TAB_ONLY_DRAFT',
  )
  await page.route('**/v1/learning/studio/artifacts', async (route) => {
    if (route.request().method() === 'POST') {
      await route.fetch()
      await route.abort('failed')
    } else await route.continue()
  })
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/保存结果未知/)).toBeVisible()
  await studio.getByRole('button', { name: '核对笔记保存结果' }).click()
  await expect(studio.getByText(/已保存第 1 版/).first()).toBeVisible()
  expect(
    (await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)).items,
  ).toHaveLength(1)
  expect(
    (await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, other.id)).items,
  ).toHaveLength(0)
})
