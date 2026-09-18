import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'

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
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
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
