import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { webCoverage, webDocumentSchema, webImportSchema, webPreviewSchema, type WebCitation } from '@/api/web-search'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { PDFCoverage, PDFPageViewer } from '../pdf-source'
import { Button } from '../ui/button'
import type { z } from 'zod'

export function WebCitationContent({ spaceId, runId, citation, importEnabled = true }: { spaceId: string; runId: string; citation: WebCitation; importEnabled?: boolean }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const [preview, setPreview] = useState<z.infer<typeof webPreviewSchema>>()
  const [saved, setSaved] = useState(false)
  const [partial, setPartial] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const header = { 'X-Learning-Space-ID': spaceId }
  const api = () => learningClient(session, spaceId)
  const key = JSON.stringify([...prefix, spaceId, citation.revision_id, 'web-import'])
  const detail = useQuery({
    queryKey: [...prefix, spaceId, citation.document_id, 'web-document'], gcTime: 0, retry: false,
    queryFn: async ({ signal }) => {
      let cursor: string | undefined
      do {
        const document = await unwrap(api().GET('/v1/learning/web-documents/{documentID}', { params: { header, path: { documentID: citation.document_id }, query: { cursor } }, signal }), webDocumentSchema)
        if (document.revision_id !== citation.revision_id || document.document_sha256 !== citation.document_sha256) throw new ApiError(409, 'version_conflict')
        if (document.fragments.some(f => f.id === citation.fragment_id)) return document
        cursor = document.next_cursor
      } while (cursor)
      throw new ApiError(404, 'run_not_found')
    },
  })
  const canImport = ['learning:write', 'knowledge:read', 'knowledge:write', 'research:adopt'].every(scope => session.device.scopes.includes(scope))
  const startPreview = async () => {
    setBusy(true); setError(undefined)
    try {
      const body = { owner: { kind: 'mentor_run' as const, id: runId }, document_revision_ids: [citation.revision_id] }
      const result = await unwrap(api().POST('/v1/learning/web-import-previews', { params: { header }, body: { ...body, operation_id: drafts.operation(key + ':preview', body) } }), webPreviewSchema)
      setPartial(false); setPreview(result)
    } catch (e) { setError(e) } finally { setBusy(false) }
  }
  const save = async () => {
    if (!preview) return
    setBusy(true); setError(undefined)
    try {
      const body = { preview_id: preview.preview_id, expected_preview_version: preview.preview_version, document_revision_id: citation.revision_id, storage_consent: true as const, accept_partial: partial }
      await unwrap(api().POST('/v1/learning/web-imports', { params: { header }, body: { ...body, operation_id: drafts.operation(key + ':save', body) } }), webImportSchema)
      setSaved(true); setPreview(undefined)
      await query.invalidateQueries({ predicate: item => item.queryKey.includes(spaceId) })
    } catch (e) { setError(e) } finally { setBusy(false) }
  }
  if (detail.error) return <p role="alert">网页内容已过期、已删除或当前不可读取。</p>
  if (!detail.data) return <p role="status">正在核对网络来源…</p>
  const document = detail.data
  const fragment = document.fragments.find(f => f.id === citation.fragment_id)!
  return <section aria-label="网络来源">
    <p>网络来源</p><h3>{document.title}</h3>
    <p>{new URL(document.url).hostname} · 读取于 {new Date(document.fetched_at).toLocaleString()}</p>
    <p>{webCoverage[document.coverage] ?? '仅引用实际读取的文本，页面可能存在读取缺口。'}</p>
    {citation.page && <p>PDF 物理页码：第 {citation.page} 页</p>}
    <blockquote className="reference-text">{fragment.text}</blockquote>
    <a href={document.url} target="_blank" rel="noopener noreferrer">打开原网页</a>
    {document.pdf && <><PDFCoverage report={document.pdf} /><PDFPageViewer spaceId={spaceId} revisionId={document.revision_id} documentId={document.document_id} webDocumentId={document.document_id} pages={document.pdf.pages.map(p => p.number)} selectedText={citation.quote} /></>}
    {importEnabled && (saved ? <p role="status">已加入来源</p> : <Button disabled={busy || !canImport} onClick={() => void startPreview()}>加入来源</Button>)}
    {importEnabled && !canImport && <p>当前身份没有加入来源的权限。</p>}
    {preview && <section aria-label="加入来源预览">
      <h4>加入来源预览</h4><p>{preview.items[0].document.title}</p>
      <p>保存为当前学习区的私有新来源。相同网址的历史资料不会被覆盖，保存后不会自动选中。</p>
      <pre className="reference-text">{preview.items[0].document.text}</pre>
      {preview.items[0].requires_partial_accept && <label><input type="checkbox" checked={partial} onChange={e => setPartial(e.target.checked)} />仅保存可用文本</label>}
      <Button disabled={busy || preview.items[0].requires_partial_accept && !partial} onClick={() => void save()}>确认加入来源</Button>
      <Button variant="outline" disabled={busy} onClick={() => setPreview(undefined)}>取消</Button>
    </section>}
    {busy && <p role="status">正在处理…</p>}{!!error && <ErrorState error={error} />}
  </section>
}
