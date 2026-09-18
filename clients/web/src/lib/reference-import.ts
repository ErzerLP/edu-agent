import type { ImportPreview, ImportRequest, ImportResult } from '@/api/references'
import type { PDFReport } from '@/api/pdf'

export type ScanItem = { id: string; name: string; path: string; bytes: number; status: 'ready' | 'error' | 'unsupported'; reason: string; selected: boolean; markdown: string; coverage: string; asNew?: boolean; pdf?: { data: string; report: PDFReport; acceptPartial: boolean; locator?: string; sourceReceipt?: string } }
export type ImportDraft = { items: ScanItem[]; paste: string; pasteName: string; url: string; request?: ImportRequest; preview?: ImportPreview; result?: ImportResult; unknown: boolean; previousOperation?: string }
export const emptyImport = (): ImportDraft => ({ items: [], paste: '', pasteName: '粘贴资料', url: '', unknown: false })
export const maxFile = 4 << 20
export const maxBatch = 12 << 20
const bytes = (text: string) => new TextEncoder().encode(text).length
export function textDocument(name: string, text: string) {
  let length = 3
  for (const match of text.matchAll(/`+/g)) length = Math.max(length, match[0].length + 1)
  const fence = '`'.repeat(length)
  return `# ${name.replace(/[\r\n]/g, ' ')}\n\n${fence}text\n${text}\n${fence}\n`
}
export function pastedItem(name: string, text: string): ScanItem {
  const id = crypto.randomUUID()
  return { id, name, path: `${name.replace(/[\\/:*?"<>|\x00-\x1f]/g, '_') || '粘贴资料'}.md`, bytes: bytes(text), status: !text.trim() || bytes(text) > maxFile ? 'error' : 'ready', reason: !text.trim() ? '内容为空' : bytes(text) > maxFile ? '内容超过 4 MiB' : '保留粘贴原文', selected: !!text.trim() && bytes(text) <= maxFile, markdown: textDocument(name, text), coverage: '完整文本' }
}

// 只读取浏览器授权返回的 File；不接受本地路径字符串或自行遍历目录。
export async function scanFiles(files: File[], parsePDF?: (data: string) => Promise<{ pdf?: PDFReport; text: string }>): Promise<ScanItem[]> {
  const result: ScanItem[] = []
  let used = 0
  for (const file of files) {
    const path = file.webkitRelativePath || file.name
    const item: ScanItem = { id: crypto.randomUUID(), name: path, path: /\.txt$/i.test(path) ? `${path}.md` : path.replace(/\.markdown$/i, '.md'), bytes: file.size, status: 'error', reason: '', selected: false, markdown: '', coverage: '完整文本' }
    if (!/\.(md|markdown|txt|pdf)$/i.test(path) || /\.pdf$/i.test(path) && !parsePDF) { item.status = 'unsupported'; item.reason = '支持 Markdown、UTF-8 文本和授权上传的文本层 PDF；此格式未读取' }
    else if (path.startsWith('/') || path.includes('\\') || path.split('/').some(v => !v || v === '.' || v === '..') || /[\x00-\x1f]/.test(path)) item.reason = '相对名称无效'
    else if (file.size > maxFile || used + file.size > maxBatch || result.length >= 1000) item.reason = '超过单文件 4 MiB、单批 12 MiB 或 1000 项预算'
    else {
      try {
        const raw = await file.arrayBuffer()
        if (/\.pdf$/i.test(path) && parsePDF) {
          const data = bytesToBase64(new Uint8Array(raw))
          const parsed = await parsePDF(data)
          if (!parsed.pdf) throw new Error('服务器没有返回逐页 PDF 报告')
          item.path = `${path}.md`
          item.pdf = { data, report: parsed.pdf, acceptPartial: false }
          item.markdown = parsed.text
          item.coverage = parsed.pdf.coverage === 'complete_text' ? '文本层已提取，不代表已验证知识' : '部分解析，需明确选择可用部分'
          item.status = parsed.text.trim() ? 'ready' : 'error'
          item.selected = item.status === 'ready' && parsed.pdf.coverage === 'complete_text'
          item.reason = item.status === 'ready' ? `已解析 ${parsed.pdf.page_count} 页` : '没有可用文本层，未运行 OCR'
          used += data.length
          result.push(item)
          continue
        }
        const text = new TextDecoder('utf-8', { fatal: true }).decode(raw)
        if (text.includes('\0')) throw new Error('正文含 NUL 字符')
        item.markdown = /\.txt$/i.test(path) ? textDocument(file.name, text) : text
        if (bytes(item.markdown) > maxFile || used + bytes(item.markdown) > maxBatch) throw new Error('转换后超过正文预算')
        item.status = 'ready'; item.selected = true; item.reason = 'UTF-8 解析成功'
        used += bytes(item.markdown)
      } catch (error) { item.reason = error instanceof TypeError ? '编码错误：请转换为 UTF-8' : error instanceof Error ? error.message : '文件读取失败' }
    }
    result.push(item)
  }
  return result
}
export function importDocuments(items: ScanItem[]) {
  const selected = items.filter(i => i.selected && i.status === 'ready')
  if (!selected.length) throw new Error('请至少选择一项成功解析的资料')
  const paths = new Set<string>()
  let used = 0
  return selected.map(i => {
    const path = i.path.normalize('NFC')
    if (paths.has(path.toLocaleLowerCase())) throw new Error(`同批路径重复：${path}。请修改名称或本次跳过。`)
    paths.add(path.toLocaleLowerCase()); used += i.pdf ? i.pdf.data.length : bytes(i.markdown)
    if (used > maxBatch || selected.length > 1000) throw new Error('所选内容超过单批预算')
    if (i.pdf && i.pdf.report.coverage !== 'complete_text' && !i.pdf.acceptPartial) throw new Error('请先明确确认 PDF 仅纳入可用部分')
    return { path, markdown: i.pdf ? '' : i.markdown, ...(i.asNew ? { as_new: true } : {}), ...(i.pdf ? { pdf: { data: i.pdf.data, accept_partial: i.pdf.acceptPartial, ...(i.pdf.locator ? { locator: i.pdf.locator, source_receipt: i.pdf.sourceReceipt } : {}) } } : {}) }
  })
}

export function bytesToBase64(data: Uint8Array) {
  let text = ''
  for (let i = 0; i < data.length; i += 8192) text += String.fromCharCode(...data.subarray(i, i + 8192))
  return btoa(text)
}

// 任何输入变化都会淘汰旧批准；提交结果未知时必须先核对原操作。
export function editImport(draft: ImportDraft, change: Partial<ImportDraft>): ImportDraft {
  if (draft.unknown) throw new Error('请先核对原操作结果')
  return { ...draft, ...change, previousOperation: draft.request?.operation_id ?? draft.previousOperation, request: undefined, preview: undefined, result: undefined }
}
