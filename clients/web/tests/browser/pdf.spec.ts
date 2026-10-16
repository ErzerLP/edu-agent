import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { mkdir, writeFile, truncate, unlink } from 'node:fs/promises'
import { pairNotebook, webAPI, defaultSpace } from './notebook-fixture'

test('同名 PDF 平移页段含新增页，经文档与章节审阅生成新版本，旧引用保留旧版本', async ({ page }) => {
  test.setTimeout(120_000)
  await pairNotebook(page, 'references')
  const goal = randomUUID()
  await webAPI(page, 'POST', '/v1/learning/goals', { operation_id: randomUUID(), payload_schema_version: 1, aggregate_type: 'goal', aggregate_id: goal, expected_version: 0, text: '平移页段修订', source: '浏览器验收', details: { name: '平移页段修订' } }, defaultSpace)
  await page.goto(`/app/spaces/${defaultSpace}/knowledge?goal=${goal}`)
  const name = `教材修订 ${randomUUID().slice(0, 8)}`
  await page.getByLabel('集合名称', { exact: true }).fill(name)
  await page.getByRole('button', { name: '创建私有集合' }).click()
  const importer = page.getByRole('region', { name: '单批资料导入', exact: true })
  await importer.getByRole('checkbox', { name: /我有权上传并保存 PDF/ }).check()
  const pages = ['原书25页 真值与机器数', '原书26页 模与补码', '原书27页 原码与补码', '原书28页 定点数', '原书29页 浮点数', '原书30页 移码']
  const pdf = (text: string[]) => execFileSync('go', ['run', './testutil/webpdffixture', ...text], { cwd: '../../server' })
  const input = importer.getByLabel('选择 Markdown / UTF-8 / PDF 文件（可多选）')
  await input.setInputFiles({ name: '教材.pdf', mimeType: 'application/pdf', buffer: pdf(pages) })
  await importer.getByRole('checkbox', { name: /我已核对逐页缺口，仅纳入/ }).check()
  await expect(importer.getByText('待导入 1 项 · 错误 0 · 不支持 0 · 被排除 0')).toBeVisible()
  await importer.getByRole('button', { name: '生成服务端预览', exact: true }).click()
  const confirmedRequests: unknown[] = []
  const confirm = async () => {
    const response = page.waitForResponse(r => r.url().endsWith('/v1/knowledge/imports/confirm') && r.request().method() === 'POST')
    await importer.getByRole('button', { name: '确认正式导入这 1 项资料', exact: true }).click()
    const value = await response
    confirmedRequests.push(value.request().postDataJSON().request)
    expect(value.ok(), await value.text()).toBe(true)
    await expect(importer.getByRole('heading', { name: '资料已导入，尚未自动用于目标' })).toBeVisible()
    return value.json()
  }
  const first = await confirm()
  const collectionId = first.summary.collection_id
  const tree = async (revision: string) => {
    const response = await page.request.get(`/v1/knowledge/revisions/${revision}/tree`, { headers: { 'X-Learning-Space-ID': defaultSpace, 'X-Knowledge-Collection-ID': collectionId } })
    expect(response.ok(), await response.text()).toBe(true)
    return response.json()
  }
  const originalTree = await tree(first.revision.revision_id)
  await page.getByRole('button', { name: '预览参考角色与范围变更', exact: true }).click()
  await page.getByRole('button', { name: '正式采用这些参考角色', exact: true }).click()
  await expect(page.getByText('已采用，供后续准备使用；当前题目保留原引用。')).toBeVisible()
  const frozen = await webAPI(page, 'GET', `/v1/learning/goals/${goal}/references`, undefined, defaultSpace)
  expect(frozen.selection.entries).toHaveLength(1)
  expect(frozen.selection.entries[0].revision_id).toBe(first.revision.revision_id)

  await importer.getByRole('button', { name: '开始另一批（保留原操作关联）' }).click()
  await input.setInputFiles({ name: '教材.pdf', mimeType: 'application/pdf', buffer: pdf([...pages.slice(1), '原书31页 校验码']) })
  await importer.getByRole('checkbox', { name: /我已核对逐页缺口，仅纳入/ }).check()
  await importer.getByRole('button', { name: '生成服务端预览', exact: true }).click()
  const review = importer.getByRole('region', { name: '身份审阅', exact: true })
  await expect(review.getByRole('combobox')).toHaveCount(1)
  await review.getByRole('combobox').selectOption({ label: '更新原资料候选 1' })
  await review.getByRole('button', { name: '按身份决定重新预览' }).click()
  await expect(review.getByRole('heading', { name: '本次章节：第 6 页', exact: true })).toBeVisible()
  await expect(review.getByText('原书31页 校验码', { exact: true })).toBeVisible()
  await expect(review.getByRole('heading', { name: '旧章节：第 6 页', exact: true })).toBeVisible()
  await expect(review.getByText('原书30页 移码', { exact: true })).toBeVisible()
  await review.getByRole('combobox').selectOption({ label: '作为新章节' })
  if (process.env.WEB_VISUAL_DIR) {
    await mkdir(process.env.WEB_VISUAL_DIR, { recursive: true })
    await review.screenshot({ path: `${process.env.WEB_VISUAL_DIR}/reviewloop-chapter-review.png` })
  }
  const previewResponse = page.waitForResponse(r => r.url().endsWith('/v1/knowledge/imports/previews') && r.request().method() === 'POST')
  await review.getByRole('button', { name: '按身份决定重新预览' }).click()
  const resolved = await previewResponse
  const resolutions = resolved.request().postDataJSON()
  expect(resolutions.document_resolutions).toHaveLength(1)
  expect(resolutions.node_resolutions).toHaveLength(1)
  expect((await resolved.json()).status).toBe('ready')
  await expect(importer.getByRole('heading', { name: '尚未正式导入' })).toBeVisible()
  await expect(review).toHaveCount(0)
  // 不改输入而再次预览，仍必须带着全部决定。
  const repeated = page.waitForResponse(r => r.url().endsWith('/v1/knowledge/imports/previews') && r.request().method() === 'POST')
  await importer.getByRole('button', { name: '生成服务端预览', exact: true }).click()
  expect((await repeated).request().postDataJSON()).toEqual(resolutions)
  const second = await confirm()
  expect(confirmedRequests[1]).toEqual(resolutions)
  expect(second.revision.revision_no).toBe(2)
  expect(second.revision.revision_id).not.toBe(first.revision.revision_id)
  expect(second.summary).toMatchObject({ added: 0, updated: 1 })
  const revisedTree = await tree(second.revision.revision_id)
  expect(revisedTree.revision.parent_revision_id).toBe(first.revision.revision_id)
  const before = originalTree.revision.documents[0].document
  const after = revisedTree.revision.documents[0].document
  expect(after.document_id).toBe(before.document_id)
  expect(after.document_revision_id).not.toBe(before.document_revision_id)
  expect(after.nodes.slice(1, 6).map((n: { node_id: string }) => n.node_id)).toEqual(before.nodes.slice(2).map((n: { node_id: string }) => n.node_id))
  expect(before.nodes.map((n: { node_id: string }) => n.node_id)).not.toContain(after.nodes[6].node_id)
  expect(after.pdf.report.pages[5].text).toContain('原书31页 校验码')
  const current = await webAPI(page, 'GET', `/v1/learning/goals/${goal}/references`, undefined, defaultSpace)
  expect(current.scope_snapshot_id).toBe(frozen.scope_snapshot_id)
  expect(current.selection).toEqual(frozen.selection)
  const historical = await webAPI(page, 'GET', `/v1/knowledge/scopes/${frozen.scope_snapshot_id}/export`, undefined, defaultSpace)
  expect(historical.documents[0].markdown).toContain('原书25页 真值与机器数')
  expect(historical.documents[0].markdown).not.toContain('原书31页 校验码')
  if (process.env.WEB_VISUAL_DIR) {
    await importer.getByRole('heading', { name: '资料已导入，尚未自动用于目标' }).scrollIntoViewIfNeeded()
    await page.screenshot({ path: `${process.env.WEB_VISUAL_DIR}/reviewloop-revision-success.png` })
  }
  await page.getByRole('button', { name: '查看旧冻结范围与更新提示', exact: true }).click()
  await expect(page.getByRole('heading', { name: '已采用的冻结范围', exact: true })).toBeVisible()
  await expect(page.getByText('固定 1 个范围 · 1 个集合有新版本，未自动更新。')).toBeVisible()
})

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
