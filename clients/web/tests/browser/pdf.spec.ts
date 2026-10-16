import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { mkdir, writeFile, truncate, unlink } from 'node:fs/promises'

test('真实 PDF 上传、覆盖确认、安全页图及移动端键盘定位', async ({ page }) => {
  test.setTimeout(120_000)
  const code = execFileSync(
    '../../server/edu-agentd',
    ['pairing-code', 'create', '--profile', 'references'],
    {
      encoding: 'utf8',
      env: {
        PATH: process.env.PATH,
        DATABASE_URL: process.env.TEST_DATABASE_URL,
        MIGRATE_ON_START: 'true',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  ).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '你的学习区', exact: true, level: 1 })).toBeVisible()
  await page.goto('/app/spaces/00000000-0000-4000-8000-000000000001/knowledge')
  const name = `PDF 集合 ${randomUUID().slice(0, 8)}`
  await page.getByLabel('集合名称', { exact: true }).fill(name)
  await page.getByRole('button', { name: '创建私有集合' }).click()
  await page
    .getByRole('checkbox', { name: '我有权上传并保存 PDF 原件及提取文本，用于参考和安全页查看' })
    .check()
  await page
    .getByLabel('选择 Markdown / UTF-8 / PDF 文件（可多选）')
    .setInputFiles({
      name: 'bilingual-embedded.pdf',
      mimeType: 'application/pdf',
      buffer: Buffer.from(
        readFileSync(
          '../../server/internal/pdfsource/testdata/bilingual-embedded.pdf.base64',
          'utf8',
        ),
        'base64',
      ),
    })
  await expect(page.getByText('待导入 0 项 · 错误 0 · 不支持 0 · 被排除 1')).toBeVisible()
  await page.getByText('PDF 逐页覆盖报告（共 3 页）').click()
  await expect(page.getByText('第 3 页：无可用文本层（扫描或空白页）')).toBeVisible()
  await page.getByText('查看第 2 页提取文本').click()
  await expect(
    page.getByText('第二物理页：版本引用保持准确', { exact: false }).first(),
  ).toBeVisible()
  await page.getByRole('checkbox', { name: /我已核对逐页缺口，仅纳入/ }).check()
  await page.getByRole('button', { name: '生成服务端预览' }).click()
  await expect(page.getByRole('heading', { name: '尚未正式导入' })).toBeVisible()
  await page.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(page.getByRole('heading', { name: '资料已导入，尚未自动用于目标' })).toBeVisible()
  await page.getByRole('button', { name: /查看 PDF 原页/ }).click()
  const select = page.getByLabel('定位 PDF 物理页')
  await select.focus()
  await select.press('ArrowDown')
  await expect(select).toHaveValue('2')
  const image = page.getByAltText('被引用 PDF 历史版本的第 2 页')
  await expect(image).toBeVisible()
  await expect
    .poll(() => image.evaluate((img: HTMLImageElement) => img.naturalWidth))
    .toBeGreaterThan(0)
  await page.setViewportSize({ width: 390, height: 844 })
  await image.scrollIntoViewIfNeeded()
  await page.screenshot({ path: test.info().outputPath('pdf-page-two.png') })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy()
  await expect(page.getByRole('button', { name: '下一页', exact: true })).toBeEnabled()
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(page.getByAltText('被引用 PDF 历史版本的第 3 页')).toBeVisible()
  const audit = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()
  expect(audit.violations.map((v) => v.id)).toEqual([])
})

test('PDF 超限、全页无文本与集合名称展示', async ({ page }) => {
  const { pairNotebook, createSpace, webAPI } = await import('./notebook-fixture')
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'PDF 提示文案验收')
  await page.goto(`/app/spaces/${space.id}?panel=sources`)
  await page.getByRole('button', { name: '＋ 添加来源', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '添加来源', exact: true })
  await dialog.getByLabel('新集合名称').fill('来源集合')
  await dialog.getByRole('button', { name: '创建并选用集合' }).click()
  const destination = dialog.getByLabel('保存到已关联集合')
  await expect(destination.locator('option:checked')).toHaveText('来源集合')
  await webAPI(page, 'POST', '/v1/knowledge/collections', { id: randomUUID(), action: 'create', name: '来源集合', source: '同名展示验收' }, space.id)
  await dialog.getByRole('button', { name: '关闭添加来源' }).click()
  await page.getByRole('button', { name: '＋ 添加来源', exact: true }).click()
  await expect(destination.locator('option:checked')).toHaveText(/来源集合（同名集合 [12]）/)
  await expect(destination.locator('option')).toHaveText(['请选择或创建集合', '来源集合（同名集合 1）', '来源集合（同名集合 2）'])
  if (process.env.WEB_VISUAL_DIR) {
    await mkdir(process.env.WEB_VISUAL_DIR, { recursive: true })
    await dialog.screenshot({ path: `${process.env.WEB_VISUAL_DIR}/03-collection-name.png` })
  }
  await dialog.getByRole('checkbox', { name: /我有权上传并保存 PDF/ }).check()
  const uploads: string[] = []
  page.on('request', request => { if (request.url().endsWith('/reference-sources')) uploads.push(request.url()) })
  const input = dialog.getByLabel('选择 Markdown / UTF-8 / PDF 文件（可多选）')
  const oversizedPath = test.info().outputPath('教材.pdf')
  await writeFile(oversizedPath, '')
  await truncate(oversizedPath, 70_809_807)
  try { await input.setInputFiles(oversizedPath) } finally { await unlink(oversizedPath) }
  const oversized = dialog.getByRole('article').filter({ hasText: '教材.pdf' })
  await expect(oversized).toContainText('67.5 MiB，超过单个 PDF 4 MiB 上限')
  await expect(oversized).not.toContainText('完整文本')
  expect(uploads).toEqual([])
  if (process.env.WEB_VISUAL_DIR) await oversized.screenshot({ path: `${process.env.WEB_VISUAL_DIR}/01-size-rejected.png` })

  // 一页无文本层的真实 PDF，经正式解析接口返回逐页报告。
  const objects = ['<< /Type /Catalog /Pages 2 0 R >>', '<< /Type /Pages /Kids [3 0 R] /Count 1 >>', '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Resources << >> /Contents 4 0 R >>', '<< /Length 0 >>\nstream\n\nendstream']
  let pdf = '%PDF-1.4\n'
  const offsets = [0]
  objects.forEach((object, index) => { offsets.push(Buffer.byteLength(pdf)); pdf += `${index + 1} 0 obj\n${object}\nendobj\n` })
  const xref = Buffer.byteLength(pdf)
  pdf += `xref\n0 5\n0000000000 65535 f \n${offsets.slice(1).map(offset => `${String(offset).padStart(10, '0')} 00000 n \n`).join('')}trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`
  await input.setInputFiles({ name: '无文本层.pdf', mimeType: 'application/pdf', buffer: Buffer.from(pdf) })
  const noText = dialog.getByRole('article').filter({ hasText: '无文本层.pdf' })
  await expect(noText).toContainText('没有可导入的文本：这份 PDF 是扫描图片，暂不支持 OCR。请使用可选中文字的 PDF。')
  await expect(noText).not.toContainText('部分解析')
  await expect(noText.getByRole('checkbox')).toBeDisabled()
  await expect(dialog.getByRole('button', { name: '生成服务端预览' })).toBeDisabled()
  if (process.env.WEB_VISUAL_DIR) await noText.screenshot({ path: `${process.env.WEB_VISUAL_DIR}/02-no-importable-text.png` })
})
