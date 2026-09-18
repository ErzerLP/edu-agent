import { useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useIdentity } from './lib/session'
import { memoryAPI, memoryScope, memoryAccessLost, candidateNames, recordNames, categoryNames, sourceNames, stepNames, type MemoryRecord, type MemoryOperation, type CandidateInput } from './api/memory'
import { Confirm, ErrorState, Pagination } from './components/common'
import { Button } from './components/ui/button'

function OperationStatus({ result }: { result?: MemoryOperation }) {
  if (!result) return null
  return <div role="status"><p>{result.candidate ? candidateNames[result.candidate.candidate.status] : '操作已接收'}{result.record ? `；${recordNames[result.record.status]}` : ''}。{result.replayed && '已核对原操作。'}</p>
    {result.record && <a href={`/app/memory?record=${result.record.logical_memory_id}`}>查看真实记录与交付回执</a>}
    {result.candidate?.candidate.status === 'pending_review' && <a href={`/app/memory?candidate=${result.candidate.candidate.candidate_id}`}>查看具体候选并审批</a>}
    {result.delivery?.public_status === 'queued' && <p>远端尚未确认完成，等待交付不等于已保存或已永久删除。</p>}
  </div>
}

export function CandidateReview({ candidateId }: { candidateId: string }) {
  const { session, prefix, drafts } = useIdentity()
  const cache = useQueryClient()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [result, setResult] = useState<MemoryOperation>()
  const detail = useQuery({ queryKey: [...prefix, 'memory', 'candidate', candidateId], queryFn: ({ signal }) => memoryAPI(session, signal).candidate(candidateId), gcTime: 0 })
  const target = detail.data?.candidate.logical_memory_id
  const base = useQuery({ queryKey: [...prefix, 'memory', 'record', target], queryFn: ({ signal }) => memoryAPI(session, signal).record(target!), enabled: !!target, gcTime: 0 })
  const value = memoryAccessLost(detail.error) || memoryAccessLost(error) ? undefined : detail.data
  const c = value?.candidate
  const canWrite = session.device.scopes.includes('memory:write') && session.device.scopes.includes('memory:web')
  const approve = !!c && value?.content_status === 'available' && c.status === 'pending_review' && c.stability === 'stable' && new Date(c.valid_until).getTime() > Date.now() && (!target || base.data?.content_status === 'available' && base.data.record.status === 'applied' && !base.error)
  const decide = async (decision: 'admit' | 'reject') => {
    if (!c || pending) return
    const body = { payload_schema_version: 1 as const, expected_revision: c.revision, decision, reason: decision === 'admit' ? '用户审阅具体内容后明确批准' : '用户明确拒绝此候选',
      ...(decision === 'admit' && target && base.data ? { expected_record_revision: base.data.record.revision, expected_record_generation: base.data.record.record_generation } : {}) }
    const operation_id = drafts.operation(`memory:decision:${candidateId}`, body)
    setPending(true); setError(undefined)
    try {
      setResult(await memoryAPI(session).decide(candidateId, { ...body, operation_id }))
      await cache.invalidateQueries({ queryKey: [...prefix, 'memory'] })
    } catch (e) { setError(e) }
    finally { setPending(false) }
  }
  return <section className="panel" aria-label="记忆候选详情">
    <h2>具体候选审阅</h2>
    {detail.isPending && <p>正在读取候选…</p>}
    {detail.error && <ErrorState error={detail.error} retry={() => void detail.refetch()} />}
    {!!error && <ErrorState error={error} />}
    {c && <>
      <p>{candidateNames[c.status]} · 候选版本 {c.revision}</p>
      <p className="memory-text">{value?.content_status === 'available' ? value.proposed_content : '候选正文已移除；已批准内容请查看正式记录。'}</p>
      <dl className="memory-metadata"><dt>理由</dt><dd>{c.reason}</dd><dt>类别</dt><dd>{categoryNames[c.category] ?? '受原准入政策约束的内容'}</dd>
        <dt>敏感性</dt><dd>{c.sensitivity === 'sensitive' ? '敏感信息，请谨慎确认' : '非敏感'}</dd><dt>稳定性</dt><dd>{c.stability === 'stable' ? '稳定长期信息' : '临时信息，不可批准为长期偏好'}</dd>
        <dt>实际作用范围</dt><dd>{memoryScope}</dd><dt>来源</dt><dd>{sourceNames[c.source_kind]} · {c.source_reference.model_id ?? '设备用户'}</dd>
        <dt>审阅/交付期限</dt><dd>{new Date(c.valid_until).toLocaleString('zh-CN')}；不是已保存记忆的自动删除日期。</dd><dt>政策</dt><dd>{c.admission_policy_version}</dd></dl>
      {target && <><p>{c.status === 'pending_review' ? '这是已有记忆的纠正候选。' : '此候选关联下列正式记录。'}</p><a href={`/app/memory?record=${target}`}>查看关联正式记录</a>
        {base.error && <ErrorState error={base.error} retry={() => void base.refetch()} />}
        {base.data && !memoryAccessLost(base.error) && <p className="memory-text">当前关联记录版本 {base.data.record.revision}：{base.data.content ?? '正文目前不可用；请先核对原记录。'}</p>}</>}
      {!canWrite && <p>当前身份只读。审批需要操作者明确授予 memory 配对档案。</p>}
      <div className="actions">
        <Confirm label="批准此条记忆" title="将这条具体内容用于今后的个性化？" disabled={!canWrite || !approve || pending} onConfirm={() => void decide('admit')}>
          <p className="memory-text">{value?.proposed_content}</p><p>{memoryScope} 仅批准候选版本 {c.revision}，不批准未来内容。</p>
        </Confirm>
        <Confirm label="拒绝此候选" title="拒绝并移除此候选正文？" disabled={!canWrite || c.status !== 'pending_review' || pending} onConfirm={() => void decide('reject')}>
          不创建长期记录，候选正文将移除；保留最小审计。纠正候选被拒绝不改变原正式记忆。
        </Confirm>
        <Button variant="outline" disabled={pending} onClick={() => { setError(undefined); void detail.refetch(); if (target) void base.refetch() }}>核对候选最新状态</Button>
      </div>
    </>}
    <OperationStatus result={result} />
  </section>
}

