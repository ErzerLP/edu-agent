import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './api/client'
import { progressPage, reviewsPage, progressQuery, progressSearch, reviewReasons, type ProgressSearch, type GoalProgress, type Review } from './api/progress'
import { labels, pageOf, spaceSchema } from './api/runtime'
import { operationResult, stateLabels } from './api/teaching'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { Confirm, ErrorState, Pagination } from './components/common'
import { FeedbackListPage } from './feedback-page'

const when = (s: string) => new Date(s).toLocaleString('zh-CN')
const defaultSpace = '00000000-0000-4000-8000-000000000001'

function ReviewItem({ item, refresh }: { item: Review; refresh: () => void }) {
  const { session, prefix, drafts } = useIdentity()
  const navigate = useNavigate()
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const busy = useRef(false)
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const start = async () => {
    if (busy.current || !item.attempt_id) return
    busy.current = true
    setPending(true)
    setError(undefined)
    const key = JSON.stringify([...prefix, item.learning_space_id, item.task_id, item.evidence_id, 'review-session'])
    const sessionID = drafts.get<string>(key) ?? crypto.randomUUID()
    drafts.set(key, sessionID)
    const payload = { payload_schema_version: 1 as const, aggregate_type: 'session' as const, aggregate_id: sessionID, expected_version: 0 as const, goal_revision_id: item.goal_revision_id, review_source: { task_id: item.task_id, evidence_id: item.evidence_id, attempt_id: item.attempt_id } }
    const client = learningClient(session, item.learning_space_id)
    try {
      await unwrap(client.POST('/v1/tutoring/sessions', { body: { ...payload, operation_id: drafts.operation(key, payload) } }), operationResult)
      drafts.delete(key)
      if (mounted.current) await navigate({ to: '/spaces/$spaceId/learn/$sessionId', params: { spaceId: item.learning_space_id, sessionId: sessionID } })
    } catch (e) {
      // 丢响应时只核对原身份；显式重试仍复用原操作，不自动再创建。
      if (mounted.current) { setError(e); refresh() }
    } finally { busy.current = false; if (mounted.current) setPending(false) }
  }
  const destination = item.carrier_session_id ?? (item.startable ? item.session_id : undefined)
  return <article className="panel compact" data-review-task={item.task_id}>
    <h3>{item.goal_name || '复习任务'} · {item.space_name}</h3>
    <p>到期：{when(item.due_at)}</p>
    {destination && <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId: item.learning_space_id, sessionId: destination }}>{item.unavailable_reason === 'goal_or_space_not_active' ? '查看复习承载' : '继续原任务复习'}</Link>}
    {!item.startable && (!item.carrier_session_id || item.unavailable_reason === 'goal_or_space_not_active') && <p className="hint">{reviewReasons[item.unavailable_reason ?? ''] ?? '任务目前不可开始，请核对原会话。'}</p>}
    <div className="actions">
      {item.session_id && destination !== item.session_id && <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId: item.learning_space_id, sessionId: item.session_id }}>查看原会话</Link>}
      {item.attempt_id && <Link to="/spaces/$spaceId/feedback/$attemptId" params={{ spaceId: item.learning_space_id, attemptId: item.attempt_id }}>原答案与证据</Link>}
      <Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId: item.learning_space_id, goalId: item.goal_id }}>返回原目标</Link>
      {!destination && item.attempt_id && item.unavailable_reason === 'original_session_not_at_review_node' && session.device.scopes.includes('learning:write') && <Confirm label="新建复习承载" title="为此原任务新建复习会话？" disabled={pending} onConfirm={() => void start()}>沿用本任务的原目标修订、来源版本及证据。新题需要明确生成并作答，原会话和旧答案保留。</Confirm>}
    </div>
    {!item.attempt_id && <p className="hint">此任务没有在线原答案入口；离线作答仍使用原离线 API/CLI 核对，不伪造新证据或自动迁移。</p>}
    <details><summary>来源版本</summary><p>任务：{item.task_id}</p><p>目标修订：{item.goal_revision_id}</p><p>知识版本：{item.knowledge_revision_id || '未知'}</p><p>来源会话：{item.session_id || '未知'}</p><p>证据：{item.evidence_id}</p></details>
    {!!error && <ErrorState error={error} />}
  </article>
}

