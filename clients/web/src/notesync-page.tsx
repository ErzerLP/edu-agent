import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ApiError } from './api/client'
import { notesyncAPI, notesyncCollection, notesyncSpace, syncCategories, syncStatusNames, syncError, syncAccessLost, type SyncReview, type SyncPreview, type SyncResolution, type SyncResult } from './api/notesync'
import { taskAdapters } from './api/tasks'
import type { ImportPreview, ImportCommand } from './api/import-jobs'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { Confirm, Pagination } from './components/common'
import { ImportDiff, ImportIdentityReview } from './components/import-review'

type Scope = { spaceId: string; collectionId: string }
function SyncError({ error }: { error: unknown }) {
  return error ? <p role="alert">{syncError(error)}</p> : null
}

// 先重新核对访问边界，再挂载正文；错误或失去引用时销毁子组件及内存中的差异。
function SyncBoundary({ spaceId, collectionId, children }: Scope & { children: ReactNode }) {
  const { session, prefix } = useIdentity()
  const mapped = spaceId === notesyncSpace && collectionId === notesyncCollection
  const status = useQuery({
    queryKey: [...prefix, spaceId, collectionId, 'notesync-status'],
    queryFn: ({ signal }) => notesyncAPI(session, spaceId, collectionId, signal).status(),
    enabled: mapped, retry: false, gcTime: 0, staleTime: 0, refetchInterval: 15000,
  })
  if (!mapped) return <section className="panel"><h2>当前来源不可同步</h2><p>只有默认学习区的默认集合有明确映射。AI 研究集合、非默认区和当前 UI 选择不会继承同步权限。</p></section>
  const value = status.data
  return <>
    <section className="panel" aria-label="同步状态">
      <h2>NoteSync 同步状态</h2>
      <p>固定学习区：{spaceId} · 固定集合：{collectionId}</p>
      <SyncError error={status.error} />
      {status.isPending && <p role="status">正在读取真实状态…</p>}
      {value && !status.error && <>
        <p>{value.configured ? value.compatible ? '连接可用' : '远端不可用或不兼容' : '尚未配置'} · {value.reason || '版本与能力检查通过'}</p>
        <p>配置来源：{value.configuration_source === 'admin_settings' ? '操作者保存的管理设置（服务启动时生效）' : value.configuration_source === 'environment' ? '服务端环境配置' : '尚无活动配置'} · 版本：{value.version ?? '不可用'}</p>
        <p>笔记库：{value.vault ?? '未配置'} · 路径前缀：{value.path_prefix ?? '未配置'}</p>
      </>}
      <p>连接设置由操作者通过服务器本机 <code>/admin/</code> 或 SSH 转发入口管理。学习端不读取 Key，也不获得管理权限。</p>
      <p>解除引用、归档、断开连接和本地隐私清除均不删除远端笔记、历史或备份；远端副本需要操作者单独清理。</p>
      <Button variant="outline" disabled={status.isFetching} onClick={() => void status.refetch()}>刷新同步状态</Button>
    </section>
    {value?.configured && !status.error && children}
  </>
}

export function NotesyncPage({ spaceId, collectionId = notesyncCollection }: { spaceId: string; collectionId?: string }) {
  return <><h1>来源同步</h1><Link to="/spaces/$spaceId/knowledge" params={{ spaceId }} search={{ goal: undefined, session: undefined }}>返回知识与参考资料</Link>
    <SyncBoundary spaceId={spaceId} collectionId={collectionId}>
      <SyncPreviewPanel spaceId={spaceId} collectionId={collectionId} />
      <SyncReviewList spaceId={spaceId} collectionId={collectionId} />
    </SyncBoundary></>
}

export function NotesyncTasks({ spaceId, collectionId = notesyncCollection }: { spaceId: string; collectionId?: string }) {
  return <section aria-label="NoteSync 同步任务"><SyncBoundary spaceId={spaceId} collectionId={collectionId}>
    <Link to="/spaces/$spaceId/notesync" params={{ spaceId }} search={{ collection: collectionId }}>打开同步预览</Link>
    <SyncReviewList spaceId={spaceId} collectionId={collectionId} />
  </SyncBoundary></section>
}