function CandidateForm({ base }: { base?: MemoryRecord }) {
  const { session, drafts } = useIdentity()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [result, setResult] = useState<MemoryOperation>()
  const [until] = useState(() => new Date(Date.now() + 7 * 86400000).toISOString().slice(0, 16))
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (pending) return
    const form = new FormData(event.currentTarget)
    const body = { payload_schema_version: 1 as const, content: String(form.get('content')), reason: String(form.get('reason')),
      category: String(form.get('category')) as CandidateInput['category'], sensitivity: form.get('sensitive') === 'on' ? 'sensitive' as const : 'non_sensitive' as const,
      stability: 'stable' as const, valid_until: new Date(String(form.get('until')) + ':00Z').toISOString() }
    const operation_id = drafts.operation(`memory:create:${base?.record.logical_memory_id ?? 'new'}`, { ...body, base: base?.record })
    setPending(true); setError(undefined)
    try { setResult(await memoryAPI(session).create({ ...body, operation_id }, base)) }
    catch (e) { setError(e) }
    finally { setPending(false) }
  }
  return <section className="panel"><h2>{base ? '提出纠正候选' : '新建长期偏好候选'}</h2>
    <p>只填写稳定长期信息。“今天只有20分钟”等请在当次聊天提出。创建后仍需查看并明确批准。</p>
    <form onSubmit={(e) => void submit(e)} autoComplete="off"><fieldset disabled={pending || !!result}>
      <label>具体内容<textarea name="content" required maxLength={4000} defaultValue={base?.content ?? ''} /></label>
      <label>保存或纠正理由<textarea name="reason" required maxLength={500} /></label>
      <label>类别<select name="category"><option value="interaction_preference">交互偏好</option><option value="time_constraint">长期时间安排</option><option value="personal_context">长期背景</option></select></label>
      <label><input type="checkbox" name="sensitive" />包含敏感信息</label>
      <label>审阅与交付期限（UTC）<input type="datetime-local" name="until" required defaultValue={until} /></label>
      <p>{memoryScope}</p>
      <Button type="submit">创建待审阅候选</Button>
    </fieldset></form>
    {!!error && <><ErrorState error={error} /><p>结果未知时可重试相同内容，沿用原操作身份。不要重复新建不同操作。</p></>}
    <OperationStatus result={result} />
  </section>
}

