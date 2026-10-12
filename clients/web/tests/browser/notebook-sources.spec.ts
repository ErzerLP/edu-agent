import { test, expect } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { pairNotebook, createSpace } from './notebook-fixture'

test('Notebook 三种添加方式保留真实来源、部分覆盖和原操作核对，查看器定位历史片段', async ({
  page,
}) => {
  test.setTimeout(120_000)
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'Notebook 来源验收')
  await page.goto(`/app/spaces/${space.id}?panel=sources`)
  const panel = page.getByRole('region', { name: '来源', exact: true })
  await panel.getByRole('button', { name: '＋ 添加来源', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '添加来源', exact: true })
  await dialog.getByRole('button', { name: '粘贴文本', exact: true }).click()
  await dialog.getByLabel('新集合名称').fill('来源集合')
  await dialog.getByRole('button', { name: '创建并选用集合' }).click()
  await dialog.getByLabel('粘贴资料名称').fill('粘贴来源')
  await dialog.getByLabel('粘贴原文', { exact: true }).fill('Notebook 文本片段：用户原文保持不变。')
  await dialog.getByRole('button', { name: '加入粘贴文本' }).click()
  await dialog.getByRole('button', { name: '生成服务端预览' }).click()
  await expect(dialog.getByRole('heading', { name: '尚未正式导入' })).toBeVisible()
  let dropped = false
  await page.route('**/v1/knowledge/imports/confirm', async (route) => {
    if (dropped) return route.continue()
    dropped = true
    const response = await route.fetch()
    expect(response.ok()).toBe(true)
    await route.abort('failed')
  })
  await dialog.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(dialog.getByText(/提交结果未知/)).toBeVisible()
  await dialog.getByRole('button', { name: '核对原操作结果' }).click()
  await expect(dialog.getByRole('heading', { name: '资料已导入，尚未自动用于目标' })).toBeVisible()
  await page.unroute('**/v1/knowledge/imports/confirm')
  await dialog.getByRole('button', { name: '关闭添加来源' }).click()
  const textRow = panel
    .getByRole('article')
    .filter({ has: page.getByRole('heading', { name: '粘贴来源.md', exact: true }) })
  await expect(textRow).toContainText('用户提供的文本')
  await textRow.getByRole('button', { name: '查看来源：粘贴来源.md' }).click()
  const viewer = page.getByRole('dialog', { name: '来源查看器', exact: true })
  await viewer.getByText(/原文片段 1/).click()
  await expect(viewer.getByRole('region', { name: '来源原文片段' })).toContainText(
    'Notebook 文本片段',
  )
  await viewer.getByRole('button', { name: '关闭来源', exact: true }).click()
  await expect(textRow.getByRole('button')).toBeFocused()

  await panel.getByRole('button', { name: '＋ 添加来源' }).click()
  await dialog.getByRole('button', { name: /开始另一批/ }).click()
  await dialog.getByRole('button', { name: 'PDF 上传', exact: true }).click()
  await dialog.getByRole('checkbox', { name: /我有权上传并保存 PDF/ }).check()
  await dialog
    .getByLabel('选择 Markdown / UTF-8 / PDF 文件（可多选）')
    .setInputFiles({
      name: 'notebook.pdf',
      mimeType: 'application/pdf',
      buffer: Buffer.from(
        readFileSync(
          '../../server/internal/pdfsource/testdata/bilingual-embedded.pdf.base64',
          'utf8',
        ),
        'base64',
      ),
    })
  await expect(dialog.getByText('待导入 0 项 · 错误 0 · 不支持 0 · 被排除 1')).toBeVisible()
  await dialog.getByRole('checkbox', { name: /我已核对逐页缺口/ }).check()
  await dialog.getByRole('button', { name: '生成服务端预览' }).click()
  await dialog.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(dialog.getByRole('heading', { name: '资料已导入，尚未自动用于目标' })).toBeVisible()
  await dialog.getByRole('button', { name: '关闭添加来源' }).click()
  const pdfRow = panel
    .getByRole('article')
    .filter({ has: page.getByRole('heading', { name: 'notebook.pdf.md' }) })
  await expect(pdfRow).toContainText('部分解析')
  await pdfRow.getByRole('button').click()
  await viewer.getByText('PDF 逐页覆盖报告（共 3 页）').click()
  await expect(viewer).toContainText('无可用文本层')
  await viewer.getByText(/原文片段 .*第 2 页/).click()
  await expect(viewer.getByRole('region', { name: '来源原文片段' })).toContainText(
    '第二物理页：版本引用保持准确',
  )
  await viewer.getByRole('button', { name: /查看 PDF 原页/ }).click()
  await expect(viewer.getByRole('img')).toBeVisible()
  await viewer.getByRole('button', { name: '关闭来源', exact: true }).click()

  await panel.getByRole('button', { name: '＋ 添加来源' }).click()
  await dialog.getByRole('button', { name: /开始另一批/ }).click()
  await dialog.getByRole('button', { name: '网址', exact: true }).click()
  await dialog.getByLabel('公开网页链接').fill('http://notebook.example/start')
  await dialog.getByRole('button', { name: '授权服务器读取此网页并解析' }).click()
  await dialog.getByLabel('导入相对名称').fill('网页来源.md')
  await dialog.getByRole('button', { name: '生成服务端预览' }).click()
  await dialog.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(dialog.getByRole('heading', { name: '资料已导入，尚未自动用于目标' })).toBeVisible()
  await dialog.getByRole('button', { name: '关闭添加来源' }).click()
  const urlRow = panel
    .getByRole('article')
    .filter({ has: page.getByRole('heading', { name: '网页来源.md' }) })
  await expect(urlRow).toContainText('网址')
  await urlRow.getByRole('button').click()
  await expect(viewer).toContainText('实际读取：http://notebook.example/page')
  await viewer.getByText(/原文片段 1/).click()
  await expect(viewer.getByRole('region', { name: '来源原文片段' })).toContainText(
    'Notebook 网页片段',
  )
  await viewer.getByRole('button', { name: '关闭来源', exact: true }).click()
  await expect(panel.getByRole('article')).toHaveCount(3)
  await expect(panel.getByRole('checkbox')).toHaveCount(0)
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(panel).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.reload()
  await expect(panel.getByRole('article')).toHaveCount(3)
})
