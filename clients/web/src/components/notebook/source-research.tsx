import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from '@/api/client'
import { sourceResearchAPI, type ResearchEvent, type ResearchPreview } from '@/api/source-research'
import { webCoverage, type WebCitation } from '@/api/web-search'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { failureLabel } from './web-search-record'
import { WebCitationContent } from './web-citation'

const stages: Record<string, string> = { planning: '规划方向', searching: '搜索资料', reading: '阅读正文', screening: '筛选资料', reporting: '整理报告' }
const statuses: Record<string, string> = { queued: '排队中', running: '搜集中', stopping: '正在停止', stopped: '已停止，结果未完成', paused_tool_steps: '工具调用已暂停', completed: '搜集完成', failed: '搜集失败，保留已完成结果', outcome_unknown: '请求结果未知，已保留提交的结果' }
export function SourceResearch({ spaceId }: { spaceId: string }) {
  const { session, prefix, drafts } = useIdentity(), queryClient = useQueryClient()
  const api = sourceResearchAPI(session, spaceId), key = [...prefix, spaceId, 'source-research']
  const [jobID, setJobID] = useState(''), [request, setRequest] = useState(''), [busy, setBusy] = useState(false), [error, setError] = useState<unknown>()
  const [additionalRequest, setAdditionalRequest] = useState(''), [reportID, setReportID] = useState('')
  const [citation,setCitation] = useState<WebCitation>()
  const [selectionDraft, setSelectionDraft] = useState<{ id: string; selected: boolean; reportID?: string }>()
  useEffect(()=>{setCitation(undefined);setReportID('');setAdditionalRequest('')},[jobID])
  const [events, setEvents] = useState<ResearchEvent[]>([]), [preview, setPreview] = useState<ResearchPreview>(), [accepted, setAccepted] = useState<string[]>([]), [done, setDone] = useState<string[]>([])
  const capability = useQuery({ queryKey: [...key, 'capabilities'], queryFn: () => api.capabilities(), retry: false })
  const jobs = useQuery({ queryKey: [...key, 'jobs'], queryFn: () => api.list(), retry: false })
  useEffect(() => {
    if (!jobID && jobs.data?.items.length) setJobID((jobs.data.items.find(j => j.available_actions.includes('stop')) ?? jobs.data.items[0]).job_id)
  }, [jobID, jobs.data])
  const jobQuery = useQuery({ queryKey: [...key, jobID], queryFn: () => api.job(jobID), enabled: !!jobID, retry: false, refetchInterval: query => query.state.data?.available_actions.includes('stop') ? 3000 : false })
  const job = jobQuery.data, active = !!job?.available_actions.includes('stop')
  useEffect(() => { if (job?.import_preview_id && !preview) void api.imports(jobID,job.import_preview_id).then(saved => {if (new Date(saved.preview.expires_at).getTime()>Date.now()){setPreview(saved.preview);setDone(saved.items.filter(i=>i.status==='imported').map(i=>i.item_id))}}).catch(setError) }, [jobID,job?.import_preview_id])
  const candidates = useQuery({ queryKey: [...key, jobID, 'candidates', job?.result_snapshot_id, job?.selection_version], queryFn: async () => {const page=await api.candidates(jobID);while(page.next_cursor){const next=await api.candidates(jobID,page.next_cursor);page.items.push(...next.items);page.next_cursor=next.next_cursor};return page}, enabled: !!job?.result_snapshot_id, retry: false })
  const reports = useQuery({queryKey:[...key,jobID,'reports',job?.report_revision_id,job?.status],queryFn:async()=>{const page=await api.reports(jobID);while(page.next_before){const next=await api.reports(jobID,page.next_before);page.items.push(...next.items);page.next_before=next.next_before}return page.items},enabled:!!job?.report_revision_id&&!active,retry:false})
  const viewedReportID = reportID || job?.report_revision_id || ''
  const report = useQuery({ queryKey: [...key, jobID, 'report', viewedReportID, job?.status], queryFn: async () => {
    const first = await api.report(jobID, viewedReportID)
    while (first.next_ordinal) { const next = await api.report(jobID, viewedReportID, first.next_ordinal); first.sections.push(...next.sections); first.next_ordinal = next.next_ordinal }
    return first
  }, enabled: !!job?.report_revision_id && !active, retry: false })
  useEffect(() => {
    if (!jobID) return
    const controller = new AbortController()
    void (async()=>{
      let after=0, backoff=500
      while(!controller.signal.aborted){
        try {
          await api.watch(jobID,after,controller.signal,incoming=>{
            after=incoming.at(-1)!.seq
            setEvents(old=>[...old.filter(e=>e.seq<incoming[0].seq),...incoming].slice(-100))
            void queryClient.invalidateQueries({queryKey:[...key,jobID],exact:true})
            backoff=500
            if(!['queued','running','stopping'].includes(incoming.at(-1)!.status) && incoming.at(-1)!.seq >= (job?.event_seq ?? 0)) controller.abort()
          })
        } catch(e){
          if(controller.signal.aborted)return
          setError(e)
          if(e instanceof ApiError && [401,403,404,503].includes(e.status)){queryClient.removeQueries({queryKey:key});return}
        }
        if(controller.signal.aborted)return
        await new Promise<void>(resolve=>{const end=()=>{clearTimeout(timer);controller.signal.removeEventListener('abort',end);resolve()};const timer=setTimeout(end,backoff);controller.signal.addEventListener('abort',end,{once:true})})
        backoff=Math.min(backoff*2,10000)
      }
    })()

    return () => controller.abort()
  }, [jobID, job?.version, session.generation, spaceId])
  const refresh = async () => { await queryClient.invalidateQueries({ predicate: q => q.queryKey.includes(spaceId) }) }
  const run = async (fn: () => Promise<void>) => { setBusy(true); setError(undefined); try { await fn() } catch (e) { setError(e); await jobQuery.refetch() } finally { setBusy(false) } }
  const select = (id: string, selected: boolean, reportID?: string) => {
    setSelectionDraft({ id, selected, reportID })
    return run(async () => {
      try {
        if (!job) return
        await queryClient.cancelQueries({ queryKey: [...key, jobID], exact: true })
        const next = await api.select(jobID, { operation_id: crypto.randomUUID(), expected_selection_version: job.selection_version, items: id ? [{ candidate_id: id, selected }] : [], ...(reportID !== undefined ? { report_revision_id: reportID || null } : {}) })
        if (candidates.data) queryClient.setQueryData([...key, jobID, 'candidates', next.result_snapshot_id, next.selection_version], { ...candidates.data, items: candidates.data.items.map(c => c.id === id ? { ...c, selected } : c) })
        queryClient.setQueryData([...key, jobID], next); setPreview(undefined); setAccepted([]); setDone([])
      } finally { setSelectionDraft(undefined) }
    })
  }
  const importAll = () => run(async () => {
    if (!preview || !job) return
    const receipts = await api.imports(jobID, preview.preview_id), completed = receipts.items.filter(r => r.status === 'imported' || r.status === 'already_imported').map(r => r.item_id)
    setDone(completed)
    for (const item of preview.items) {
      if (completed.includes(item.item_id)) continue
      await api.import(jobID, preview, item, accepted.includes(item.item_id))
      completed.push(item.item_id); setDone([...completed]); await refresh()
    }
  })
  const selected = candidates.data?.items.filter(c => (selectionDraft?.id === c.id ? selectionDraft.selected : c.selected) && !c.source).length ?? 0
  const selectedReport = !!(selectionDraft?.reportID !== undefined ? selectionDraft.reportID : job?.selected_report_revision_id)
  return <section className="source-research" aria-label="智能搜集资料">
    <h3>智能搜集资料</h3>
    {jobs.data && jobs.data.items.length > 1 && <label>已保存的搜集<select value={jobID} onChange={e => { setJobID(e.target.value); setPreview(undefined); setEvents([]) }}>{jobs.data.items.map(j => <option key={j.job_id} value={j.job_id}>{j.request.slice(0, 40)} · {statuses[j.status]}</option>)}</select></label>}
    <label>想找什么资料？<textarea value={request} maxLength={4000} disabled={busy || active} placeholder="可以说明用途、基础和希望覆盖的内容。" onChange={e => setRequest(e.target.value)} /></label>
    <p>将根据你的描述搜索并读取网页，进度和结果保存 7 天；你可以随时停止。只有确认导入的资料才会加入来源。</p>
    <details><summary>外发与保存说明</summary><p>搜索词会发送给搜索服务，需求和读到的内容会用于模型整理。已发出的请求可能产生用量，停止无法撤回。</p></details>
    {capability.data && !capability.data.available && <p role="alert">请先在设置中配置并启用模型和 Brave 搜索。</p>}
    <Button disabled={busy || active || !request.trim() || !capability.data?.available} onClick={() => void run(async () => {
      const body = { request: request.trim() }, operation_id = drafts.operation(JSON.stringify([...key, 'start']), body)
      const created = await api.start({ ...body, operation_id }); drafts.delete(JSON.stringify([...key, 'start'])); setJobID(created.job_id); setEvents([]); setPreview(undefined); setDone([]); setAccepted([])
      queryClient.setQueryData([...key, created.job_id], created); await jobs.refetch()
    })}>开始搜集</Button>
    {job && <>
      <p>本次需求：{job.request}</p>
      <p role="status" aria-live="polite">{statuses[job.status]} · {stages[job.stage] ?? job.stage} · 第 {job.round_number} 轮 · 已尝试 {job.progress.read_attempted}/{job.limits.max_pages_per_round} 页，成功 {job.progress.read_succeeded}，失败 {job.progress.read_failed} · 已找到 {job.candidate_count} 份新增候选</p>
      {active && <Button className="source-research-stop" variant="outline" disabled={busy} onClick={() => void run(async () => { const next = await api.command(jobID, { operation_id: crypto.randomUUID(), expected_version: job.version, round_id: job.round_id, kind: 'stop' }); queryClient.setQueryData([...key, jobID], next) })}>停止搜集</Button>}
      {job.destination_change && <p>连接目的地已变化。继续或补充会将需求与已读公开片段发送给模型 {job.destination_change.model.model}（{job.destination_change.model.endpoint}），搜索词发送至 {job.destination_change.search.endpoint}。</p>}
      {job.available_actions.includes('resume') && <Button disabled={busy || !capability.data?.available} onClick={() => void run(async () => { const next = await api.command(jobID, { operation_id: crypto.randomUUID(), expected_version: job.version, round_id: job.round_id, kind: 'resume', ...(job.destination_change?{confirm_destination:job.destination_change.confirmation}:{}) }); queryClient.setQueryData([...key, jobID], next) })}>{job.destination_change ? '确认目的地并继续这次搜集' : '继续这次搜集'}</Button>}
      {job.available_actions.includes('more') && <section aria-label="开始补充搜集">
        <label>补充要求（可选）<textarea value={additionalRequest} maxLength={4000} disabled={busy} onChange={e=>setAdditionalRequest(e.target.value)}/></label>
        <p>开始补充搜集，按当前设置获得新名额。{job.status!=='completed'?'原轮未完成的部分不会自动恢复。':''}旧结果、已取消项和报告版本保留。</p>
        <Button disabled={busy || !capability.data?.available} onClick={()=>void run(async()=>{const body={expected_version:job.version,additional_request:additionalRequest.trim(),...(job.destination_change?{confirm_destination:job.destination_change.confirmation}:{})};const draftKey=JSON.stringify([...key,jobID,'more']);const next=await api.more(jobID,{...body,operation_id:drafts.operation(draftKey,body)});drafts.delete(draftKey);setAdditionalRequest('');setReportID('');setPreview(undefined);setDone([]);setAccepted([]);queryClient.setQueryData([...key,jobID],next)})}>{job.destination_change?'确认目的地并再找一些':'再找一些'}</Button>
      </section>}
      <ol>{job.directions.map(d => <li key={d.id}><strong>{d.title}</strong>：{d.purpose}</li>)}</ol>
      {job.progress.query && <p>当前搜索词：{job.progress.query}</p>}{job.progress.title && <p>当前页面：{job.progress.title}</p>}
      {job.progress.notice && <p role="status">{job.progress.notice}</p>}
      <details><summary>查看检索进度记录</summary><ol>{events.map(e => <li key={e.seq}>{stages[e.stage] ?? e.stage} · {e.progress.title || e.progress.query || `已读取 ${e.progress.read_succeeded} 页`}</li>)}</ol></details>
      {!!job.progress.failures?.length && <details><summary>读取失败 {job.progress.failures.length} 项</summary><ul>{job.progress.failures.map(f => <li key={f.resource_id}>{f.title}：{failureLabel(f.category)}</li>)}</ul></details>}
      <p className="hint">保留至 {new Date(job.expires_at).toLocaleString()}。资料数不含可选的 1 份报告；一个 PDF 按一个网页计算，失败也占阅读名额。</p>
      {job.usage_task_id && <a href="/app/usage">查看搜集用量</a>}
      {!!job.result_snapshot_id && <>
        <fieldset disabled={busy}><legend>候选资料</legend>
          {candidates.data?.items.length === 0 && <p>未找到合格候选，请查看报告中的缺口。</p>}
          {candidates.data?.items.map(c => <article key={c.id} className="panel compact">
            <label><input type="checkbox" aria-label={`选择候选：${c.title}`} checked={selectionDraft?.id === c.id ? selectionDraft.selected : c.selected} disabled={!!c.source} onChange={e => void select(c.id, e.target.checked)} />{c.title}</label>
            <p>{c.round_id===job.round_id?'本轮新增':'此前结果'}</p><p className="reference-text">{c.url}</p><p>{c.media_type}{c.physical_pages > 0 ? ` · ${c.physical_pages} 个物理页` : ''}{c.source ? ' · 已加入来源' : ''}</p>
            <p>入选理由：{c.reason}</p><p>{webCoverage[c.coverage] ?? c.coverage}</p><p>{c.quality_notes}</p>
          </article>)}
          {!!reports.data?.length && <label>报告版本<select value={viewedReportID} onChange={e=>{setReportID(e.target.value);setCitation(undefined)}}>{reports.data.map(r=><option key={r.revision_id} value={r.revision_id}>版本 {r.version}{r.revision_id===job.report_revision_id?'（最新）':''} · {r.status==='partial'?'未完成':'完整'}</option>)}</select></label>}
          <details><summary>整理报告{report.data?.status === 'partial' ? '（未完成）' : ''}</summary>
            {report.data?.sections.map(section => <article key={section.id}><h4>{section.title}</h4><p style={{whiteSpace:'pre-wrap'}}>{section.body.split(/(\[[a-z0-9-]+:[1-9][0-9]*\])/g).map((part,i)=>{const c=section.citations.find(c=>'['+c.id+']'===part);return c?<button className="citation-link" key={i} type="button" onClick={()=>setCitation(c)}>{part}</button>:part})}</p>{section.citations.map(c => <details key={c.id}><summary>[{c.id}] 引文{c.page ? ` · 第 ${c.page} 页` : ''}</summary><blockquote>{c.quote}</blockquote><p>引文已核对实际读取的正文位置，尚未保存为来源。</p></details>)}{section.gaps.map((gap, i) => <p key={i}>缺口：{gap}</p>)}</article>)}
            {citation && <WebCitationContent spaceId={spaceId} runId={job.run_id} citation={citation} importEnabled={false} />}
            {report.data?.gaps.map((gap,i)=><p key={`report-gap-${i}`}>整体缺口：{gap}</p>)}
            {!report.data && <p>整理报告尚未生成。</p>}
          </details>
          <label><input type="checkbox" checked={selectedReport} disabled={!report.data?.sections.length} onChange={e => void select('', false, e.target.checked ? viewedReportID : '')} />将整理报告也加入来源</label>
          {selectedReport && job.selected_report_revision_id!==job.report_revision_id && <p>已选择旧版报告。当前有新版报告，<Button variant="outline" onClick={()=>void select('',false,job.report_revision_id)}>改选新版报告</Button></p>}
          <p>将加入 {Math.min(selected,job.limits.max_sources_per_round)} 份资料，{selectedReport ? '另含 1 份整理报告' : '不含整理报告'}。</p>
          {selected>job.limits.max_sources_per_round&&<p>已选 {selected} 份资料，本次最多导入 {job.limits.max_sources_per_round} 份，其余可在本次完成后继续分批导入。</p>}
          <Button disabled={!capability.data?.can_import || selected === 0 && !selectedReport} onClick={() => void run(async () => { const body = { result_snapshot_id: job.result_snapshot_id, expected_selection_version: job.selection_version }; const next = await api.preview(jobID, { ...body, operation_id: drafts.operation(JSON.stringify([...key, jobID, 'preview']), body) }); drafts.delete(JSON.stringify([...key, jobID, 'preview'])); setPreview(next); setAccepted([]); setDone([]) })}>查看导入预览</Button>
        </fieldset>
        {preview && <section aria-label="确认导入预览"><h4>确认导入预览</h4><p>资料保存到当前学习区的私有来源。报告会保留引用片段和原网页地址；未选网页不会另建来源。</p>
          {preview.items.map(item => <div key={item.item_id}><p>{item.title} · {done.includes(item.item_id) || item.already_imported ? '已加入来源' : '待导入'} · {webCoverage[item.coverage] ?? item.coverage}</p>{item.requires_partial_accept && <label><input type="checkbox" checked={accepted.includes(item.item_id)} disabled={busy} onChange={e => setAccepted(old => e.target.checked ? [...old, item.item_id] : old.filter(id => id !== item.item_id))} />{item.kind === 'document' ? '仅保存可用文本' : '接受未完成的整理报告'}：{item.title}</label>}</div>)}
          <Button disabled={busy || done.length === preview.items.length || preview.items.some(item => item.requires_partial_accept && !done.includes(item.item_id) && !accepted.includes(item.item_id))} onClick={() => void importAll()}>{done.length ? '重试未完成项' : '确认导入'}</Button>
          {done.length === preview.items.length && <p role="status">已导入所选来源。</p>}
        </section>}
      </>}
    </>}
    {[error, capability.error, jobQuery.error, candidates.error, report.error, reports.error].filter(Boolean).map((e, i) => <ErrorState key={i} error={e} />)}
  </section>
}