function RecordDetail({ memoryId, deliveryOnly = false }: { memoryId: string; deliveryOnly?: boolean }) {
  const { session, prefix, drafts } = useIdentity()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [result, setResult] = useState<MemoryOperation>()
  const [correcting, setCorrecting] = useState(false)
  const detail = useQuery({ queryKey: [...prefix, 'memory', 'record', memoryId], queryFn: ({ signal }) => memoryAPI(session, signal).record(memoryId), gcTime: 0 })
  const value = memoryAccessLost(detail.error) || memoryAccessLost(error) ? undefined : detail.data
  const write = session.device.scopes.includes('memory:write') && session.device.scopes.includes('memory:web')
  const act = async (kind: 'delete' | 'replay') => {
    if (!value || pending) return
    const body = { payload_schema_version: 1 as const, expected_revision: value.record.revision, expected_record_generation: value.record.record_generation }
    const operation_id = drafts.operation(`memory:${kind}:${memoryId}`, body)
    setPending(true); setError(undefined)
    try {
      setResult(kind === 'delete' ? await memoryAPI(session).remove(memoryId, { ...body, operation_id }) : await memoryAPI(session).replay(value.delivery.delivery_id, operation_id))
      setCorrecting(false); await detail.refetch()
    } catch (e) { setError(e) }
    finally { setPending(false) }
  }
  return <><section className="panel"><h2>{deliveryOnly ? '交付状态与重放' : '已保存记录详情'}</h2>
    {detail.isPending && <p>正在读取正式记录…</p>}
    {detail.error && <ErrorState error={detail.error} retry={() => void detail.refetch()} />}
    {!!error && <ErrorState error={error} />}
    {value && <>
      <p>{recordNames[value.record.status]} · 版本 {value.record.revision}</p><p>{memoryScope}</p>
      <p className="memory-text">{value.content_status === 'available' ? value.content : '正文不可读取，可能等待交付、远端不可用或已经清除；不代表空记忆。'}</p>
      <a href={`/app/memory?candidate=${value.record.candidate_id}`}>为什么使用这条信息：查看准入来源和理由</a>
      <p>交付：{value.delivery.public_status === 'applied' ? '远端已确认' : value.delivery.public_status === 'queued' ? '仍在等待，不宣称完成' : '已拒绝'}；尝试 {value.delivery.attempt_count} 次。</p>
      <p>删除/保存回执：{stepNames[value.receipt.status]} · {value.receipt.reason} · {value.receipt.verification_method}</p>
      <p>回执 ID：{value.receipt.receipt_id}</p>
      <div className="actions"><Button variant="outline" disabled={pending} onClick={() => { setError(undefined); void detail.refetch() }}>刷新交付回执</Button>
        <Button variant="outline" disabled={!write || pending || value.record.status !== 'applied' || value.content_status !== 'available'} onClick={() => setCorrecting(!correcting)}>提出纠正</Button>
        <Confirm label="删除此条记忆" title="删除这条全局记忆？" disabled={!write || pending || ['deleted', 'delete_pending'].includes(value.record.status)} onConfirm={() => void act('delete')}>
          删除该条记忆及其远端版本，需等待原服务验证。此动作不删除聊天、目标、学习事实或已下载导出；不能恢复已删除正文。
        </Confirm>
        <Confirm label="重放交付" title="按原交付身份申请重试？" disabled={!write || pending || value.delivery.public_status !== 'queued' || new Date(value.delivery.valid_until).getTime() <= Date.now()} onConfirm={() => void act('replay')}>
          仅原服务允许的失败交付可以重放；不绕过期限、版本、删除或隐私屏障。普通排队或未知结果仍先对账。
        </Confirm>
      </div>
    </>}
    <OperationStatus result={result} />
  </section>{correcting && value && <CandidateForm key={`${memoryId}:${value.record.revision}`} base={value} />}</>
}