function ReviewLink({ spaceId, collectionId, reviewId, children }: Scope & { reviewId: string; children: ReactNode }) {
  return <Link to="/spaces/$spaceId/notesync/$reviewId" params={{ spaceId, reviewId }} search={{ collection: collectionId }}>{children}</Link>
}
function SyncReviewList({ spaceId, collectionId }: Scope) {
  const { session, prefix } = useIdentity()
  const [status, setStatus] = useState<'all' | 'open' | 'resolved' | 'closed'>('all')
  const [cursors, setCursors] = useState<(string | undefined)[]>([undefined])
  const query = useQuery({
    queryKey: [...prefix, spaceId, collectionId, 'notesync-reviews', status, cursors.at(-1)],
    queryFn: ({ signal }) => taskAdapters.notesync(session, spaceId, collectionId, status, cursors.at(-1), signal),
    retry: false, gcTime: 0,
  })
  return <section className="panel" aria-label="同步审阅列表"><h2>同步审阅</h2>
    <p>保留 NoteSync 原状态；“已解决”表示本地审阅已提交，不能据此认定远端发布完成。</p>
    <label>审阅状态<select value={status} onChange={e => { setStatus(e.target.value as typeof status); setCursors([undefined]) }}>
      <option value="all">全部状态</option>{Object.entries(syncStatusNames).map(([key, label]) => <option key={key} value={key}>{label}（{key}）</option>)}
    </select></label>
    <Button variant="outline" onClick={() => void query.refetch()}>刷新审阅列表</Button>
    <SyncError error={query.error} />
    {query.isPending && <p role="status">正在读取审阅…</p>}
    {!query.error && query.data && <>
      {query.data.items.length === 0 && <p>当前筛选没有审阅。尚未预览不代表已经同步。</p>}
      {query.data.items.map(item => <article className="panel compact" key={item.review_id}>
        <h3>{item.remote_path}</h3><p>{syncCategories[item.category]} · {syncStatusNames[item.status]}（{item.status}）</p>
        <p>文档：{item.document_id ?? item.remote_document_id ?? '等待身份审阅'} · 基线版本 {item.head_revision_no}</p>
        <ReviewLink spaceId={spaceId} collectionId={collectionId} reviewId={item.review_id}>查看同一同步审阅</ReviewLink>
      </article>)}
      <Pagination page={cursors.length} previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined} next={query.data.next_cursor ? () => setCursors([...cursors, query.data!.next_cursor]) : undefined} />
    </>}
  </section>
}

function useSyncAction(scope: Scope) {
  const { session } = useIdentity()
  const [busy, setBusy] = useState(false), [error, setError] = useState<unknown>()
  const controller = useRef<AbortController | null>(null)
  useEffect(() => () => controller.current?.abort(), [])
  const run = async (fn: (api: ReturnType<typeof notesyncAPI>, signal: AbortSignal) => Promise<void>) => {
    if (controller.current && !controller.current.signal.aborted) return
    const current = new AbortController(); controller.current = current
    setBusy(true); setError(undefined)
    try { await fn(notesyncAPI(session, scope.spaceId, scope.collectionId, current.signal), current.signal) }
    catch (e) { if (!current.signal.aborted) setError(e) }
    finally { if (!current.signal.aborted) { setBusy(false); controller.current = null } }
  }
  return { busy, error, run }
}

