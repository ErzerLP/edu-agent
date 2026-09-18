import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { adaptiveMode, changeCapabilities, changeContext, changeHeader, changeSchema, changesSchema, changeStatus, type LearningChange } from '@/api/changes'
import type { Goal } from '@/api/runtime'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { ErrorState } from './common'

export function ChangePanel({ goal, teachingSessionId, archived, onChanged, onRestore }: { goal: Goal; teachingSessionId?: string; archived: boolean; onChanged?: () => void; onRestore?: (activityId: string) => void }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const signature = useRef('')
  const space = goal.learning_space_id
  const client = () => learningClient(session, space)
  const params = { path: { goalID: goal.goal_id }, header: changeHeader(space) }
  const key = [...prefix, space, goal.goal_id, 'learning-changes']
  const capability = useQuery({ queryKey: [...prefix, 'learning-change-capabilities'], queryFn: ({ signal }) => unwrap(client().GET('/v1/learning/changes/capabilities', { signal }), changeCapabilities), retry: false })
  const enabled = capability.data?.available === true
  const changes = useQuery({ queryKey: key, queryFn: async ({ signal }) => {
    const result = await unwrap(client().GET('/v1/learning/goals/{goalID}/changes', { params, signal }), changesSchema)
    if (result.items.some((c) => c.goal_id !== goal.goal_id || c.learning_space_id !== space || c.privacy_generation !== session.generation)) throw new ApiError(502, 'invalid_response')
    return result
  }, enabled, refetchInterval: 1500, gcTime: 0 })
  const mode = useQuery({ queryKey: [...key, 'mode'], queryFn: ({ signal }) => unwrap(client().GET('/v1/learning/goals/{goalID}/adaptive-mode', { params, signal }), adaptiveMode), enabled })
  const current = useQuery({ queryKey: [...key, teachingSessionId, 'route'], queryFn: ({ signal }) => unwrap(client().GET('/v1/learning/goals/{goalID}/change-context', { params: { ...params, query: { session_id: teachingSessionId! } }, signal }), changeContext), enabled: enabled && !!teachingSessionId, gcTime: 0 })
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  useEffect(() => {
    const next = JSON.stringify(changes.data?.items.map((c) => [c.id, c.status, c.restored, c.applied]) ?? [])
    if (signature.current && signature.current !== next) {
      void query.invalidateQueries({ queryKey: [...prefix, space], predicate: (q) => !q.queryKey.includes('learning-changes') })
      if (teachingSessionId) void current.refetch()
      onChanged?.()
    }
    signature.current = next
  }, [changes.data])
  const inactive = archived || !['draft', 'active'].includes(goal.management.status)
  const command = async (c: LearningChange, action: 'approve' | 'apply_now' | 'reject' | 'cancel' | 'compensate' | 'restore_focus') => {
    if (pending) return
    setPending(true); setError(undefined)
    const body = { session_id: c.session_id, action, expected_revision: c.revision, hash: c.hash, interaction_id: c.interaction_id, immediate: false as const }
    const operation_id = drafts.operation(JSON.stringify([...key, c.id, action]), body)
    try {
      await unwrap(client().POST('/v1/learning/goals/{goalID}/changes/{changeID}', { params: { ...params, path: { goalID: goal.goal_id, changeID: c.id } }, body: { ...body, operation_id } }), changeSchema)
      if (mounted.current && action === 'restore_focus') onRestore?.(c.base.activity_id)
      if (mounted.current) await changes.refetch()
    } catch (e) { if (mounted.current) { setError(e); void changes.refetch() } }
    finally { if (mounted.current) setPending(false) }
  }
  const setMode = async (value: 'adaptive' | 'cautious') => {
    if (!mode.data || pending) return
    setPending(true); setError(undefined)
    try { await unwrap(client().POST('/v1/learning/goals/{goalID}/adaptive-mode', { params, body: { mode: value, version: mode.data.version } }), adaptiveMode); if (mounted.current) await mode.refetch() }
    catch (e) { if (mounted.current) setError(e) }
    finally { if (mounted.current) setPending(false) }
  }
  const adoptKnowledge = async () => {
    if (!teachingSessionId || pending) return
    setPending(true); setError(undefined)
    try {
      const snap = await current.refetch()
      if (!snap.data) return
      const candidate = { kind: 'route' as const, trigger: 'authorized_source', reason: '接入已审阅知识维护，保留原课堂', evidence_ids: [], context_id: '', steps: snap.data.steps, explanation: '' }
      const payload = { session_id: teachingSessionId, base: snap.data.base, candidate }
      const id = drafts.operation(JSON.stringify([...key, teachingSessionId, 'knowledge-change']), payload)
      await unwrap(client().POST('/v1/learning/goals/{goalID}/changes/{changeID}', { params: { ...params, path: { goalID: goal.goal_id, changeID: id } }, body: { ...payload, operation_id: id, action: 'propose', expected_revision: 0, hash: '', interaction_id: '', immediate: false } }), changeSchema)
      if (mounted.current) await changes.refetch()
    } catch (e) { if (mounted.current) setError(e) }
    finally { if (mounted.current) setPending(false) }
  }
  if (!enabled) return <p className="hint">当前服务器尚未提供教学变更能力。</p>
  const items = changes.data?.items.filter((c) => !teachingSessionId || c.session_id === teachingSessionId) ?? []
  return <section className="panel" aria-label="教学变更">
    <h2>路径与教学变更</h2>
    <label>调整模式 <select value={mode.data?.mode ?? 'adaptive'} disabled={pending || inactive || !mode.data} onChange={(e) => void setMode(e.target.value as 'adaptive' | 'cautious')}><option value="adaptive">自适应：同目标调整在安全点应用</option><option value="cautious">谨慎：先预览后采用</option></select></label>
    <p className="hint">目标范围与完成标准始终需要具体确认。路径是建议，可跳过；跳过和自述不会记为掌握。</p>
    {teachingSessionId && <Button variant="outline" disabled={pending || inactive || !current.data?.steps.length} onClick={() => void adoptKnowledge()}>接入已审阅知识维护</Button>}
    {!!(error || changes.error || mode.error || current.error) && <ErrorState error={error || changes.error || mode.error || current.error} retry={() => { void changes.refetch(); void mode.refetch(); if (teachingSessionId) void current.refetch() }} />}
    {current.data && <details><summary>当前建议路径</summary><ol>{current.data.steps.map((step, i) => <li key={i}>{step.name} · {step.criterion}</li>)}</ol></details>}
    {items.length === 0 && <p>尚无教学变更。在课堂导师中提出“先补一个前置概念”或“换一种安排”。</p>}
    {items.map((c) => <article className="panel compact" key={c.id} aria-label={`变更：${c.candidate.reason}`}>
      <h3>{c.candidate.reason}</h3><p role="status">{changeStatus[c.status]} · 候选修订 {c.revision}</p>
      <p>依据：{({ user_request: '用户请求', activity_feedback: '正式活动反馈', authorized_source: '已授权来源', goal_constraint: '目标约束变化' } as Record<string, string>)[c.candidate.trigger]}{c.candidate.evidence_ids.length > 0 && `，${c.candidate.evidence_ids.length} 条正式证据`}</p>
      <p>影响：{c.impact}</p><p>生效时间：{c.status === 'queued_for_boundary' ? '本题处理后自动接入' : c.status === 'waiting_approval' ? '采用后按安全边界接入' : c.status_reason}</p>
      {c.candidate.kind === 'goal' ? <table><caption>目标旧值与新值</caption><thead><tr><th>字段</th><th>原值</th><th>新值</th></tr></thead><tbody>{Object.keys(c.diff.after_goal).filter((k) => JSON.stringify(c.diff.before_goal[k as keyof typeof c.diff.before_goal]) !== JSON.stringify(c.diff.after_goal[k as keyof typeof c.diff.after_goal])).map((k) => <tr key={k}><th>{({ name: '名称', expected_outcome: '预期结果', scope: '范围', exclusions: '不包含', self_assessment: '自述基础', purpose: '用途', completion_criteria: '完成标准', priority: '优先级', deadline: '期限', weekly_minutes: '每周分钟', timezone: '时区' } as Record<string, string>)[k] ?? k}</th><td>{String(c.diff.before_goal[k as keyof typeof c.diff.before_goal] ?? '未设置')}</td><td>{String(c.diff.after_goal[k as keyof typeof c.diff.after_goal] ?? '未设置')}</td></tr>)}</tbody></table>
        : c.candidate.kind === 'route' ? <div><p>原安排：{c.diff.before_steps.map((s) => s.name).join(' → ') || '无'}</p><p>新安排：{c.diff.after_steps.map((s) => s.name).join(' → ') || '等待来源'}</p><ol>{c.diff.after_steps.map((s, i) => <li key={i}>{s.prompt}<br />完成依据：{s.criterion} · 难度 {s.difficulty}/5</li>)}</ol></div>
        : <div><p>{c.candidate.explanation}</p>{c.compensates && <details><summary>查看呈现旧值与补偿后内容</summary><p>原呈现：{c.diff.before_content.map((b) => b.fallback).join('\n')}</p><p>补偿后：{c.diff.after_content.map((b) => b.fallback).join('\n')}</p></details>}</div>}
      {!teachingSessionId && <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId: space, sessionId: c.session_id }}>打开对应教学现场</Link>}
      <div className="actions">
        {c.status === 'waiting_approval' && <><Button disabled={pending || inactive} onClick={() => void command(c, 'approve')}>{c.risk === 'goal_scope' ? '采用新的目标范围和完成标准' : '本题处理后应用此安排'}</Button><Button variant="outline" disabled={pending} onClick={() => void command(c, 'reject')}>不采用此变更</Button></>}
        {c.status === 'queued_for_boundary' && <><Button disabled={pending || inactive} onClick={() => void command(c, 'apply_now')}>立即切换并保留原题现场</Button><Button variant="outline" disabled={pending} onClick={() => void command(c, 'cancel')}>取消排队</Button></>}
        {c.status === 'applied' && <Button variant="outline" disabled={pending || inactive} onClick={() => void command(c, 'compensate')}>预览补偿撤回</Button>}
        {c.frame_id && !c.restored && c.status === 'applied' && <Button disabled={pending || inactive} onClick={() => void command(c, 'restore_focus')}>返回原题与草稿</Button>}
      </div>
    </article>)}
  </section>
}
