import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { conversationDraftKey, type TutorConversation, type TurnSubmission } from '@/api/conversations'
import { settingsSchema } from '@/api/settings'
import { isMentorTerminal, mentorCurrentSchema, mentorReceiptSchema, mentorSnapshotSchema, mentorStatus, observeMentor, type MentorSnapshot } from '@/api/mentor'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { Confirm, ErrorState } from './common'
import { ReferenceLink } from '@/knowledge-page'
import { CandidateReview } from '@/memory-page'

export function MentorPanel({ goal, archivedSpace, teachingSessionId, conversation, onChange }: { goal: { learning_space_id: string; goal_id: string; revision: number; management: { status: string } }; archivedSpace: boolean; teachingSessionId?: string; conversation?: TutorConversation; onChange?: () => void }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const key = conversation ? conversationDraftKey(prefix, conversation.id) : JSON.stringify([...prefix, goal.learning_space_id, goal.goal_id, teachingSessionId, 'mentor'])
  const [prompt, setPrompt] = useState(() => drafts.get<string>(key) ?? '')
  const [save, setSave] = useState(false)
  const [requests, setRequests] = useState(3)
  const [tokens, setTokens] = useState(30000)
  const [run, setRun] = useState<MentorSnapshot>()
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const [answer, setAnswer] = useState('')
  const [confirmedDestination, setConfirmedDestination] = useState('')
  const [unconfirmed, setUnconfirmed] = useState(() => drafts.get<TurnSubmission>(key + ':unconfirmed'))
  const lastTerminal = useRef('')
  const sessionId = useRef<string>(crypto.randomUUID())
  const mounted = useRef(true)
  const output = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  const header = { 'X-Learning-Space-ID': goal.learning_space_id }
  const client = () => learningClient(session, goal.learning_space_id)
  const current = useQuery({
    queryKey: [...prefix, goal.learning_space_id, goal.goal_id, 'mentor-current', conversation?.id, conversation?.current_run_id],
    queryFn: async ({ signal }) => {
      if (conversation) return { run: conversation.current_run_id ? await unwrap(client().GET('/v1/learning/runs/{runID}', { params: { path: { runID: conversation.current_run_id }, header }, signal }), mentorSnapshotSchema) : null, save_available: conversation.saved }
      return unwrap(client().GET('/v1/learning/goals/{goalID}/runs', { params: { path: { goalID: goal.goal_id }, header }, signal }), mentorCurrentSchema)
    },
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
      if (next.goal_id === goal.goal_id && next.space_id === goal.learning_space_id && next.privacy_generation === session.generation && (!conversation || next.session_id === conversation.id && next.conversation_id === conversation.id)) {
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
      if (conversation && isMentorTerminal(next.status) && lastTerminal.current !== `${next.run_id}:${next.version}`) { lastTerminal.current = `${next.run_id}:${next.version}`; onChange?.() }
      if (!conversation) query.setQueryData([...prefix, goal.learning_space_id, goal.goal_id, 'mentor-current'], { run: next, save_available: current.data?.save_available ?? false })
    })
  }, [run?.run_id, session, key])
  useEffect(() => { if (follow.current && output.current) output.current.scrollTop = output.current.scrollHeight }, [run?.output])
  useEffect(() => { setAnswer('') }, [run?.interaction?.id])
  const limits = configuration.data?.limits
  const requestBudget = Math.min(requests, limits?.research_requests ?? requests)
  const tokenBudget = Math.min(tokens, limits?.research_tokens ?? tokens)
  const inactive = archivedSpace || !['draft', 'active'].includes(goal.management.status)
  const destinationBlocked = !!conversation?.confirmation_required && confirmedDestination !== conversation.destination
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
      const turn = conversation ? { expected_version: conversation.version, prompt, request_budget: requestBudget, token_budget: tokenBudget, ...(confirmedDestination ? { confirm_destination: confirmedDestination } : {}) } : undefined
      const submission = turn ? (unconfirmed ?? { ...turn, operation_id: drafts.operation(key + ':turn', turn) }) : undefined
      if (submission) { drafts.set(key + ':unconfirmed', submission); setUnconfirmed(submission) }
      const receipt = conversation && turn
        ? await unwrap(client().POST('/v1/learning/conversations/{conversationID}/turns', { params: { path: { conversationID: conversation.id }, header }, body: submission! }), mentorReceiptSchema)
        : await unwrap(client().POST('/v1/learning/goals/{goalID}/runs', { params: { path: { goalID: goal.goal_id }, header }, body: { ...payload, operation_id } }), mentorReceiptSchema)
      await refresh(receipt.run_id)
      drafts.delete(key)
      drafts.delete(key + ':unconfirmed')
      if (mounted.current) { setPrompt(''); setUnconfirmed(undefined); setConfirmedDestination(''); onChange?.() }
    } catch (e) {
      if (e instanceof ApiError && e.status < 500) { drafts.delete(key + ':unconfirmed'); if (mounted.current) setUnconfirmed(undefined) }
      if (mounted.current) setError(e)
    }
    finally { if (mounted.current) setPending(false) }
  }
  const reconcile = async () => {
    if (!unconfirmed || pending) return
    setPending(true); setError(undefined)
    try {
      const receipt = await unwrap(client().GET('/v1/learning/operations/{operationID}', { params: { path: { operationID: unconfirmed.operation_id }, header } }), mentorReceiptSchema)
      if (conversation && receipt.session_id !== conversation.id) throw new ApiError(502, 'invalid_response')
      await refresh(receipt.run_id)
      drafts.delete(key); drafts.delete(key + ':unconfirmed')
      if (mounted.current) { setUnconfirmed(undefined); setPrompt(''); onChange?.() }
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
      if (mounted.current) { setAnswer(''); onChange?.() }
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
    {unconfirmed && <div role="alert"><p>上一轮提交结果尚待服务器核对。重试沿用原操作身份，不创建第二次运行。</p><Button variant="outline" disabled={pending} onClick={() => void reconcile()}>核对提交结果</Button><Button variant="outline" disabled={pending} onClick={() => void start()}>重试原提交</Button></div>}
    {conversation?.confirmation_required && <fieldset>
      <legend>模型目的地已变化，当前仅查看</legend>
      <p>原目的地：{conversation.provider} · {conversation.endpoint || '未配置'}。新目的地：{conversation.destination_provider} · {conversation.destination_endpoint || '未配置'}。</p>
      <p>继续将发送本对话已提交文字、完整工具结果及必要来源引用，以及原绑定上下文。取消后仍可在此查看已保存历史。</p>
      <label className="check-label"><input type="checkbox" checked={confirmedDestination === conversation.destination} onChange={(e) => setConfirmedDestination(e.target.checked ? conversation.destination : '')} />同意将上述历史发送到此目的地</label>
      <Button variant="outline" onClick={() => setConfirmedDestination('')}>取消外发，仅查看</Button>
    </fieldset>}
    {run && !current.error && <>
      <p role="status">{mentorStatus[run.status]} · 最近活动：{new Date(run.updated_at).toLocaleTimeString('zh-CN')}</p>
      <div className="mentor-output" ref={output} onScroll={() => { const element = output.current; if (element) follow.current = element.scrollHeight - element.scrollTop - element.clientHeight < 40 }} aria-label="导师输出">
        {run.output || (run.body_available ? '等待导师输出…' : '正文已清除、到期，或临时交流不能在此进程恢复。')}
      </div>
      {run.result_unknown && <p role="alert">请求可能已到达模型服务，但结果无法确认；不会自动重发。</p>}
      {run.reason === 'history_context_limit' && <p role="alert">对话或工具上下文达到上限，本次未继续请求。已有历史保留，请新建对话。</p>}
      {run.cost_unknown && <p className="hint">已发出 {run.requests_used} 次模型请求，实际费用未核实。</p>}
      <p className="hint">剩余预算：{run.requests_left} 次请求 / {run.tokens_left} Token 预留额度。</p>
      {run.saved && run.body_available && <p className="hint">{conversation ? '已提交轮次保留至主动删除；' : ''}本次运行恢复缓存最晚保留至 {new Date(run.expires_at).toLocaleString('zh-CN')}。</p>}
      {run.interaction && <fieldset disabled={pending || inactive || !!conversation?.confirmation_required}>
        <legend>{run.interaction.memory_candidate_id ? '长期记忆申请：等待具体审批' : run.interaction.approval ? '确认交流方向' : '导师需要你的回答'}</legend>
        <p>{run.interaction.question}</p>
        {run.interaction.reference_selection && <ReferenceLink spaceId={goal.learning_space_id} goalId={goal.goal_id} sessionId={teachingSessionId} />}
        {run.interaction.memory_candidate_id && <CandidateReview key={run.interaction.memory_candidate_id} candidateId={run.interaction.memory_candidate_id} />}
        {run.interaction.choices.length > 0 ? <div className="actions">{run.interaction.choices.map((choice) => <Button key={choice} variant="outline" onClick={() => void command('respond', choice)}>{choice}</Button>)}</div> : <form onSubmit={(e) => { e.preventDefault(); void command('respond') }} onKeyDown={(e) => { if (e.key === 'Enter' && e.nativeEvent.isComposing) e.preventDefault() }}>
          <label>回应导师<textarea value={answer} onChange={(e) => setAnswer(e.target.value)} maxLength={2000} rows={3} /></label>
          <Button type="submit" disabled={!answer.trim()}>提交回答</Button>
        </form>}
      </fieldset>}
      {run.memory_status && <details><summary>个性化使用的来源与范围</summary>
        <p>{run.memory_status === 'not_authorized' ? '未授权读取长期记忆。' : run.memory_status === 'unavailable' ? '长期记忆不可用，本轮没有使用。' : run.memory_status === 'partial' ? '仅读取部分可用信息；其他记录未使用。' : '本轮读取获准的全局长期信息。'} 学习事实使用本次绑定学习区的正式证据；Nocturne 不作为掌握度真值。</p>
        <ul>{run.memory_sources?.map(source => <li key={source.memory_id}><a href={`/app/memory?record=${source.memory_id}`}>记忆来源 · 版本 {source.revision}</a><p>{source.scope}</p></li>)}</ul>
      </details>}
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
      {run?.status === 'paused_budget' && <div className="actions"><Button disabled={pending || inactive || !modelReady || !!conversation?.confirmation_required} onClick={() => void command('continue_budget')}>分配上述预算并继续</Button><Button variant="outline" disabled={pending} onClick={() => void command('stop')}>不增加预算，停止</Button></div>}
    </details>
    {!active && <form onSubmit={(e) => { e.preventDefault(); void start() }} onKeyDown={(e) => { if (e.key === 'Enter' && e.nativeEvent.isComposing) e.preventDefault() }}>
      <fieldset disabled={pending || inactive || destinationBlocked || otherActive || !modelReady || !current.data || !session.capabilities.save_goal}>
        <label>想和导师讨论什么？<textarea rows={4} maxLength={4000} disabled={!!unconfirmed} value={prompt} onChange={(e) => { setPrompt(e.target.value); drafts.set(key, e.target.value) }} /></label>
        {!conversation && <><label className="check-label"><input type="checkbox" checked={save} disabled={!current.data?.save_available} onChange={(e) => setSave(e.target.checked)} />加密保存必要恢复正文（最多七日）</label>
        <p className="hint">{save ? '用于离开页面、断线和服务重启后恢复本次运行，可随时清除。' : '临时交流正文仅在服务器当前进程内存中，服务重启后不可恢复；正式操作回执仍会保存。'} {!current.data?.save_available && '服务器尚未配置独立正文加密密钥。'}</p></>}
        <p className="hint">{teachingSessionId ? '目标、本次交流、当前题目、已授权资料片段与正式学习证据' : '目标和本次交流'}将发送到配置的模型端点：{configuration.data?.effective_mentor.endpoint}。关闭页面仅停止观察；需要终止请点击“停止运行”。</p>
        <Button type="submit" disabled={!prompt.trim() || !!unconfirmed}>{pending ? '正在受理…' : '发起导师交流'}</Button>
      </fieldset>
    </form>}
    {!!error && <ErrorState error={error} retry={() => { setError(undefined); void current.refetch() }} />}
  </section>
}
