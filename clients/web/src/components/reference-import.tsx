import { useEffect, useRef, useState } from 'react'
import { ApiError, errorText } from '@/api/client'
import { knowledgeAPI, type Collection, type ImportRequest, type ImportResult } from '@/api/references'
import { editImport, emptyImport, fetchedItem, importDocuments, maxFile, pastedItem, reviewedImport, scanFiles, type ImportDraft, type ScanItem } from '@/lib/reference-import'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { ErrorState } from './common'
import { PDFCoverage } from './pdf-source'
import { Upload, FileText } from 'lucide-react'
import { SubmissionNotice } from './notebook/submission-notice'

function itemDescription(item: ScanItem) {
  if (item.status !== 'ready') {
    if (item.status === 'error' && item.bytes > maxFile && item.reason.startsWith('超过单文件')) {
      return `${(item.bytes / (1 << 20)).toFixed(1)} MiB，超过单个 ${/\.pdf$/i.test(item.name) ? 'PDF' : '文件'} ${maxFile / (1 << 20)} MiB 上限`
    }
    if (item.pdf && !item.markdown.trim() && item.pdf.report.pages.length > 0 && item.pdf.report.pages.every(p => p.status === 'no_text')) {
      return '没有可导入的文本：这份 PDF 是扫描图片，暂不支持 OCR。请使用可选中文字的 PDF。'
    }
    return `${item.reason} · ${item.bytes} 字节`
  }
  return `${item.reason} · ${item.bytes} 字节 · ${item.coverage}${!item.selected ? ' · 本次排除' : ''}`
}

