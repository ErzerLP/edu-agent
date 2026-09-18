import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ApiError, learningClient, unwrap } from './api/client'
import { goalSchema, pageOf, spaceSchema } from './api/runtime'
import { knowledgeAPI, referencePreview, referenceState, roleNames, type Collection, type ImportResult, type ReferenceEntry, type ReferencePreview, type ReferenceRequest } from './api/references'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { Confirm, ErrorState } from './components/common'
import { ReferenceImport } from './components/reference-import'
import { PDFCoverage, PDFPageViewer } from './components/pdf-source'

type AdoptionDraft = { entries: ReferenceEntry[]; request?: ReferenceRequest; preview?: ReferencePreview; unknown?: boolean; done?: boolean }
const entryKey = (e: ReferenceEntry) => JSON.stringify([e.collection_id, e.revision_id, e.document_id, e.node_id])
function download(name: string, markdown: string) {
  const url = URL.createObjectURL(new Blob([markdown], { type: 'text/markdown;charset=utf-8' }))
  const a = document.createElement('a'); a.href = url; a.download = name.split('/').at(-1) || '资料.md'; a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
export function ReferenceLink({ spaceId, goalId, sessionId, status = false }: { spaceId: string; goalId: string; sessionId?: string; status?: boolean }) {
  const { session, prefix } = useIdentity()
  const policy = useQuery({ queryKey: [...prefix, spaceId, goalId, sessionId, 'references'], enabled: status && session.capabilities.references, gcTime: 0, queryFn: () => unwrap(learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}/references', { params: { path: { goalID: goalId }, query: { session_id: sessionId } } }), referenceState) })
  return <div className="reference-context"><Link to="/spaces/$spaceId/knowledge" params={{ spaceId }} search={{ goal: goalId, session: sessionId }}>补充参考（可选）</Link>{status && policy.data && <p className="hint">{policy.data.selection.session_id ? '本次会话' : '目标'}参考政策 v{policy.data.version} · {policy.data.selection.entries.length ? Object.entries(roleNames).map(([role, name]) => `${name} ${policy.data!.selection.entries.filter(e => e.role === role).length}`).join(' / ') : '未补充参考，仍可正常学习'} · 新选择须采用后才用于后续准备</p>}{policy.error && <ErrorState error={policy.error} />}</div>
}

export function KnowledgePage({ spaceId, goalId, sessionId }: { spaceId: string; goalId?: string; sessionId?: string }) {
  const { session, prefix, drafts } = useIdentity()
  const api = knowledgeAPI(session, spaceId)
  const key = JSON.stringify([...prefix, spaceId, goalId, sessionId, 'reference-adoption'])
  const [draft, setDraft] = useState<AdoptionDraft>(() => drafts.get<AdoptionDraft>(key) ?? { entries: [] })
  const [collectionId, setCollectionId] = useState(''), [revision, setRevision] = useState(''), [frozen, setFrozen] = useState('')
  const [role, setRole] = useState<ReferenceEntry['role']>('supplement')
  const [name, setName] = useState(''), [source, setSource] = useState('浏览器授权资料'), [search, setSearch] = useState('')
  const [pending, setPending] = useState(false), [error, setError] = useState<unknown>(), [confirmScope, setConfirmScope] = useState(false), [notice, setNotice] = useState('')
  const live = useRef(true)
  useEffect(() => { live.current = true; return () => { live.current = false } }, [])
  const save = (v: AdoptionDraft) => { if (live.current) { setDraft(v); drafts.set(key, v) } }
  const canManage = session.capabilities.references
  const collections = useQuery({ queryKey: [...prefix, spaceId, 'collections'], queryFn: () => api.collections() })
  const discovery = useQuery({ queryKey: [...prefix, spaceId, 'collection-discovery'], queryFn: () => api.collections(true), enabled: canManage })
  const space = useQuery({ queryKey: [...prefix, spaceId, 'space'], queryFn: () => unwrap(learningClient(session).GET('/v1/learning-spaces/{spaceID}', { params: { path: { spaceID: spaceId } } }), spaceSchema) })
  const goal = useQuery({ queryKey: [...prefix, spaceId, goalId, 'latest'], queryFn: () => unwrap(learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}', { params: { path: { goalID: goalId! } } }), goalSchema), enabled: !!goalId })
  const goals = useQuery({ queryKey: [...prefix, spaceId, 'reference-goals'], queryFn: () => unwrap(learningClient(session, spaceId).GET('/v1/learning/goals', { params: { query: { limit: 100 } } }), pageOf(goalSchema)), enabled: !goalId })
  const current = useQuery({ queryKey: [...prefix, spaceId, goalId, sessionId, 'references'], queryFn: () => unwrap(learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}/references', { params: { path: { goalID: goalId! }, query: { session_id: sessionId } } }), referenceState), enabled: !!goalId && canManage, gcTime: 0 })
  useEffect(() => { if (current.data && !drafts.get(key)) setDraft({ entries: current.data.selection.entries }) }, [current.data, key])
  const collection = collections.data?.items.find(c => c.id === collectionId)
  const revisionId = revision || collection?.head_revision_id || ''
  const tree = useQuery({ queryKey: [...prefix, spaceId, collectionId, revisionId, 'reference-tree'], queryFn: () => api.tree(collectionId, revisionId), enabled: !!collection && !!revisionId && !frozen, gcTime: 0 })
  const body = useQuery({ queryKey: [...prefix, spaceId, collectionId, revisionId, frozen, 'reference-body'], queryFn: () => frozen ? api.scopeExport(frozen) : api.export(collectionId, revisionId), enabled: !!frozen || !!collection && !!revisionId, gcTime: 0 })
  const frozenInfo = useQuery({ queryKey: [...prefix, spaceId, frozen, 'scope'], queryFn: () => api.scope(frozen), enabled: !!frozen, gcTime: 0 })
  const inactive = space.data?.status !== 'active' || !!goal.data && !['active', 'draft'].includes(goal.data.management.status)
  const busy = pending || draft.unknown === true
  const run = async (fn: () => Promise<void>) => { if (pending) return; setPending(true); setError(undefined); setNotice(''); try { await fn() } catch (e) { if (live.current) setError(e) } finally { if (live.current) setPending(false) } }
  const refresh = async () => { if (live.current) { await collections.refetch(); if (canManage) await discovery.refetch() } }
  const select = (c: Collection, rev = '') => { setCollectionId(c.id); setRevision(rev); setFrozen('') }
  const changeEntries = (entries: ReferenceEntry[]) => { if (!busy) { save({ entries }); setConfirmScope(false) } }
  const add = (entry: Omit<ReferenceEntry, 'role'>) => { const e = { ...entry, role }; changeEntries([...draft.entries.filter(x => entryKey(x) !== entryKey(e)), e]) }
  const imported = (result: ImportResult) => {
    setRevision(result.revision.revision_id); void refresh()
    if (goalId && !draft.unknown) {
      const selected = result.summary.document_ids.map(document_id => ({ collection_id: result.summary.collection_id, revision_id: result.revision.revision_id, document_id, role }))
      changeEntries([...draft.entries.filter(e => !selected.some(v => v.collection_id === e.collection_id && v.document_id === e.document_id)), ...selected])
    }
  }
  const previewAdoption = async () => {
    if (!goal.data) return
    const request: ReferenceRequest = { operation_id: crypto.randomUUID(), expected_goal_version: goal.data.revision, selection: { session_id: sessionId ?? '', entries: draft.entries } }
    save({ entries: draft.entries, request })
    const value = await unwrap(learningClient(session, spaceId).POST('/v1/learning/goals/{goalID}/references/previews', { params: { path: { goalID: goalId! } }, body: request }), referencePreview)
    save({ entries: draft.entries, request, preview: value }); if (live.current) setConfirmScope(false)
  }
  const adopt = async () => {
    if (!draft.request || !draft.preview || !goalId) return
    save({ ...draft, unknown: true })
    try {
      await unwrap(learningClient(session, spaceId).POST('/v1/learning/goals/{goalID}/references/confirm', { params: { path: { goalID: goalId } }, body: { request: draft.request, receipt: draft.preview.receipt, confirm_scope: confirmScope } }), referenceState)
      save({ ...draft, unknown: false, done: true }); if (live.current) await current.refetch()
    } catch (e) { if (e instanceof ApiError && e.status >= 400 && e.status < 500) save({ ...draft, unknown: false }); throw e }
  }
  const reconcile = async () => {
    if (!draft.request || !goalId) return
    try {
      await unwrap(learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}/references/operations/{operationID}', { params: { path: { goalID: goalId, operationID: draft.request.operation_id } } }), referenceState)
      save({ ...draft, unknown: false, done: true }); if (live.current) await current.refetch()
    } catch (e) { if (e instanceof ApiError && e.status === 404) { save({ ...draft, unknown: false }); setNotice('原采用操作尚未提交，可重试原确认。'); return }; throw e }
  }
  return <>
    <section className="intro reference-context"><span className="eyebrow">参考始终可选</span><h1>知识与参考资料</h1><p>当前学习区：{space.data?.name ?? '读取中'} · {goal.data ? `目标：${goal.data.management.details.name} · ${sessionId ? '仅本次教学会话' : '目标后续学习'}` : '独立资料管理，尚未选择目标'}</p>
      {goalId && <div className="actions"><Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId }}>返回目标</Link>{sessionId && <><Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId, sessionId }}>返回原课堂</Link><ReferenceLink spaceId={spaceId} goalId={goalId} /></>}</div>}
      {!canManage && <p>当前身份可浏览已有资料。导入、共享和正式采用需要管理员明确创建“参考管理”配对码后重新配对。</p>}
    </section>
    {!!(error || collections.error || discovery.error || space.error || goal.error || current.error || tree.error || body.error || frozenInfo.error) && <ErrorState error={error || collections.error || discovery.error || space.error || goal.error || current.error || tree.error || body.error || frozenInfo.error} />}
    {notice && <p role="status">{notice}</p>}
    {!goalId && <section className="panel"><h2>选择参考的使用目标（可选）</h2>{goals.data?.items.map(g => <p key={g.goal_id}><ReferenceLink spaceId={spaceId} goalId={g.goal_id} /> · {g.management.details.name}</p>)}{goals.data?.next_cursor && <p>这里只显示前 100 个目标；其余目标可从学习区搜索后进入“补充参考”。</p>}</section>}
    {canManage && <section className="panel"><h2>创建资料集合</h2><form onSubmit={e => { e.preventDefault(); void run(async () => { const c = await api.collection({ id: drafts.operation(`${key}:create`, { name, source }), action: 'create', name, source }); if (live.current) { select(c); setName('') }; await refresh() }) }}><fieldset disabled={pending || inactive}><label>集合名称<input required value={name} maxLength={120} onChange={e => setName(e.target.value)} /></label><label>来源说明<input required value={source} maxLength={500} onChange={e => setSource(e.target.value)} /></label><Button type="submit">创建私有集合</Button></fieldset></form></section>}
    <section className="panel"><h2>当前区的资料集合</h2><div className="reference-scroll" tabIndex={0}>{collections.data?.items.map(c => <article className="panel compact" key={c.id}><h3>{c.name}</h3><p>{c.source} · {c.shared ? '已共享' : '私有'} · {c.head_revision_id ? '已有正式版本' : '尚无资料'}</p><Button variant="outline" onClick={() => select(c)}>浏览与导入「{c.name}」</Button>{canManage && <><Confirm disabled={pending || inactive} label={c.shared ? '关闭共享' : '开启共享'} title={`确认${c.shared ? '关闭' : '开启'}「${c.name}」共享？`} onConfirm={() => void run(async () => { await api.collection({ id: c.id, action: 'share', expected_version: c.version, shared: !c.shared }); await refresh() })}>开启后其他区可发现并关联；关闭不撤销已有引用。</Confirm><Confirm disabled={pending || inactive} label="解除本区引用" title={`解除「${c.name}」的本区引用？`} onConfirm={() => void run(async () => { await api.collection({ id: c.id, action: 'unlink' }); if (live.current && c.id === collectionId) { setCollectionId(''); setRevision('') }; await refresh() })}>不会删除资料、其他区引用或历史冻结范围。</Confirm></>}</article>)}</div></section>
    {canManage && <section className="panel"><h2>发现可关联集合</h2>{discovery.data?.items.filter(c => !collections.data?.items.some(v => v.id === c.id)).map(c => <p key={c.id}>{c.name} · {c.source}<Button variant="outline" disabled={pending || inactive} onClick={() => void run(async () => { await api.collection({ id: c.id, action: 'link' }); await refresh() })}>关联「{c.name}」到本区</Button></p>)}</section>}
    {collection && canManage && <CollectionEditor key={`${collection.id}:${collection.version}`} collection={collection} disabled={pending || inactive} onSave={(name, source) => void run(async () => { await api.collection({ id: collection.id, action: 'edit', expected_version: collection.version, name, source }); await refresh() })} />}
    {collection && canManage && !inactive && <ReferenceImport key={`${collection.id}:${goalId}:${sessionId}`} spaceId={spaceId} collection={collection} target={`${goalId ?? ''}:${sessionId ?? ''}`} onImported={imported} />}
    {(collection || frozen) && <section className="panel" aria-label="资料内容与范围选择"><h2>{frozen ? '已采用的冻结范围' : `${collection?.name} · ${revision ? '指定历史版本' : '最新版本'}`}</h2>
      {tree.data && <p>版本 {tree.data.revision.revision_no}<Button variant="outline" disabled={!tree.data.revision.parent_revision_id} onClick={() => setRevision(tree.data!.revision.parent_revision_id!)}>浏览前一版本</Button><Button variant="outline" onClick={() => { setRevision(''); setFrozen('') }}>回到最新版本</Button></p>}
      {frozenInfo.data && <p>固定 {frozenInfo.data.entries.length} 个范围 · {frozenInfo.data.updates?.length ?? 0} 个集合有新版本，未自动更新。</p>}
      <label>搜索文档、章节和正文<input type="search" value={search} onChange={e => setSearch(e.target.value)} /></label>
      {goalId && canManage && <label>新选择的角色<select disabled={busy} value={role} onChange={e => setRole(e.target.value as ReferenceEntry['role'])}>{Object.entries(roleNames).map(([v, label]) => <option key={v} value={v}>{label}</option>)}</select></label>}
      {goalId && collection && revisionId && !frozen && <Button disabled={busy || inactive} onClick={() => add({ collection_id: collection.id, revision_id: revisionId })}>选择此集合版本</Button>}
      <div className="reference-scroll" tabIndex={0}>{tree.data?.revision.documents.filter(d => `${d.path} ${d.document.nodes.map(n => n.title).join(' ')}`.includes(search)).map(d => <article className="panel compact" key={d.document.document_id}><h3>{d.path}</h3>{goalId && <><Button variant="outline" disabled={busy || inactive || !canManage} onClick={() => add({ collection_id: collectionId, revision_id: revisionId, document_id: d.document.document_id })}>选择整篇「{d.path}」</Button><ul>{d.document.nodes.filter(n => n.heading_level > 0).map(n => <li key={n.node_id}>{n.title}<Button variant="ghost" disabled={busy || inactive || !canManage} onClick={() => add({ collection_id: collectionId, revision_id: revisionId, document_id: d.document.document_id, node_id: n.node_id })}>选择章节「{n.title}」</Button></li>)}</ul></>}</article>)}</div>
      {tree.data?.revision.documents.filter(d => d.document.pdf).map(d => <section key={d.document.document_revision_id} aria-label={`PDF 来源 ${d.path}`}><h3>{d.path} · 历史 PDF</h3><PDFCoverage report={d.document.pdf!.report} /><PDFPageViewer spaceId={spaceId} revisionId={frozen || revisionId} documentId={d.document.document_revision_id} collectionId={frozen ? undefined : collectionId} pages={d.document.pdf!.report.pages.filter(p => !frozen || d.document.pdf!.ranges.some(r => r.number === p.number && d.document.nodes.some(n => n.section_range.start <= r.range.start && n.section_range.end >= r.range.end))).map(p => p.number)} /></section>)}
      <div className="reference-scroll" tabIndex={0}>{body.data?.documents.filter(d => `${d.path} ${d.markdown}`.includes(search)).map((d, i) => <details key={`${d.path}:${i}`}><summary>{d.path}</summary><pre className="reference-text">{d.markdown}</pre><Button variant="outline" onClick={() => download(d.path, d.markdown)}>导出「{d.path}」Markdown</Button></details>)}</div>
    </section>}
    {goalId && canManage && <section className="panel" aria-label="参考采用与角色"><h2>用于{sessionId ? '本次会话' : '此目标'}的参考</h2><p>补充参考允许与已有来源一起使用；优先依据先进入检索候选；存在限制范围时，仅使用所选限制条目。不会影响其他目标。</p>
      {current.data && <p>已采用政策版本 {current.data.version}{current.data.scope_snapshot_id && <Button variant="outline" onClick={() => { setFrozen(current.data!.scope_snapshot_id); setCollectionId('') }}>查看旧冻结范围与更新提示</Button>}</p>}
      <p role="status">{draft.done ? '已采用，供后续准备使用；当前题目保留原引用。' : `待采用 ${draft.entries.length} 项`}</p>
      {draft.done && !sessionId && draft.entries.length > 0 && <Link to="/spaces/$spaceId/goals/$goalId/research" params={{ spaceId, goalId }} search={{ start: true }}>使用已采用参考准备新课堂（需要另行授权）</Link>}
      <div className="reference-scroll" tabIndex={0}>{draft.entries.map((e, i) => <article className="panel compact" key={entryKey(e)}><p>{collections.data?.items.find(c => c.id === e.collection_id)?.name ?? '历史引用集合'} · {e.node_id ? '所选章节' : e.document_id ? '所选文档' : '整个集合'} · 固定版本 {e.revision_id.slice(0, 8)}</p><label>第 {i + 1} 项角色<select value={e.role} disabled={busy || inactive} onChange={event => changeEntries(draft.entries.map((v, j) => j === i ? { ...v, role: event.target.value as ReferenceEntry['role'] } : v))}>{Object.entries(roleNames).map(([v, text]) => <option key={v} value={v}>{text}</option>)}</select></label><Button variant="outline" disabled={busy || inactive} onClick={() => changeEntries(draft.entries.filter((_, j) => j !== i))}>移除此选择</Button><Button variant="ghost" onClick={() => { const c = collections.data?.items.find(c => c.id === e.collection_id); if (c) select(c, e.revision_id) }}>浏览所选版本</Button></article>)}</div>
      <Button disabled={busy || inactive || !goal.data} onClick={() => void run(previewAdoption)}>预览参考角色与范围变更</Button>
      {draft.preview && !draft.done && <div><h3>具体变更：{sessionId ? '仅本次会话' : '仅此目标'}</h3><ReferenceDiff spaceId={spaceId} before={draft.preview.before.selection.entries} after={draft.preview.after.entries} collections={collections.data?.items ?? []} /><p>{draft.preview.impact}</p>{draft.preview.requires_scope_confirmation && <label><input type="checkbox" checked={confirmScope} disabled={busy} onChange={e => setConfirmScope(e.target.checked)} />我确认上面列出的旧限制范围与新限制范围</label>}<Button disabled={busy || inactive || draft.preview.requires_scope_confirmation && !confirmScope} onClick={() => void run(adopt)}>{draft.preview.requires_scope_confirmation ? '确认此目标／会话的新限制范围' : '正式采用这些参考角色'}</Button><Button variant="outline" disabled={busy} onClick={() => changeEntries(draft.entries)}>返回修改（作废旧批准）</Button></div>}
      {draft.unknown && <p role="alert">采用结果未知，请先核对原操作。</p>}{draft.request && <Button disabled={pending} variant="outline" onClick={() => void run(reconcile)}>核对原采用操作</Button>}
    </section>}
  </>
}
function CollectionEditor({ collection, disabled, onSave }: { collection: Collection; disabled: boolean; onSave: (name: string, source: string) => void }) {
  const [name, setName] = useState(collection.name), [source, setSource] = useState(collection.source)
  return <details className="panel"><summary>编辑「{collection.name}」集合信息</summary><form onSubmit={e => { e.preventDefault(); onSave(name, source) }}><fieldset disabled={disabled}><label>修改集合名称<input required maxLength={120} value={name} onChange={e => setName(e.target.value)} /></label><label>修改来源说明<input required maxLength={500} value={source} onChange={e => setSource(e.target.value)} /></label><Button type="submit">保存集合信息</Button></fieldset></form></details>
}
function ReferenceDiff({ before, after, collections, spaceId }: { before: ReferenceEntry[]; after: ReferenceEntry[]; collections: Collection[]; spaceId: string }) {
  const item = (e: ReferenceEntry) => <ReferenceLabel entry={e} spaceId={spaceId} collection={collections.find(c => c.id === e.collection_id)?.name} />
  return <div className="reference-scroll" tabIndex={0}><h4>旧选择</h4><ul>{before.map(e => <li key={entryKey(e)}>{item(e)}</li>)}</ul>{!before.length && <p>无用户参考限制</p>}<h4>新选择</h4><ul>{after.map(e => <li key={entryKey(e)}>{item(e)}</li>)}</ul>{!after.length && <p>移除用户参考选择，恢复已有基础来源</p>}</div>
}
function ReferenceLabel({ entry, spaceId, collection }: { entry: ReferenceEntry; spaceId: string; collection?: string }) {
  const { session, prefix } = useIdentity()
  const tree = useQuery({ queryKey: [...prefix, spaceId, entry.collection_id, entry.revision_id, 'reference-tree'], queryFn: () => knowledgeAPI(session, spaceId).tree(entry.collection_id, entry.revision_id), gcTime: 0, retry: false })
  const doc = tree.data?.revision.documents.find(d => d.document.document_id === entry.document_id)
  const node = doc?.document.nodes.find(n => n.node_id === entry.node_id)
  return <span>{collection ?? '历史集合'} / {entry.document_id ? doc?.path ?? '正在读取文档名称或历史关联已解除' : '整个集合'} / {entry.node_id ? node?.title ?? '所选章节' : '全部章节'} / 固定版本 {tree.data?.revision.revision_no ?? entry.revision_id.slice(0, 8)} / {roleNames[entry.role]}</span>
}