export function MemoryPage({ candidateId, memoryId }: { candidateId?: string; memoryId?: string }) {
  const { session, prefix } = useIdentity()
  const [tab, setTab] = useState<'candidates' | 'records' | 'corrections' | 'delivery'>(memoryId ? 'records' : 'candidates')
  const [cursors, setCursors] = useState<(string | undefined)[]>([undefined])
  const [creating, setCreating] = useState(false)
  const [exporting, setExporting] = useState(false)
  const [error, setError] = useState<unknown>()
  const [message, setMessage] = useState('')
  const cursor = cursors.at(-1)
  const read = session.device.scopes.includes('memory:read')
  const write = session.device.scopes.includes('memory:write') && session.device.scopes.includes('memory:web')
  const candidates = useQuery({ queryKey: [...prefix, 'memory', 'candidates', cursor], queryFn: ({ signal }) => memoryAPI(session, signal).candidates(cursor), enabled: read && (tab === 'candidates' || tab === 'corrections'), gcTime: 0 })
  const records = useQuery({ queryKey: [...prefix, 'memory', 'records', cursor], queryFn: ({ signal }) => memoryAPI(session, signal).records(cursor), enabled: read && (tab === 'records' || tab === 'delivery'), gcTime: 0 })
  const list = tab === 'candidates' || tab === 'corrections' ? candidates : records
  const exportPage = async () => {
    if (exporting) return
    setExporting(true); setError(undefined); setMessage('')
    try {
      const data = await memoryAPI(session).export(cursor)
      const url = URL.createObjectURL(new Blob([JSON.stringify({ scope: memoryScope, page: cursors.length, ...data }, null, 2)], { type: 'application/json' }))
      const link = document.createElement('a'); link.href = url; link.download = `长期记忆-第${cursors.length}页.json`; link.click(); URL.revokeObjectURL(url)
      setMessage(`已导出本页 ${data.items.length} 条。${data.degraded ? '部分正文不可用，导出保留真实缺失状态。' : ''}${data.next_cursor ? '还有下一页，请翻页后继续导出。' : ''}`)
    } catch (e) { setError(e) }
    finally { setExporting(false) }
  }
  return <><section className="intro"><h1>长期记忆</h1><p>{memoryScope}</p><p>保存聊天、记录学习事实和保存长期记忆是不同动作。Nocturne 不保存成绩册或掌握度真值。</p><a href="/app/settings/data">数据与隐私管理</a></section>
    {!read ? <p>当前配对没有记忆读取权限；请联系操作者使用 memory 档案重新配对。已有记录不会因此删除。</p> : <>
      <div role="tablist" aria-label="记忆分类">{([['candidates', '候选'], ['records', '已保存'], ['corrections', '纠正'], ['delivery', '交付状态']] as const).map(([id, label]) => <Button key={id} role="tab" aria-selected={tab === id} variant="outline" onClick={() => { setTab(id); setCursors([undefined]); setError(undefined) }}>{label}</Button>)}</div>
      {candidateId && <CandidateReview key={candidateId} candidateId={candidateId} />}
      {memoryId && <RecordDetail key={memoryId} memoryId={memoryId} deliveryOnly={tab === 'delivery'} />}
      <section className="panel"><h2>{tab === 'candidates' ? '候选列表' : tab === 'corrections' ? '纠正候选' : tab === 'delivery' ? '交付记录' : '正式记录'}</h2>
        {tab === 'corrections' && <p>按原候选分页，仅显示本页待审批的纠正候选；其他页仍可翻阅。已批准的纠正请到正式记录和交付状态查看。</p>}
        {list.error && <ErrorState error={list.error} retry={() => void list.refetch()} />}
        {list.isPending && <p>正在读取…</p>}
        {!list.error && (tab === 'candidates' || tab === 'corrections') && candidates.data?.items.filter(v => tab !== 'corrections' || !!v.candidate.logical_memory_id && v.candidate.status === 'pending_review').map(v => <article key={v.candidate.candidate_id}><a href={`/app/memory?candidate=${v.candidate.candidate_id}`}>{candidateNames[v.candidate.status]} · {categoryNames[v.candidate.category] ?? '记忆候选'} · {new Date(v.candidate.created_at).toLocaleString('zh-CN')}</a><p>{v.candidate.reason}</p></article>)}
        {!list.error && (tab === 'records' || tab === 'delivery') && records.data?.items.map(v => <p key={v.logical_memory_id}><a href={`/app/memory?record=${v.logical_memory_id}`}>{recordNames[v.status]} · 版本 {v.revision} · {new Date(v.created_at).toLocaleString('zh-CN')}</a></p>)}
        {!list.error && list.data?.items.length === 0 && <p>此页暂无记录。</p>}
        <Pagination page={cursors.length} previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined} next={list.data?.next_cursor && !list.error ? () => setCursors([...cursors, list.data!.next_cursor]) : undefined} />
        {write && <Button variant="outline" onClick={() => setCreating(!creating)}>{creating ? '关闭新建表单' : '新建偏好候选'}</Button>}
        {(tab === 'records' || tab === 'delivery') && <Confirm label="导出本页长期记忆" title="将本页长期信息下载到当前设备？" disabled={exporting || !!list.error || !list.data} onConfirm={() => void exportPage()}>
          范围仅为全局正式记忆列表的当前页（最多 25 条），含敏感正文和准入/交付元数据，不含聊天、成绩、模型或搜索 Key。已下载文件不受服务端删除控制。
        </Confirm>}
        {!!error && <ErrorState error={error} />}{message && <p role="status">{message}</p>}
      </section>{creating && write && <CandidateForm />}
    </>}</>
}