export function ReferenceImport({ spaceId, collection, target, onImported, method, initialItems }: { spaceId: string; collection: Collection; target: string; onImported: (result: ImportResult) => void; method?: 'pdf' | 'text' | 'url' | 'note'; initialItems?: ScanItem[] }) {
  const { session, prefix, drafts } = useIdentity()
  const key = JSON.stringify([...prefix, spaceId, target, collection.id, 'reference-import'])
  const [draft, setDraft] = useState<ImportDraft>(() => drafts.get<ImportDraft>(key) ?? { ...emptyImport(), items: initialItems ?? [] })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [localError, setLocalError] = useState('')
  const [search, setSearch] = useState('')
  const [pdfConsent, setPDFConsent] = useState(false)
  const [decisions, setDecisions] = useState<Record<string, string>>({})
  const [dragging, setDragging] = useState(false)
  const live = useRef(true)
  const api = knowledgeAPI(session, spaceId)
  useEffect(() => { live.current = true; return () => { live.current = false } }, [])
  const save = (next: ImportDraft) => { if (live.current) { drafts.set(key, next); setDraft(next) } }
  const edit = (change: Partial<ImportDraft>) => { save(editImport(draft, change)); setDecisions({}); setError(undefined); setLocalError('') }
  const run = async (fn: () => Promise<void>) => {
    if (busy) return
    setBusy(true); setError(undefined); setLocalError('')
    try { await fn() } catch (e) { if (live.current) { if (e instanceof ApiError || !(e instanceof Error)) setError(e); else setLocalError(e.message) } }
    finally { if (live.current) setBusy(false) }
  }
  const preview = async (request?: ImportRequest) => {
    const body = request ?? draft.request ?? { operation_id: crypto.randomUUID(), expected_parent_revision_id: (await api.collections()).items.find(c => c.id === collection.id)?.head_revision_id ?? null, source: `浏览器授权单批${draft.previousOperation ? `；关联原操作 ${draft.previousOperation}` : ''}`, documents: importDocuments(draft.items) }
    save({ ...draft, request: body, preview: undefined, result: undefined, unknown: false })
    const value = await api.preview(collection.id, body)
    save({ ...draft, request: body, preview: value, result: undefined, unknown: false })
    if (live.current) setDecisions({})
  }
  const reviewed = async () => {
    const review = draft.preview?.identity_review
    if (!review || !draft.request) return
    const documents = [...review.document_reviews]
    const nodes = [...review.node_reviews]
    if ([...documents, ...nodes].some(v => !decisions[v.locator])) throw new Error('请逐项选择更新、作为新资料或本次跳过')
    const skipped = new Set([...documents, ...nodes].filter(v => decisions[v.locator] === 'skip').map(v => v.path))
    if (skipped.size) {
      const items = draft.items.map(i => skipped.has(i.path) ? { ...i, selected: false } : i)
      edit({ items }); return
    }
    const newPaths = new Set(documents.filter(v => decisions[v.locator] === 'new').map(v => v.path))
    if (newPaths.size) {
      edit({ items: draft.items.map(i => newPaths.has(i.path) ? { ...i, asNew: true, path: i.path.replace(/\.md$/i, '') + `-副本-${i.id.slice(0, 8)}.md` } : i) })
      setLocalError('已为新资料分配独立名称，请核对后重新生成服务端预览。不会继承原文档或节点身份。')
      return
    }
    await preview(reviewedImport(draft.request, review, decisions))
  }
  const accepted = (result: ImportResult) => {
    if (result.summary.operation_id !== draft.request?.operation_id || result.summary.collection_id !== collection.id || result.summary.space_id !== spaceId || result.summary.actor_device_id !== session.device.id) throw new ApiError(502, 'invalid_response')
    save({ ...draft, result, unknown: false }); if (live.current) onImported(result)
  }
  const confirm = async () => {
    if (!draft.request || !draft.preview?.receipt) return
    // 在发请求前记录未知状态；丢响应后保留原请求及 operation。
    save({ ...draft, unknown: true })
    try { accepted(await api.confirm(collection.id, draft.request, draft.preview.receipt)) }
    catch (e) { if (e instanceof ApiError && e.status >= 400 && e.status < 500) save({ ...draft, unknown: false }); throw e }
  }
  const reconcile = async () => {
    if (!draft.request) return
    try { accepted(await api.operation(collection.id, draft.request.operation_id)) }
    catch (e) {
      if (e instanceof ApiError && e.status === 404) { save({ ...draft, unknown: false }); setLocalError('原操作尚未提交。可用原确认重试；修改内容会创建新操作。'); return }
      throw e
    }
  }
  const ready = draft.items.filter(i => i.selected && i.status === 'ready').length
  const review = draft.preview?.identity_review
  const addFiles = (files: File[]) => {
    if (busy || draft.unknown || draft.result) return
    void run(async () => {
      const items = await scanFiles(files, pdfConsent ? data => api.pdf(data).catch(error => { throw new Error(errorText(error)) }) : undefined)
      if (live.current) edit({ items: [...draft.items, ...items] })
    })
  }
  return <section className={`panel reference-import${method ? ' notebook-import' : ''}`} aria-label="单批资料导入" aria-busy={busy}>
    {method ? <h3 className="source-import-title"><FileText size={16} aria-hidden="true" />导入到「{collection.name}」</h3> : <h2>单批导入到「{collection.name}」</h2>}
    <p className="hint">预览后确认保存；重名资料会逐项请你审阅。</p>
    {!!error && <ErrorState error={error} />}{localError && <p role="alert">{localError}</p>}
    <fieldset disabled={busy || draft.unknown || !!draft.result}>
      <legend>来源选择</legend>
      {(!method || method === 'pdf') && <>
      <label><input type="checkbox" checked={pdfConsent} onChange={e => setPDFConsent(e.target.checked)} />我有权上传并保存 PDF 原件及提取文本，用于参考和安全页查看</label>
      <label className={`source-dropzone${dragging ? ' dragging' : ''}`}
        onDragOver={e => { e.preventDefault(); if (!busy && !draft.unknown && !draft.result) setDragging(true) }}
        onDragLeave={() => setDragging(false)}
        onDrop={e => { e.preventDefault(); setDragging(false); addFiles(Array.from(e.dataTransfer.files)) }}>
        <Upload size={30} aria-hidden="true" />
        <strong>拖到这里或选择文件</strong>
        <span>支持 PDF、Markdown 和 UTF-8 文本，可多选</span>
        <input aria-label="选择 Markdown / UTF-8 / PDF 文件（可多选）" type="file" multiple accept=".pdf,.md,.markdown,.txt,text/plain,text/markdown,application/pdf" onChange={e => { const files = Array.from(e.target.files ?? []); e.target.value = ''; addFiles(files) }} />
      </label>
      <p className="hint">PDF 需先勾选保存授权，再选择文件：单份最多 4 MiB、100 页、16000 字节文本；扫描件不做 OCR。Office 暂不支持。不上传资料仍可学习。</p>
      </>}
      {(!method || method === 'text') && <>
      <label>粘贴资料名称<input placeholder="给这份资料起个名字" value={draft.pasteName} maxLength={120} onChange={e => edit({ pasteName: e.target.value })} /></label>
      <label>粘贴原文<textarea placeholder="在这里粘贴要学习的原文…" rows={5} value={draft.paste} onChange={e => edit({ paste: e.target.value })} /></label>
      <Button variant="outline" onClick={() => edit({ items: [...draft.items, pastedItem(draft.pasteName, draft.paste)], paste: '' })}>加入粘贴文本</Button>
      </>}
      {(!method || method === 'url') && <>
      <label>公开网页链接<input type="url" placeholder="https://example.org/article" value={draft.url} onChange={e => edit({ url: e.target.value })} /></label>
      <p className="hint">点击下方授权按钮会把此公开地址发送到目标网站，并读取正文供你审阅保存。</p>
      <Button variant="outline" disabled={!draft.url} onClick={() => void run(async () => {
        const source = await api.source(draft.url)
        if (live.current) edit({ items: [...draft.items, fetchedItem(source)], url: '' })
      })}>授权服务器读取此网页并解析</Button>
      </>}

    </fieldset>
    <p role="status" className="source-import-status">{(busy) && <span className="source-badge">解析处理中…</span>} 待导入 {ready} 项 · 错误 {draft.items.filter(i => i.status === 'error').length} · 不支持 {draft.items.filter(i => i.status === 'unsupported').length} · 被排除 {draft.items.filter(i => i.status === 'ready' && !i.selected).length}</p>
    {!!draft.items.length && <label>搜索清单、正文和差异<input type="search" value={search} onChange={e => setSearch(e.target.value)} /></label>}
    <div className="reference-scroll" tabIndex={0} aria-label="解析报告">
      {draft.items.filter(i => `${i.name} ${i.path} ${i.markdown}`.includes(search)).map(item => <article className="panel compact" key={item.id}>
        <label><input type="checkbox" checked={item.selected} disabled={busy || draft.unknown || !!draft.result || item.status !== 'ready' || !!item.pdf && item.pdf.report.coverage !== 'complete_text' && !item.pdf.acceptPartial} onChange={e => edit({ items: draft.items.map(i => i.id === item.id ? { ...i, selected: e.target.checked } : i) })} />{item.name}</label>
        <span className={`source-badge ${item.status !== 'ready' ? 'failed' : item.pdf && item.pdf.report.coverage !== 'complete_text' ? 'partial' : 'ready'}`}>{item.status === 'error' ? '解析失败' : item.status === 'unsupported' ? '不支持' : item.pdf && item.pdf.report.coverage !== 'complete_text' ? '部分解析' : '可预览'}</span>
        <p>{itemDescription(item)}</p>
        {item.pdf && <><PDFCoverage report={item.pdf.report} />{item.status === 'ready' && item.pdf.report.coverage !== 'complete_text' && <label><input type="checkbox" checked={item.pdf.acceptPartial} disabled={busy || draft.unknown || !!draft.result} onChange={e => edit({ items: draft.items.map(i => i.id === item.id && i.pdf ? { ...i, selected: e.target.checked, pdf: { ...i.pdf, acceptPartial: e.target.checked } } : i) })} />我已核对逐页缺口，仅纳入「{item.name}」的可用文本；未解析页不算已验证</label>}</>}
        {item.status === 'ready' && <><label>导入相对名称<input value={item.path} disabled={busy || draft.unknown || !!draft.result} onChange={e => edit({ items: draft.items.map(i => i.id === item.id ? { ...i, path: e.target.value } : i) })} /></label><label><input type="checkbox" checked={!!item.asNew} disabled={busy || draft.unknown || !!draft.result} onChange={e => edit({ items: draft.items.map(i => i.id === item.id ? { ...i, asNew: e.target.checked, path: e.target.checked ? i.path.replace(/\.md$/i, '') + `-副本-${i.id.slice(0, 8)}.md` : i.path } : i) })} />作为全新资料（不继承原身份，使用独立名称）</label><details><summary>查看本次原文</summary><pre className="reference-text">{item.markdown}</pre></details></>}
      </article>)}
    </div>
    {!draft.result && !draft.unknown && <div className="source-import-actions"><Button disabled={busy || !ready} onClick={() => void run(() => preview())}>生成服务端预览</Button></div>}
    {review && !draft.result && <section aria-label="身份审阅">
      <h3>身份冲突：逐项决定</h3><p>更新保留原身份和历史。作为新资料需要未被占用的相对名称；可返回修改名称后重新预览。跳过会排除该文档。</p>
      <div className="reference-scroll" tabIndex={0}>
        {[...review.document_reviews.map(v => ({ ...v, node: false, title: v.path })), ...review.node_reviews.map(v => ({ ...v, node: true, title: v.candidates[0]?.evidence?.incoming_title ?? `章节 ${v.preorder + 1}` }))].filter(v => v.path.includes(search) || !search).map(v => <article className="panel compact identity-review-item" key={v.locator}>
          <p className="hint">{v.path} · {v.node ? '章节' : '文档'}</p>
          {v.node && <><h4>本次章节：{v.title}</h4><p>{v.candidates[0]?.evidence?.incoming_excerpt || '本章节没有可用正文摘录，可展开原文核对。'}</p>
            {v.candidates.map((c, i) => <div className="reference-context" key={c.revision_id}><h4>旧章节：{c.evidence?.title ?? `候选 ${i + 1}`}</h4><p>{c.evidence?.excerpt || '此候选没有可用正文摘录，可展开原版本核对。'}</p></div>)}
          </>}
          <label>{v.node ? `章节身份决定：${v.title}` : `文档身份决定：${v.path}`}
            <select value={decisions[v.locator] ?? ''} disabled={busy || draft.unknown} onChange={e => setDecisions({ ...decisions, [v.locator]: e.target.value })}>
              <option value="">请选择身份处理</option>{v.candidates.map((c, i) => <option key={c.revision_id} value={v.node ? c.revision_id : c.stable_id}>{v.node ? `更新原章节：${c.evidence?.title ?? `候选 ${i + 1}`}` : `更新原资料候选 ${i + 1}`}</option>)}<option value="new">作为新{v.node ? '章节' : '资料'}</option><option value="skip">本次跳过此文档</option>
            </select>
          </label>
        </article>)}
        {draft.preview?.before?.map((d, i) => <details key={i}><summary>原版本：{d.path}</summary><pre className="reference-text">{d.markdown}</pre></details>)}
      </div>
      <Button disabled={busy || draft.unknown} onClick={() => void run(reviewed)}>按身份决定重新预览</Button>
    </section>}
    {draft.preview?.status === 'ready' && !draft.result && <section aria-label="导入变更预览">
      <h3>尚未正式导入</h3><p>新增 {draft.preview.summary.added} · 更新 {draft.preview.summary.updated} · 未变化 {draft.preview.summary.unchanged}</p>
      <p>{draft.preview.impact_known ? `关联证据 ${draft.preview.affected_evidence} 条，原引用不改写。` : '关联证据影响尚不可用。'}</p>
      <div className="reference-scroll" tabIndex={0}>{draft.preview.diff.filter(d => `${d.after_path} ${d.before_path} ${d.unified_diff}`.includes(search)).map(d => <details key={d.document_id}><summary>{d.after_path || d.before_path} · {d.kind}</summary><pre className="reference-text">{d.unified_diff || '无正文差异'}</pre>{d.truncated && <p>差异达到展示预算，请对照原文与当前资料详情。</p>}</details>)}</div>
      <div className="source-import-actions">
      <Button variant="outline" disabled={busy || draft.unknown} onClick={() => edit({})}>返回修改（作废旧批准）</Button>
      <Button disabled={busy || draft.unknown} onClick={() => void run(confirm)}>确认正式导入这 {ready} 项资料</Button>
      </div>
    </section>}
    <SubmissionNotice pending={draft.unknown} inFlight={busy}>提交结果未知。先核对原操作，当前草稿与操作编号已保留。</SubmissionNotice>
    {draft.request && <Button variant="outline" disabled={busy} onClick={() => void run(reconcile)}>核对原操作结果</Button>}
    {draft.result && <div role="status"><h3>{method ? '资料已保存' : '资料已导入，尚未自动用于目标'}</h3><p>新增 {draft.result.summary.added} · 更新 {draft.result.summary.updated} · 未变化 {draft.result.summary.unchanged}。失败与排除项未计为成功。</p><Button variant="outline" onClick={() => edit({ items: [], result: undefined })}>开始另一批（保留原操作关联）</Button></div>}
  </section>
}