function GoalItem({ item, compact, refresh }: { item: GoalProgress; compact?: boolean; refresh: () => void }) {
  const [assessmentError, setAssessmentError] = useState<unknown>()
  const { session } = useIdentity()
  const navigate = useNavigate()
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const spaceId = item.learning_space_id
  const name = item.goal.management.details.name
  const openAssessment = async (assessment: string) => {
    try {
      const result = await unwrap(learningClient(session, spaceId).GET('/v1/learning/assessments/{assessmentID}', { params: { path: { assessmentID: assessment } } }), z.object({ attempt: z.object({ attempt_id: z.uuid() }) }))
      if (mounted.current) await navigate({ to: '/spaces/$spaceId/feedback/$attemptId', params: { spaceId, attemptId: result.attempt.attempt_id } })
    } catch (e) { if (mounted.current) setAssessmentError(e) }
  }
  const mastery: Record<string, string> = { unseen: '未知，尚无证据', learning: '待巩固', provisional: '待确认', retained: '已有保持证据' }
  return <article className="panel section" aria-label={`目标进度：${name}`} data-goal-id={item.goal.goal_id}>
    <h3><Link className="underline" to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId: item.goal.goal_id }}>{name}</Link> <small>{item.space_name} · {labels[item.goal.management.status]}</small></h3>
    <Link to="/progress" search={progressSearch.parse({ space: spaceId, goal: item.goal.goal_id, status: 'all' })}>仅查看此目标</Link>
    {item.goal.management.completion && <p>手动完成依据：{item.goal.management.completion.reason}。此状态独立于路线与掌握证据。</p>}
    <p className="hint">截止：{item.goal.management.details.deadline ? when(item.goal.management.details.deadline) : '未设置'} · 活跃时间约 {Math.round(item.estimated_active_seconds / 60)} 分钟（按学习事件间隔估算）</p>
    <h4>继续位置与当前建议</h4>
    {item.sessions.length === 0 && <p>尚无学习会话，可返回目标明确开始。</p>}
    {item.sessions.map(s => <div className="session-row" key={s.session_id}>
      <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId, sessionId: s.session_id }}>{s.resumable ? '继续学习' : '查看原会话'} · {stateLabels[s.state] ?? s.state}</Link>
      <span>{s.position.replace(s.state, stateLabels[s.state] ?? s.state)}</span>{!s.resumable && <small>{s.state === 'Completed' ? '原会话已完成' : '目标或学习区当前不允许继续'}</small>}
    </div>)}
    {item.pending_assessments.length > 0 && <div><h4>待确认评估</h4>{item.pending_assessments.map(p => <div key={p.assessment_id}><Button variant="outline" onClick={() => void openAssessment(p.assessment_id)}>查看原评估与证据</Button><span>{p.reasons.join('、')}</span></div>)}</div>}
    {!!assessmentError && <ErrorState error={assessmentError} />}
    {!compact && <>
      <h4>路线版本与已完成活动</h4>
      {item.routes.length === 0 && <p>路线进度未知：尚无路线，不显示百分比。</p>}
      {item.routes.map(r => <details key={r.route.route_revision_id} open={item.sessions.some(s => s.route_revision_id === r.route.route_revision_id)}>
        <summary>路线修订 {r.route.revision} · {r.percent === null ? '进度未知' : `${r.numerator}/${r.denominator} 个活动完成（${Math.round(r.percent)}%）`}</summary>
        <p className="hint">版本：{r.route.route_revision_id} · 创建：{when(r.route.created_at)}。比例只反映此版本的明确完成步骤，不代表能力分数。</p>
        {r.previous_revision_id && <p>本版新增活动 {r.added_steps.length} 项，移除或替换 {r.removed_steps.length} 项。分母变化不代表掌握下降；前版完成记录保留。</p>}
        <ul>{r.route.steps.map(step => <li key={step.route_step_id}>{r.completed_steps.includes(step.route_step_id) ? '已完成' : '尚无本版完成记录'} · {step.teaching_intent} · {step.completion_condition}</li>)}</ul>
      </details>)}
      <h4>概念与正式证据</h4>
      {item.evidence_count === 0 && <p>掌握情况未知：尚无正式证据。自述和模型建议不作为掌握度。</p>}
      {item.nodes.map(n => <details key={n.mastery.node_revision_id}><summary>{item.routes.flatMap(r => r.route.steps).find(s => s.node_revision_id === n.mastery.node_revision_id)?.teaching_intent || '历史概念'} · {mastery[n.mastery.state] ?? n.mastery.state} · 证据 {n.mastery.valid_evidence_count} 条 · 待确认 {n.mastery.pending_assessments} 项</summary><small>适用节点版本：{n.mastery.node_revision_id}</small></details>)}
      {item.evidence_sources.map(e => <p key={e.evidence_id}>{e.attempt_id ? <Link to="/spaces/$spaceId/feedback/$attemptId" params={{ spaceId, attemptId: e.attempt_id }}>查看正式证据及原答案</Link> : '历史证据暂无在线答案入口'} · {e.current_goal_revision ? '当前目标修订' : '历史目标修订，保留原适用版本'}</p>)}
      <h4>近期学习</h4>
      {item.recent_activity.length === 0 && <p>尚无学习记录。</p>}
      {item.recent_activity.map(event => <p key={event.event_id}>{when(event.received_at)} · {eventLabels[event.event_type] ?? '学习状态已更新'} <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId, sessionId: event.parent_session_id ?? event.aggregate_id }}>原会话</Link></p>)}
      {item.recent_has_more && <p className="hint">仅显示最近十项事件，完整作答可在评估记录查看。</p>}
      <h4>复习任务</h4>{item.reviews.map(review => <ReviewItem key={review.task_id} item={review} refresh={refresh} />)}
    </>}
  </article>
}
const eventLabels: Record<string, string> = { LearningSessionStarted: '学习会话已创建', RouteRevisionCreated: '路线版本已更新', ActivityIssued: '活动已生成', ReviewPresented: '复习活动已生成', AttemptSubmitted: '答案已保存', EvidenceAccepted: '正式证据已接纳', AssessmentMarkedProvisional: '评估待确认', AssessmentRecorded: '模型反馈已保存', RouteAdvanced: '活动已完成', LearningCompleted: '本次学习已完成' }

