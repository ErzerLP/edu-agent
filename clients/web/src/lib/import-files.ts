import type { ImportItem, ImportJob, ImportCommand } from '../api/import-jobs'

export const importLimits = {
  file: 4 << 20,
  request: 16 << 20,
  task: 128 << 20,
  count: 1000,
  scan: 5000,
}
export type LocalImport = { item: ImportItem; content: Uint8Array<ArrayBuffer>; name: string }
export type ScanItem = {
  name: string
  bytes: number
  selected: boolean
  error?: string
  source?: LocalImport
}
const encoder = new TextEncoder()
export async function sha256(bytes: Uint8Array<ArrayBuffer>) {
  return Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)), (n) =>
    n.toString(16).padStart(2, '0'),
  ).join('')
}
export function base64(bytes: Uint8Array) {
  let text = ''
  for (let i = 0; i < bytes.length; i += 8192)
    text += String.fromCharCode(...bytes.subarray(i, i + 8192))
  return btoa(text)
}
function validPath(path: string) {
  return (
    path.length > 0 &&
    [...path].length <= 512 &&
    encoder.encode(path).length <= 1024 &&
    !/[\\\p{Cc}]/u.test(path) &&
    path.split('/').every((part) => part !== '' && part !== '.' && part !== '..')
  )
}
export async function scanImportFiles(files: File[], signal?: AbortSignal): Promise<ScanItem[]> {
  if (files.length > importLimits.scan) throw new Error('选择超过 5000 项，请缩小来源范围。')
  const result: ScanItem[] = []
  let total = 0
  for (const file of files) {
    signal?.throwIfAborted()
    const name = (file.webkitRelativePath || file.name).normalize('NFC')
    const row: ScanItem = { name, bytes: file.size, selected: false }
    result.push(row)
    try {
      if (!validPath(name)) throw new Error('相对路径不规范。')
      if (!/\.(md|txt)$/i.test(name))
        throw new Error('不支持此格式，只接受 UTF-8 Markdown 或纯文本。')
      if (file.size > importLimits.file) throw new Error('超过单文件 4 MiB 上限。')
      if (total + file.size > importLimits.task)
        throw new Error('选择超过任务 128 MiB 上限，请缩小来源范围。')
      const raw = new Uint8Array(await file.arrayBuffer())
      // 保留 BOM 与原始字节，不能用宽松解码悄悄替换损坏文本。
      let text: string
      try {
        text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(raw)
      } catch {
        throw new Error('编码错误：仅接受 UTF-8，请转换后重新选择。')
      }
      let path = name,
        content = raw
      if (/\.txt$/i.test(name)) {
        const metadata = JSON.stringify({
          name,
          sha256: await sha256(raw),
          type: 'text/plain; charset=utf-8',
        }).replace(
          /[<>&\u2028\u2029]/g,
          (ch) => `\\u${ch.charCodeAt(0).toString(16).padStart(4, '0')}`,
        )
        let fence = '```'
        while (text.includes(fence)) fence += '`'
        content = encoder.encode(
          `<!-- import-source-v1 ${base64(encoder.encode(metadata)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')} -->\n${fence}text\n${text}\n${fence}\n`,
        )
        path += '.md'
      }
      if (!validPath(path)) throw new Error('转换后的路径超限。')
      if (content.length > importLimits.file) throw new Error('转换后的正文超过单文件 4 MiB 上限。')
      total += content.length
      if (total > importLimits.task) throw new Error('选择超过任务 128 MiB 上限，请缩小来源范围。')
      const duplicate = result.find(
        (previous) => previous.source?.item.path.toLowerCase() === path.toLowerCase(),
      )
      if (duplicate) {
        duplicate.selected = false
        duplicate.error = '目标路径与其他来源冲突。'
        throw new Error(duplicate.error)
      }
      row.source = {
        name,
        content,
        item: { path, bytes: content.length, sha256: await sha256(content) },
      }
      row.selected = true
    } catch (e) {
      row.error = e instanceof Error ? e.message : '文件读取失败。'
    }
  }
  signal?.throwIfAborted()
  return result
}
export function selectedSources(rows: ScanItem[]) {
  const sources = rows
    .filter((row) => row.selected && !row.error && row.source)
    .map((row) => row.source!)
  if (
    !sources.length ||
    sources.length > importLimits.count ||
    sources.reduce((n, source) => n + source.item.bytes, 0) > importLimits.task
  )
    throw new Error('请选择 1–1000 个文件，正文总量不超过 128 MiB。')
  return sources
}
export function verifySources(job: ImportJob, sources: LocalImport[]) {
  const files = new Map(sources.map((source) => [source.item.path, source]))
  if (files.size !== job.batches.length)
    throw new Error('必须重新选择完整原清单；当前文件数量不一致。')
  for (const batch of job.batches) {
    const source = files.get(batch.item.path)
    if (
      !source ||
      source.item.bytes !== batch.item.bytes ||
      source.item.sha256 !== batch.item.sha256
    )
      throw new Error(
        `本地来源缺失或摘要变化：${batch.item.path}。恢复原文件，或创建新任务重新预览。`,
      )
  }
  return files
}
type JobAPI = {
  get(id: string): Promise<ImportJob>
  command(command: ImportCommand): Promise<ImportJob>
}
export async function uploadRemaining(
  api: JobAPI,
  id: string,
  sources: LocalImport[],
  signal: AbortSignal,
  update: (job: ImportJob) => void,
) {
  // 每次恢复先查原任务和未知批次回执，全部来源核对完成之前不发送正文。
  let job = await api.get(id)
  signal.throwIfAborted()
  update(job)
  const files = verifySources(job, sources)
  for (let batch = 0; batch < job.batches.length; batch++) {
    signal.throwIfAborted()
    const item = job.batches[batch]
    if (!['pending', 'missing'].includes(item.status)) continue
    const content = files.get(item.item.path)!.content
    job = await api.command({
      id,
      action: 'upload',
      batch,
      ...(content.length
        ? { content_base64: base64(content) }
        : { document: { path: item.item.path, markdown: '' } }),
    })
    signal.throwIfAborted()
    update(job)
  }
  return job
}
export async function continueImport(
  api: JobAPI,
  id: string,
  signal: AbortSignal,
  update: (job: ImportJob) => void,
  all = false,
) {
  let job = await api.get(id)
  signal.throwIfAborted()
  update(job)
  while (job.approved && !['completed', 'cancelled', 'expired'].includes(job.status)) {
    signal.throwIfAborted()
    job = await api.command({ id, action: 'continue' })
    signal.throwIfAborted()
    update(job)
    if (!all || job.status !== 'partial' || job.batches.some((batch) => batch.status === 'unknown'))
      break
  }
  return job
}
