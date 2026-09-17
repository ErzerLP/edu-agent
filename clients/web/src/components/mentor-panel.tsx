import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { learningClient, unwrap } from '@/api/client'
import type { Goal } from '@/api/runtime'
import { settingsSchema } from '@/api/settings'
import { isMentorTerminal, mentorCurrentSchema, mentorReceiptSchema, mentorSnapshotSchema, mentorStatus, observeMentor, type MentorSnapshot } from '@/api/mentor'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { Confirm, ErrorState } from './common'

export function MentorPanel({ goal, archivedSpace, teachingSessionId }: { goal: Goal; archivedSpace: boolean; teachingSessionId?: string }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const key = JSON.stringify([...prefix, goal.learning_space_id, goal.goal_id, teachingSessionId, 'mentor'])
  const [prompt, setPrompt] = useState(() => drafts.get<string>(key) ?? '')
  const [save, setSave] = useState(false)
  const [requests, setRequests] = useState(3)
  const [tokens, setTokens] = useState(30000)
  const [run, setRun] = useState<MentorSnapshot>()
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const [answer, setAnswer] = useState('')
  const sessionId = useRef<string>(crypto.randomUUID())
  const mounted = useRef(true)
  const output = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  const header = { 'X-Learning-Space-ID': goal.learning_space_id }
  const client = () => learningClient(session, goal.learning_space_id)
  const current = useQuery({
    queryKey: [...prefix, goal.learning_space_id, goal.goal_id, 'mentor-current'],
    queryFn: ({ signal }) => unwrap(client().GET('/v1/learning/goals/{goalID}/runs', { params: { path: { goalID: goal.goal_id }, header }, signal }), mentorCurrentSchema),
    gcTime: 0,
    refetchInterval: (query) => {
      const other = query.state.data?.run
      return other && other.teaching_session_id !== teachingSessionId && !isMentorTerminal(other.status) ? 2500 : false
    },
  })
  const configuration = useQuery({
    queryKey: [...prefix, 'settings'],
    queryFn: ({ signal }) => unwrap(learningClient(session).GET('/v1/settings', { signal }), settingsSchema),
  })
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  useEffect(() => {
    if (current.data?.run) {
      const next = current.data.run
      if (next.goal_id === goal.goal_id && next.space_id === goal.learning_space_id && next.privacy_generation === session.generation) {
        sessionId.current = next.session_id
        if (next.teaching_session_id === teachingSessionId) {
          setRun((old) => !old || old.run_id !== next.run_id || old.watermark <= next.watermark ? next : old)
        } else {
          setRun(undefined)
        }
      }
    }
  }, [current.data, goal.goal_id, goal.learning_space_id, session.generation, teachingSessionId])
  useEffect(() => {
    if (!run) return
    return observeMentor(session, run, (next, failure) => {
      setRun(next); setError(failure)
      query.setQueryData([...prefix, goal.learning_space_id, goal.goal_id, 'mentor-current'], { run: next, save_available: current.data?.save_available ?? false })
    })
  }, [run?.run_id, session, key])
  useEffect(() => { if (follow.current && output.current) output.current.scrollTop = output.current.scrollHeight }, [run?.output])
  useEffect(() => { setAnswer('') }, [run?.interaction?.id])
  const limits = configuration.data?.limits
  const requestBudget = Math.min(requests, limits?.research_requests ?? requests)
  const tokenBudget = Math.min(tokens, limits?.research_tokens ?? tokens)
  const inactive = archivedSpace || !['draft', 'active'].includes(goal.management.status)
  const modelReady = configuration.data?.effective_mentor.enabled && configuration.data.effective_mentor.configured
  const active = run && !isMentorTerminal(run.status)
  const other = current.data?.run?.teaching_session_id !== teachingSessionId ? current.data?.run : undefined
  const otherActive = !!other && !isMentorTerminal(other.status)
  const refresh = async (runID: string) => {
    const result = await unwrap(client().GET('/v1/learning/runs/{runID}', { params: { path: { runID }, header } }), mentorSnapshotSchema)
    if (mounted.current) setRun(result)
  }
  const start = async () => {
    if (pending || !prompt.trim()) return
    setPending(true); setError(undefined)
    const payload = { session_id: run?.session_id ?? sessionId.current, expected_version: goal.revision, prompt, save, request_budget: requestBudget, token_budget: tokenBudget, ...(teachingSessionId ? { teaching_session_id: teachingSessionId } : {}) }
    const operation_id = drafts.operation(key + ':create', payload)
    try {
      const receipt = await unwrap(client().POST('/v1/learning/goals/{goalID}/runs', { params: { path: { goalID: goal.goal_id }, header }, body: { ...payload, operation_id } }), mentorReceiptSchema)
      await refresh(receipt.run_id)
      if (mounted.current) { setPrompt(''); drafts.delete(key) }
    } catch (e) { if (mounted.current) setError(e) }
    finally { if (mounted.current) setPending(false) }
  }
  const command = async (kind: 'stop' | 'clear' | 'respond' | 'continue_budget', selected?: string) => {
    if (!run || pending) return
    setPending(true); setError(undefined)
    const payload = { expected_version: run.version, kind,
      ...(kind === 'respond' ? { interaction_id: run.interaction?.id, answer: selected ?? answer } : {}),
      ...(kind === 'continue_budget' ? { request_budget: requestBudget, token_budget: tokenBudget } : {}),
    }
    const operation_id = drafts.operation(key + ':command', payload)
    try {
      await unwrap(client().POST('/v1/learning/runs/{runID}/commands', { params: { path: { runID: run.run_id }, header }, body: { ...payload, operation_id } }), mentorReceiptSchema)
      await refresh(run.run_id)
      if (mounted.current) setAnswer('')
    } catch (e) {
      if (mounted.current) setError(e)
      await refresh(run.run_id).catch(() => {})
    } finally { if (mounted.current) setPending(false) }
  }
  return <section className="panel mentor-panel" aria-label="目标内导师">
    <h2>目标内导师</h2>
    <p className="hint">{teachingSessionId ? '导师可以追加解释、建议新安排；路线按教学调整模式接入，目标范围和完成标准会先展示差异供你确认。' : '围绕这个目标交流、澄清想法。进入具体课堂后可调整教学安排。'}</p>
    {current.error && <ErrorState error={current.error} retry={() => void current.refetch()} />}
    {otherActive && <p role="status">该目标的导师正在另一处交流，本课堂不会显示或接入那里的输出。{other?.teaching_session_id
      ? <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId: goal.learning_space_id, sessionId: other.teaching_session_id }}>打开对应课堂查看或停止</Link>
      : <Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId: goal.learning_space_id, goalId: goal.goal_id }}>打开目标交流查看或停止</Link>}</p>}
    {configuration.error && <ErrorState error={configuration.error} retry={() => void configuration.refetch()} />}
    {!modelReady && <p>请先在设置中配置并启用 Web 导师模型。</p>}
    {inactive && <p>当前目标或学习区已暂停、完成或归档，不能发起或继续运行。</p>}
    {run && <>
      <p role="status">{mentorStatus[run.status]} · 最近活动：{new Date(run.updated_at).toLocaleTimeString('zh-CN')}</p>
      <div className="mentor-output" ref={output} onScroll={() => { const element = output.current; if (element) follow.current = element.scrollHeight - element.scrollTop - element.clientHeight < 40 }} aria-label="导师输出">
        {run.output || (run.body_available ? '等待导师输出…' : '正文已清除、到期，或临时交流不能在此进程恢复。')}
      </div>
      {run.result_unknown && <p role="alert">请求可能已到达模型服务，但结果无法确认；不会自动重发。</p>}
      {run.cost_unknown && <p className="hint">已发出 {run.requests_used} 次模型请求，实际费用未核实。</p>}
      <p className="hint">剩余预算：{run.requests_left} 次请求 / {run.tokens_left} Token 预留额度。</p>
      {run.saved && run.body_available && <p className="hint">必要恢复正文已加密保存，最晚保留至 {new Date(run.expires_at).toLocaleString('zh-CN')}。</p>}
      {run.interaction && <fieldset disabled={pending || inactive}>
        <legend>{run.interaction.approval ? '确认交流方向' : '导师需要你的回答'}</legend>
        <p>{run.interaction.question}</p>
        {run.interaction.choices.length > 0 ? <div className="actions">{run.interaction.choices.map((choice) => <Button key={choice} variant="outline" onClick={() => void command('respond', choice)}>{choice}</Button>)}</div> : <form onSubmit={(e) => { e.preventDefault(); void command('respond') }} onKeyDown={(e) => { if (e.key === 'Enter' && e.nativeEvent.isComposing) e.preventDefault() }}>
          <label>回应导师<textarea value={answer} onChange={(e) => setAnswer(e.target.value)} maxLength={2000} rows={3} /></label>
          <Button type="submit" disabled={!answer.trim()}>提交回答</Button>
        </form>}
      </fieldset>}
      <div className="actions">
        {active && <Button variant="outline" disabled={pending || run.status === 'cancelling'} onClick={() => void command('stop')}>{run.status === 'cancelling' ? '正在停止…' : '停止运行'}</Button>}
        {run.body_available && <Confirm label="清除本次正文" title="清除本次运行正文？" disabled={pending} onConfirm={() => void command('clear')}>正文和事件缓存将被清除，在途运行将停止，正式操作回执仍保留。</Confirm>}
      </div>
      <details><summary>技术信息</summary><p>会话：{run.session_id}<br />运行：{run.run_id}<br />版本：{run.version} · watermark：{run.watermark}<br />阶段：{run.stage} · 原因：{run.reason || '无'}</p></details>
    </>}
    <details open={run?.status === 'paused_budget'}><summary>本次模型预算</summary>
      <div className="filters"><label>请求次数上限<input type="number" min={1} max={limits?.research_requests ?? 1000} value={requestBudget} onChange={(e) => setRequests(Math.max(1, Number(e.target.value)))} /></label>
        <label>Token 预留上限<input type="number" min={1} max={limits?.research_tokens ?? 10000000} value={tokenBudget} onChange={(e) => setTokens(Math.max(1, Number(e.target.value)))} /></label></div>
      <p className="hint">每次请求预留输入估算与最大输出，额度不足时停止新调用。继续分配会产生新的外部请求及可能费用。</p>
      {run?.status === 'paused_budget' && <div className="actions"><Button disabled={pending || inactive || !modelReady} onClick={() => void command('continue_budget')}>分配上述预算并继续</Button><Button variant="outline" disabled={pending} onClick={() => void command('stop')}>不增加预算，停止</Button></div>}
    </details>
    {!active && <form onSubmit={(e) => { e.preventDefault(); void start() }} onKeyDown={(e) => { if (e.key === 'Enter' && e.nativeEvent.isComposing) e.preventDefault() }}>
      <fieldset disabled={pending || inactive || otherActive || !modelReady || !current.data || !session.capabilities.save_goal}>
        <label>想和导师讨论什么？<textarea rows={4} maxLength={4000} value={prompt} onChange={(e) => { setPrompt(e.target.value); drafts.set(key, e.target.value) }} /></label>
        <label className="check-label"><input type="checkbox" checked={save} disabled={!current.data?.save_available} onChange={(e) => setSave(e.target.checked)} />加密保存必要恢复正文（最多七日）</label>
        <p className="hint">{save ? '用于离开页面、断线和服务重启后恢复本次运行，可随时清除。' : '临时交流正文仅在服务器当前进程内存中，服务重启后不可恢复；正式操作回执仍会保存。'} {!current.data?.save_available && '服务器尚未配置独立正文加密密钥。'}</p>
        <p className="hint">{teachingSessionId ? '目标、本次交流、当前题目、已授权资料片段与正式学习证据' : '目标和本次交流'}将发送到配置的模型端点：{configuration.data?.effective_mentor.endpoint}。关闭页面仅停止观察；需要终止请点击“停止运行”。</p>
        <Button type="submit" disabled={!prompt.trim()}>{pending ? '正在受理…' : '发起导师交流'}</Button>
      </fieldset>
    </form>}
    {!!error && <ErrorState error={error} retry={() => { setError(undefined); void current.refetch() }} />}
  </section>
}