function PreviewItems({ preview, spaceId, collectionId }: Scope & { preview: SyncPreview }) {
  const partial = preview.total_rows > preview.page * preview.page_size || preview.page > 1
  return <section aria-label="同步预览结果">
    <p>第 {preview.page} 页 · 本页 {preview.items.length} 项 · 远端列表共 {preview.total_rows} 项（可能包含前缀外笔记）。预览不会发布。</p>
    {partial && <p role="status">这是部分结果，不代表全库同步完成。{!preview.next_page && preview.total_rows > preview.page * preview.page_size && '已达扫描范围限制，可指定路径单独核对。'}</p>}
    {preview.items.length === 0 && <p>本页没有前缀范围内的候选；这不是全库同步成功。</p>}
    {preview.items.map(item => <article className="panel compact" key={item.remote_path}>
      <h3>{item.remote_path}</h3><p>{syncCategories[item.category]}</p>
      {item.review_id ? <ReviewLink spaceId={spaceId} collectionId={collectionId} reviewId={item.review_id}>打开预览审阅</ReviewLink> : <p>无需冲突解决；本地发布仍由原同步队列处理。</p>}
      <details><summary>本页三方差异</summary><pre>{item.diff.base_to_local}</pre><pre>{item.diff.base_to_remote}</pre>
        {(item.diff.local_truncated || item.diff.remote_truncated) && <p>差异已裁剪，请查看审阅完整正文。</p>}
      </details>
    </article>)}
  </section>
}
function SyncPreviewPanel(scope: Scope) {
  const [path, setPath] = useState(''), [preview, setPreview] = useState<SyncPreview>(), [previewPath, setPreviewPath] = useState('')
  const action = useSyncAction(scope)
  const start = (page = 1, selected = path) => action.run(async (api, signal) => {
    setPreview(undefined)
    const value = await api.preview(selected, page)
    if (!signal.aborted) { setPreview(value); setPreviewPath(selected) }
  })
  if (syncAccessLost(action.error)) return <SyncError error={action.error} />
  return <section className="panel"><h2>只读预览</h2><p>扫描固定笔记库的管理前缀，保存审阅差异；不导入、不发布。</p>
    <label>远端路径（留空分页扫描）<input value={path} maxLength={1024} disabled={action.busy} onChange={e => setPath(e.target.value)} /></label>
    <Button disabled={action.busy} onClick={() => void start()}>生成同步预览</Button><SyncError error={action.error} />
    {preview && <><PreviewItems {...scope} preview={preview} />{preview.next_page && <Button disabled={action.busy} onClick={() => void start(preview.next_page, previewPath)}>预览下一页</Button>}</>}
  </section>
}