export function ProgressPanel({ search, compact = false }: { search: ProgressSearch; compact?: boolean }) {
  const { session, prefix } = useIdentity()
  const cache = useQueryClient()
  const [cursors, setCursors] = useState([''])
  const [saved, setSaved] = useState(false)
  const isReview = search.view === 'reviews'
  const query = useQuery({
    queryKey: [...prefix, 'progress', search, cursors.at(-1), compact],
    queryFn: async ({ signal }) => {
      const client = learningClient(session, search.space)
      const q = progressQuery(search, cursors.at(-1)!, compact ? 5 : 10)
      return isReview
        ? { kind: 'reviews' as const, page: await unwrap(client.GET('/v1/learning/reviews', { params: { query: { ...q, due_before: search.due } }, signal }), reviewsPage) }
        : { kind: 'goals' as const, page: await unwrap(client.GET('/v1/learning/progress', { params: { query: { ...q, order: search.order } }, signal }), progressPage) }
    },
    gcTime: 0,
  })
  const refresh = () => { setCursors(['']); void cache.invalidateQueries({ queryKey: [...prefix, 'progress'] }) }
  const error = query.error
  const redacted = error instanceof ApiError && ['content_redacted', 'privacy_clear_in_progress'].includes(error.code)
  useEffect(() => { if (redacted) cache.removeQueries({ queryKey: [...prefix, 'progress'], type: 'inactive' }) }, [redacted])
  const result = !error ? query.data : undefined
  const metadata = result?.page.metadata
  const incomplete = metadata && (metadata.rebuilding || metadata.degraded || metadata.incomplete || (result?.kind === 'goals' && result.page.committed_event_high_water > metadata.as_of_event_seq))
  return <section aria-label={isReview ? '到期复习' : '学习进度'}>
    {query.isFetching && <p role="status">正在读取服务端进度…</p>}
    {!!error && <div role="alert" className="notice">
      {redacted ? <p>学习数据已清除或正在清除，旧结果已撤下。</p> : error instanceof ApiError && error.code === 'stale_cursor' ? <><p>进度快照已变化，当前游标已失效。请返回第一页重新读取。</p><Button onClick={refresh}>从第一页重新读取</Button></> : <><p>进度投影或网络暂不可用，不能据此判断没有复习。</p><ErrorState error={error} retry={refresh} /></>}
    </div>}
    {metadata && <p className="hint">更新：{when(result!.page.updated_at)} · 投影代次 {metadata.generation} · 事件位置 {metadata.as_of_event_seq}</p>}
    {incomplete && <p role="status">{metadata?.rebuilding ? '投影重建中' : '投影延迟或不完整'}，当前仅展示已保存的快照，尚不能确认全部待办。{metadata?.reason_codes.join('、')}</p>}
    {result && <>
      <p>{isReview ? '复习任务' : '目标'}共 {result.page.total} 项 · 当前页 {result.page.items.length} 项</p>
      {result.kind === 'reviews' && <p className="hint">本轮截止：{when(result.page.due_before)}；翻页沿用服务器冻结截止时间。</p>}
      {result.page.data_cleared && <p>此范围的学习数据已清除，旧进度、复习及答案不可恢复。</p>}
      {result.page.items.length === 0 && !query.isFetching && !incomplete && !result.page.data_cleared && <p>{isReview ? '当前筛选和截止时间内没有到期复习。' : '当前筛选内没有目标进度。'}</p>}
      {result.kind === 'reviews' ? result.page.items.map(item => <ReviewItem key={item.task_id} item={item} refresh={refresh} />) : result.page.items.map(item => <GoalItem key={item.goal.goal_id} item={item} compact={compact} refresh={refresh} />)}
      <Pagination page={cursors.length} previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined} next={result.page.next_cursor ? () => setCursors([...cursors, result.page.next_cursor!]) : undefined} />
    </>}
    {(error || incomplete) && !redacted && <><Button variant="outline" onClick={() => setSaved(!saved)}>查看已保存的作答记录</Button>{saved && <FeedbackListPage spaceId={search.space ?? defaultSpace} />}{!search.space && saved && <p>当前显示默认区的原始记录；选择其他学习区可查看其记录。</p>}</>}
  </section>
}

