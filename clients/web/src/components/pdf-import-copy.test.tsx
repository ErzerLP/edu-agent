import { expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ReferenceImport } from './reference-import'
import { AddSourceContent } from './notebook/add-source-dialog'
import { scanFiles, type ScanItem } from '@/lib/reference-import'
import type { Collection } from '@/api/references'
import type { PDFReport } from '@/api/pdf'

vi.hoisted(() => { vi.stubGlobal('window', { location: { origin: 'http://localhost' } }) })

vi.mock('@/lib/session', async () => {
  const { DraftStore } = await import('@/lib/drafts')
  return { useIdentity: () => ({ session: {}, prefix: ['copy-test'], drafts: new DraftStore() }) }
})

const collection: Collection = { id: '5adbe6c0-0000-4000-8000-000000000001', name: '来源集合', source: '', shared: false, head_revision_id: null, version: 1 }
const report: PDFReport = { parser: 'fixture', fingerprint: 'a'.repeat(64), page_count: 1, coverage: 'partial_pdf', pages: [{ number: 1, status: 'no_text', gaps: ['no_text_layer_or_blank'], text: '', fingerprint: '', start: 0, end: 0 }] }
const renderItems = (items: ScanItem[]) => renderToStaticMarkup(<ReferenceImport spaceId="space" collection={collection} target="notebook" method="pdf" initialItems={items} onImported={() => {}} />)

it('超限失败行显示实际大小和单 PDF 上限，隐藏完整文本标签', async () => {
  const items = await scanFiles([new File([new Uint8Array(70_809_807)], '教材.pdf')], async () => { throw new Error('超限文件不应解析') })
  const html = renderItems(items)
  expect(html).toContain('67.5 MiB，超过单个 PDF 4 MiB 上限')
  expect(html).not.toContain('完整文本')
  expect(html).not.toContain('单批 12 MiB 或 1000 项预算')
})

it('全部无文本层时解释扫描件和替代方案，不再提示选择可用部分', async () => {
  const items = await scanFiles([new File(['%PDF'], '扫描.pdf')], async () => ({ text: '', pdf: report }))
  const html = renderItems(items)
  expect(html).toContain('没有可导入的文本：这份 PDF 是扫描图片，暂不支持 OCR。请使用可选中文字的 PDF。')
  expect(html).not.toContain('部分解析')
  expect(html).not.toContain('我已核对逐页缺口')
  expect(html).toContain('PDF 逐页覆盖报告')
})

it('仍有可用文本的 PDF 保留部分解析与明确确认', async () => {
  const items = await scanFiles([new File(['%PDF'], '部分.pdf')], async () => ({ text: '可用正文', pdf: { ...report, page_count: 2, pages: [...report.pages, { ...report.pages[0], number: 2, status: 'text', text: '可用正文' }] } }))
  const html = renderItems(items)
  expect(html).toContain('部分解析，需明确选择可用部分')
  expect(html).toContain('我已核对逐页缺口')
  expect(html).not.toContain('这份 PDF 是扫描图片')
})

it('集合选项仅显示名称，共享标记与同名序号，不暴露内部 ID', () => {
  const client = new QueryClient()
  client.setQueryData(['copy-test', 'space', 'notebook-collections'], { items: [collection, { ...collection, id: 'bbbbbbbb-0000-4000-8000-000000000002' }, { ...collection, id: 'cccccccc-0000-4000-8000-000000000003', name: '教材', shared: true }] })
  const html = renderToStaticMarkup(<QueryClientProvider client={client}><AddSourceContent spaceId="space" /></QueryClientProvider>)
  const labels = [...html.matchAll(/<option[^>]*>(.*?)<\/option>/g)].map(match => match[1])
  expect(labels).toEqual(['请选择或创建集合', '来源集合（同名集合 1）', '来源集合（同名集合 2）', '教材（共享集合）'])
})