export function NotesyncReviewPage({ spaceId, collectionId = notesyncCollection, reviewId, operationId }: Scope & { reviewId: string; operationId?: string }) {
  return <><h1>同步审阅</h1><Link to="/spaces/$spaceId/notesync" params={{ spaceId }} search={{ collection: collectionId }}>返回来源同步</Link>
    <SyncBoundary spaceId={spaceId} collectionId={collectionId}>
      <SyncReviewDetail spaceId={spaceId} collectionId={collectionId} reviewId={reviewId} operationId={operationId} />
    </SyncBoundary></>
}
function SyncReviewDetail({ spaceId, collectionId, reviewId, operationId }: Scope & { reviewId: string; operationId?: string }) {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const action = useSyncAction({ spaceId, collectionId })
  const [kind, setKind] = useState<SyncResolution['kind']>('accept_remote'), [merged, setMerged] = useState('')
  const [plan, setPlan] = useState<ImportPreview>(), [command, setCommand] = useState<SyncResolution>()
  const [result, setResult] = useState<SyncResult>(), [fresh, setFresh] = useState<SyncPreview>(), [notice, setNotice] = useState('')
  const [unknown, setUnknown] = useState(!!operationId), [stale, setStale] = useState(false)
  const query = useQuery({ queryKey: [...prefix, spaceId, collectionId, 'notesync-review', reviewId], queryFn: ({ signal }) => notesyncAPI(session, spaceId, collectionId, signal).review(reviewId), retry: false, gcTime: 0 })
  const review = query.data
  const operationURL = (operation?: string) => navigate({ to: '/spaces/$spaceId/notesync/$reviewId', params: { spaceId, reviewId }, search: { collection: collectionId, operation }, replace: true })
  const reconcile = () => action.run(async (api, signal) => {
    const id = operationId ?? command?.operation_id
    if (!id) return
    const receipt = await api.operation(id)
    if (receipt.review_id !== reviewId) throw new ApiError(409, 'idempotency_conflict')
    if (!signal.aborted) { setResult(receipt); setUnknown(false); await query.refetch() }
  })
  useEffect(() => { if (operationId) void reconcile() }, [])
  useEffect(() => {
    if (action.error instanceof ApiError && ['stale_notesync_review', 'revision_conflict', 'stale_identity_review'].includes(action.error.code)) { setStale(true); setPlan(undefined) }
  }, [action.error])
  const reset = () => { setPlan(undefined); setCommand(undefined); setNotice('') }
  const prepare = (decisions?: Pick<ImportCommand, 'document_resolutions' | 'node_resolutions'>) => action.run(async (api, signal) => {
    if (!review) return
    const identity = plan?.identity_review
    const next: SyncResolution = { operation_id: command?.operation_id ?? crypto.randomUUID(), basis_hash: review.basis_hash, kind, ...(kind === 'merged' ? { merged_markdown: merged } : {}), ...(decisions && identity ? { ...decisions, identity_review_basis_hash: identity.identity_review_basis_hash, identity_review_operation_id: identity.identity_review_operation_id, identity_review_receipt: identity.identity_review_receipt } : {}) }
    const value = await api.plan(reviewId, next)
    if (!signal.aborted) { setCommand(next); setPlan(value) }
  })
  const resolve = () => action.run(async (api, signal) => {
    if (!review) return
    const request = kind === 'keep_canonical' ? { operation_id: crypto.randomUUID(), basis_hash: review.basis_hash, kind } : command
    if (!request) return
    setCommand(request); setUnknown(true); setNotice('')
    await operationURL(request.operation_id)
    try {
      const value = await api.resolve(reviewId, request)
      if (!signal.aborted) { setResult(value); setUnknown(false); setPlan(undefined); await query.refetch() }
    } catch (e) {
      if (!signal.aborted && e instanceof ApiError && e.status >= 400 && e.status < 500 && ![408, 429].includes(e.status)) {
        setUnknown(false); await operationURL()
      }
      throw e
    }
  })
  const repreview = () => action.run(async (api, signal) => {
    if (!review) return
    const value = await api.preview(review.remote_path)
    if (!signal.aborted) {
      setFresh(value)
      if (value.items.some(item => item.review_id === reviewId && item.basis_hash === review.basis_hash)) { setStale(false); setPlan(undefined); setCommand(undefined) }
      setNotice('已重新核对。下方仍保留原审阅差异，请进入新审阅后再次确认。')
    }
  })
  if (query.error || syncAccessLost(action.error)) return <SyncError error={query.error || action.error} />
  if (!review) return <p role="status">正在读取原审阅…</p>
  const closed = review.status !== 'open'
  const canWrite = session.device.scopes.includes('knowledge:write') && session.device.scopes.includes('knowledge:approve')
  const blocked = action.busy || unknown || stale || closed || !!result
  return <section className="panel" aria-label="同步审阅详情">
    <h2>{review.remote_path}</h2><p>{syncCategories[review.category]} · {syncStatusNames[review.status]}（{review.status}）</p>
    <p>审阅：{review.review_id} · 本地路径：{review.canonical_path} · 笔记库：{review.remote_vault}</p>
    <p>本地文档身份：{review.document_id ?? '尚无'} · 远端声明身份：{review.remote_document_id ?? '尚无'}</p>
    <p>知识基线：{review.head_revision_id || '初始版本'}（{review.head_revision_no}） · 原因：{review.reason_code}</p>
    {review.resolution_kind && <p>原解决动作：{review.resolution_kind} · 操作：{review.resolution_operation_id ?? '无'}</p>}
    <SyncBodies review={review} />
    <p>采用/合并只创建新的知识修订，旧课堂、作答与评估依据继续引用旧版本，不复制学习证据。</p>
    <Link to="/spaces/$spaceId/knowledge" params={{ spaceId }} search={{ goal: undefined, session: undefined }}>查看正式知识维护、参考资料及后续引用调整</Link>
    <SyncError error={action.error} />{notice && <p role="status">{notice}</p>}
    {unknown && <div role="alert"><p>解决结果未知：只核对原操作，禁止再次提交。原操作：{operationId ?? command?.operation_id}</p><Button disabled={action.busy} onClick={() => void reconcile()}>核对原操作结果</Button></div>}
    {result && <p role="status">原操作已核对：{result.resolution_kind} · 知识修订 {result.knowledge_revision_id ?? '本地未变化'}。远端发布仍需重新预览核对，不视为远端写入成功。</p>}
    {!canWrite && <p>当前身份可以查看和预览；解决需要操作者签发已有 import 或 references 配对权限。</p>}
    {!closed && <>
      <label>原合同解决动作<select disabled={blocked} value={kind} onChange={e => { reset(); setKind(e.target.value as typeof kind) }}>
        <option value="accept_remote" disabled={review.remote.missing || review.category === 'invalid_remote_markdown'}>采用远端</option>
        <option value="keep_canonical" disabled={review.local.missing}>保留本地（拒绝远端改动，排队回发）</option>
        <option value="merged">采用手工合并正文</option>
      </select></label>
      {kind === 'merged' && <label>合并 Markdown（保留文档、章节及来源版本标记）<textarea rows={12} disabled={blocked} value={merged} onChange={e => { reset(); setMerged(e.target.value) }} /></label>}
      {kind !== 'keep_canonical' && <Button disabled={blocked || kind === 'merged' && !merged.trim() || kind === 'accept_remote' && (review.remote.missing || review.category === 'invalid_remote_markdown')} onClick={() => void prepare()}>预览实际影响与身份</Button>}
      {plan && <><ImportDiff preview={plan} />{plan.status === 'review' && <ImportIdentityReview key={plan.identity_review?.identity_review_basis_hash} preview={plan} disabled={blocked} submit={decisions => void prepare(decisions)} />}</>}
      {canWrite && <Confirm label="确认正式解决" title="确认按所选原合同动作解决？" disabled={blocked || kind === 'keep_canonical' && review.local.missing || kind !== 'keep_canonical' && plan?.status !== 'ready'} onConfirm={() => void resolve()}>
        {kind === 'keep_canonical' ? '拒绝远端变化并保留本地，可能排队覆盖已核对的远端版本；这不是仅关闭审阅。' : '采用所核对的正文及身份决定，创建不可变知识修订，并由原队列处理发布。'} 版本变化会拒绝此动作。
      </Confirm>}
    </>}
    <Button variant="outline" disabled={action.busy || unknown} onClick={() => void repreview()}>重新预览当前路径</Button>
    {fresh && <PreviewItems spaceId={spaceId} collectionId={collectionId} preview={fresh} />}
  </section>
}