export function ProgressPage({ search }: { search: ProgressSearch }) {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const spaces = useQuery({ queryKey: [...prefix, 'progress-spaces'], queryFn: ({ signal }) => unwrap(learningClient(session).GET('/v1/learning-spaces', { params: { query: { limit: 100 } }, signal }), pageOf(spaceSchema)), gcTime: 0 })
  const change = (value: Partial<ProgressSearch>) => void navigate({ to: '/progress', search: { ...search, ...value } })
  return <>
    <h1>学习进度与复习</h1>
    <form className="panel" onSubmit={e => e.preventDefault()}>
      <label>范围<select value={search.space ?? ''} onChange={e => change({ space: e.target.value || undefined, goal: undefined })}><option value="">全部学习区</option>{spaces.data?.items.map(s => <option value={s.id} key={s.id}>{s.name}</option>)}{search.space && !spaces.data?.items.some(s => s.id === search.space) && <option value={search.space}>当前学习区</option>}</select></label>
      {search.goal && <p>限定目标 <Button variant="ghost" onClick={() => change({ goal: undefined })}>查看此范围全部目标</Button></p>}
      <label>目标状态<select value={search.status} onChange={e => change({ status: e.target.value as ProgressSearch['status'] })}>{['active', 'draft', 'paused', 'completed', 'archived', 'all'].map(s => <option key={s} value={s}>{s === 'all' ? '全部状态（含归档区）' : labels[s as keyof typeof labels]}</option>)}</select></label>
      <label>列表<select value={search.view} onChange={e => change({ view: e.target.value as ProgressSearch['view'] })}><option value="goals">活动、概念与继续位置</option><option value="reviews">到期复习</option></select></label>
      {search.view === 'goals' ? <label>排序<select value={search.order} onChange={e => change({ order: e.target.value as ProgressSearch['order'] })}><option value="priority">优先级、目标截止时间</option><option value="recent">近期学习</option></select></label> : <label>复习截止时间（UTC）<input type="datetime-local" value={search.due?.slice(0, 16) ?? ''} onChange={e => change({ due: e.target.value ? new Date(e.target.value + 'Z').toISOString() : undefined })} /></label>}
      <p className="hint">默认只显示进行中目标及活跃区。暂停、完成与归档仅隐藏，历史和复习日期保留。</p>
    </form>
    <ProgressPanel key={JSON.stringify(search)} search={search} />
  </>
}

export function HomeProgress() {
  return <section className="section"><h2>继续学习与需要处理的事项</h2><ProgressPanel compact search={progressSearch.parse({ order: 'recent' })} /><details><summary>到期复习</summary><ProgressPanel search={progressSearch.parse({ view: 'reviews' })} /></details><Link to="/progress" search={progressSearch.parse({})}>查看全部进度与复习 →</Link></section>
}