function SyncBodies({ review }: { review: SyncReview }) {
  const [tab, setTab] = useState<'base' | 'local' | 'remote'>('local')
  return <>
    <div className="sync-tabs" role="tablist" aria-label="审阅正文面板">
      {(['base', 'local', 'remote'] as const).map(key => <Button key={key} role="tab" aria-selected={tab === key} aria-controls={`sync-${key}`} onClick={() => setTab(key)}>{key === 'base' ? '发布基线' : key === 'local' ? '本地正文' : '远端正文'}</Button>)}
    </div>
    <div className="sync-bodies">{(['base', 'local', 'remote'] as const).map(key => {
      const value = review[key]
      return <section id={`sync-${key}`} className={`sync-body ${tab === key ? 'selected' : ''}`} key={key} aria-label={key === 'base' ? '发布基线' : key === 'local' ? '本地正文' : '远端正文'}>
        <h3>{key === 'base' ? '发布基线' : key === 'local' ? '本地正文' : '远端正文'}</h3>
        <p>{value.missing ? '此版本缺失' : '版本存在'} · {value.path ?? review.remote_path}</p>
        <p>文档修订 {value.document_revision_id ?? '无'} · 来源修订 {value.source_revision_id ?? '无'} · 远端版本 {value.remote_version}</p>
        <pre tabIndex={0}>{value.markdown || (value.missing ? '没有正文' : '正文为空')}</pre>
      </section>
    })}</div>
    <details open><summary>基线到本地 / 远端差异</summary>
      {(review.diff.local_truncated || review.diff.remote_truncated) && <p role="status">差异达到展示上限，以上正文可用于完整核对。</p>}
      <pre tabIndex={0}>{review.diff.base_to_local || '本地无差异'}</pre><pre tabIndex={0}>{review.diff.base_to_remote || '远端无差异'}</pre>
    </details>
  </>
}
