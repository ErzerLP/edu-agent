import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { z } from 'zod'
import { learningClient, unwrap, ApiError } from './api/client'
import { goalSchema, spaceSchema, type Goal } from './api/runtime'
import { mentorCurrentSchema, mentorSnapshotSchema, mentorReceiptSchema, isMentorTerminal, observeMentor, type MentorSnapshot } from './api/mentor'
import { researchStage, sourceStatus, sourceSchema, type ResearchSource } from './api/research'
import { settingsSchema } from './api/settings'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { ErrorState, Confirm } from './components/common'

export function ResearchPage({ spaceId, goalId }: { spaceId: string; goalId: string }) {
  const { session, prefix } = useIdentity()
  const goal = useQuery({ queryKey: [...prefix, spaceId, goalId, 'latest'], queryFn: ({ signal }) => unwrap(learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}', { params: { path: { goalID: goalId } }, signal }), goalSchema) })
  const space = useQuery({ queryKey: [...prefix, spaceId, 'space'], queryFn: ({ signal }) => unwrap(learningClient(session).GET('/v1/learning-spaces/{spaceID}', { params: { path: { spaceID: spaceId } }, signal }), spaceSchema) })
  if (goal.error || space.error) return <ErrorState error={goal.error || space.error} />
  if (!goal.data || !space.data) return <p role="status">正在读取目标…</p>
  if (goal.data.learning_space_id !== spaceId || goal.data.goal_id !== goalId) return <ErrorState error={new ApiError(404, 'wrong_space')} />
  return <><Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId }}>← 返回目标</Link><h1>研究相关知识</h1><p>{goal.data.management.details.name}</p><ResearchPanel goal={goal.data} archived={space.data.status === 'archived'} /></>
}

function SourceViewer({ source, select, decide, disabled }: { source: ResearchSource; select?: string; decide: (kind: 'adopt' | 'reject') => void; disabled: boolean }) {
  return <article className="panel" id={`source-${source.id}`}>
    <h3>{source.title}</h3><p>{sourceStatus[source.status]}</p>
    <p className="hint">原始来源：{source.locator}</p>
    {source.final_url && <p className="hint">实际读取：{source.final_url}</p>}
    {source.fetched_at && <p>获取于 {new Date(source.fetched_at).toLocaleString('zh-CN')} · {source.coverage === 'complete_text' ? '完整文本' : '部分解析，可能有遗漏'}</p>}
    {source.failure && <p role="alert">缺口：{source.failure}。未以搜索摘要替代正文。</p>}
    {source.text ? <>
      <p className="hint">这是获准保存的历史文本副本；网页可能已改变。用途：本目标参考，不代表结论已证实。</p>
      {source.fragments.map((fragment, index) => <details key={fragment.id} open={select === fragment.id || undefined} id={`fragment-${fragment.id}`}><summary>原文片段 {index + 1}（字节 {fragment.start}–{fragment.end}）</summary><blockquote style={{ whiteSpace: 'pre-wrap' }}>{fragment.text}</blockquote></details>)}
      <details><summary>查看上下文与版本</summary><p>修订：{source.revision_id}</p><p>指纹：{source.fingerprint}</p><p>解析器：{source.parser}</p><pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{source.text}</pre></details>
    </> : <p>没有可显示的原文副本。</p>}
    {['parsed', 'partial'].includes(source.status) && <div className="actions"><Button disabled={disabled} onClick={() => decide('adopt')}>纳入本目标参考</Button><Button variant="outline" disabled={disabled} onClick={() => decide('reject')}>拒绝此来源</Button></div>}
  </article>
}

function ResearchPanel({ goal, archived }: { goal: Goal; archived: boolean }) {
  const { session, prefix, drafts } = useIdentity()
  const key = JSON.stringify([...prefix, goal.learning_space_id, goal.goal_id, 'research'])
  const [topic, setTopic] = useState(() => drafts.get<string>(key) ?? '')
  const [consent, setConsent] = useState(false)
  const [autoAdopt, setAutoAdopt] = useState(false)
  const [save, setSave] = useState(false)
  const [mode, setMode] = useState<'supplement' | 'prefer' | 'restrict'>('supplement')
  const [domains, setDomains] = useState('')
  const [requests, setRequests] = useState(10)
  const [tokens, setTokens] = useState(30000)
  const [run, setRun] = useState<MentorSnapshot>()
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const [selected, setSelected] = useState<string>()
  const sessionID = useRef<string>(crypto.randomUUID())
  const mounted = useRef(true)
  const header = { 'X-Learning-Space-ID': goal.learning_space_id }
  const client = () => learningClient(session, goal.learning_space_id)
  const current = useQuery({ queryKey: [...prefix, goal.learning_space_id, goal.goal_id, 'research-current'], queryFn: ({ signal }) => unwrap(client().GET('/v1/learning/goals/{goalID}/research', { params: { path: { goalID: goal.goal_id }, header }, signal }), mentorCurrentSchema), gcTime: 0 })
  const configuration = useQuery({ queryKey: [...prefix, 'settings'], queryFn: ({ signal }) => unwrap(learningClient(session).GET('/v1/settings', { signal }), settingsSchema) })
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  useEffect(() => { if (current.data?.run) { setRun(current.data.run); sessionID.current = current.data.run.session_id } }, [current.data])
  useEffect(() => { if (run) return observeMentor(session, run, (next, failure) => { setRun(next); setError(failure) }) }, [run?.run_id, session, key])
  const ready = configuration.data?.search.enabled && configuration.data.search.configured && configuration.data.effective_mentor.enabled && configuration.data.effective_mentor.configured
  const inactive = archived || !['draft', 'active'].includes(goal.management.status)
  const active = run && !isMentorTerminal(run.status)
  const limits = configuration.data?.limits
  const requestBudget = Math.min(requests, limits?.research_requests ?? requests)
  const tokenBudget = Math.min(tokens, limits?.research_tokens ?? tokens)
  const refresh = async (id: string) => { const result = await unwrap(client().GET('/v1/learning/runs/{runID}', { params: { path: { runID: id }, header } }), mentorSnapshotSchema); if (mounted.current) setRun(result) }
  const act = async (fn: () => Promise<void>) => { setPending(true); setError(undefined); try { await fn() } catch (e) { if (mounted.current) setError(e) } finally { if (mounted.current) setPending(false) } }
  const start = () => act(async () => {
    const payload = { session_id: run?.session_id ?? sessionID.current, expected_version: goal.revision, prompt: topic, save, request_budget: requestBudget, token_budget: tokenBudget, research: { topic, external_consent: true as const, auto_adopt: autoAdopt, policy: { mode, domains: domains.split(/[\s,，]+/).filter(Boolean) } } }
    const operation_id = drafts.operation(key + ':create', payload)
    const receipt = await unwrap(client().POST('/v1/learning/goals/{goalID}/runs', { params: { path: { goalID: goal.goal_id }, header }, body: { ...payload, operation_id } }), mentorReceiptSchema)
    await refresh(receipt.run_id)
  })
  const command = (kind: 'stop' | 'clear' | 'continue_budget') => act(async () => {
    if (!run) return
    const payload = { expected_version: run.version, kind, ...(kind === 'continue_budget' ? { request_budget: requestBudget, token_budget: tokenBudget } : {}) }
    await unwrap(client().POST('/v1/learning/runs/{runID}/commands', { params: { path: { runID: run.run_id }, header }, body: { ...payload, operation_id: drafts.operation(key + ':command', payload) } }), mentorReceiptSchema)
    await refresh(run.run_id)
  })
  const decide = (source: ResearchSource, kind: 'adopt' | 'reject') => act(async () => {
    if (!run) return
    const payload = { expected_version: run.version, kind }
    await unwrap(client().POST('/v1/learning/runs/{runID}/sources/{sourceID}/decisions', { params: { path: { runID: run.run_id, sourceID: source.id }, header }, body: { ...payload, operation_id: drafts.operation(key + ':source:' + source.id, payload) } }), mentorReceiptSchema)
    await refresh(run.run_id)
  })
  const state = run?.research
  const historical = useQuery({
    queryKey: [...prefix, goal.learning_space_id, goal.goal_id, run?.run_id, run?.watermark, 'historical-sources'],
    enabled: !!run && !state && run.kind === 'research', gcTime: 0,
    queryFn: async ({ signal }) => {
      const path = { runID: run!.run_id }
      const page = await unwrap(client().GET('/v1/learning/runs/{runID}/sources', { params: { path, header }, signal }), z.object({ items: z.array(sourceSchema), next_cursor: z.string() }))
      return Promise.all(page.items.map((source) => unwrap(client().GET('/v1/learning/runs/{runID}/sources/{sourceID}', { params: { path: { ...path, sourceID: source.id }, header }, signal }), sourceSchema)))
    },
  })
  const sources = state?.sources ?? (historical.error ? [] : historical.data ?? [])
  return <>
    <p>只将你确认的公开主题发送给搜索和模型服务。请去掉姓名、成绩、身份和私人原文；不会自动发送目标内容。研究不启动正式教学。</p>
    {(error || current.error || configuration.error) && <ErrorState error={error || current.error || configuration.error} retry={() => { void current.refetch(); void configuration.refetch() }} />}
    {historical.error && <ErrorState error={historical.error} retry={() => void historical.refetch()} />}
    {!ready && <p>请先配置并启用搜索和 Web 导师。</p>}
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (consent && !pending) void start() }}>
      <fieldset disabled={pending || !!active || inactive}>
        <legend>拟定公开查询</legend>
        <label>公开研究主题<input maxLength={100} value={topic} onChange={(e) => { setTopic(e.target.value); drafts.set(key, e.target.value) }} required /></label>
        <label>来源范围<select value={mode} onChange={(e) => setMode(e.target.value as typeof mode)}><option value="supplement">公开来源补充</option><option value="prefer">优先指定网站</option><option value="restrict">仅限指定网站</option></select></label>
        {mode !== 'supplement' && <label>网站域名（最多 5 个，用逗号分隔）<input value={domains} onChange={(e) => setDomains(e.target.value)} placeholder="example.org" required /></label>}
        <label><input type="checkbox" checked={consent} onChange={(e) => setConsent(e.target.checked)} />确认以上主题可外发，允许搜索、读取公共网页和模型整理，可能消耗额度</label>
        <label><input type="checkbox" checked={autoAdopt} disabled={!session.device.scopes.includes('knowledge:write')} onChange={(e) => setAutoAdopt(e.target.checked)} />研究并将有引用的新来源纳入本目标参考</label>
        <label><input type="checkbox" checked={save} disabled={!current.data?.save_available} onChange={(e) => setSave(e.target.checked)} />加密保存运行七天，支持重启恢复；未勾选仅保留当前进程</label>
        <Button disabled={!ready || !consent || !topic.trim()} type="submit">{run ? '重新研究' : '研究相关知识'}</Button>
      </fieldset>
    </form>
    <div className="filters"><label>外部请求预算<input type="number" min={1} max={limits?.research_requests ?? 1000} value={requestBudget} onChange={(e) => setRequests(Math.max(1, Number(e.target.value)))} /></label><label>Token 预算<input type="number" min={1} max={limits?.research_tokens ?? 10000000} value={tokenBudget} onChange={(e) => setTokens(Math.max(1, Number(e.target.value)))} /></label></div>
    {run && <section className="panel"><p role="status">{researchStage[run.stage] ?? run.stage} · {run.status === 'partial' ? '有缺口，未完整完成' : run.status === 'succeeded' ? '结果可查看' : ''}</p><p>已发出 {run.requests_used} 次外部请求，剩余 {run.requests_left} 次 / {run.tokens_left} Token。实际费用未核实。</p>
      {run.reason && <p>运行说明：{run.reason}</p>}{run.result_unknown && <p role="alert">在途请求的结果未知，没有自动重发。</p>}
      {!run.body_available && <p>正文已清除、到期或临时副本不可恢复。</p>}
      {active && <Button disabled={pending} variant="outline" onClick={() => void command('stop')}>停止研究</Button>}
      {run.status === 'paused_budget' && <Button disabled={pending || inactive} onClick={() => void command('continue_budget')}>按以上预算继续</Button>}
      {run.body_available && <Confirm label="清除研究运行" title="清除本次研究运行？" disabled={pending} onConfirm={() => void command('clear')}>候选正文、来源缓存和综合结果将清除。已经正式采纳的参考属于知识库，遵循知识库清除流程。</Confirm>}
    </section>}
    {state && <>
      <p>候选 {state.sources.length} · 已读取 {state.sources.filter((s) => s.fetched_at).length} · 部分解析 {state.sources.filter((s) => s.coverage && s.coverage !== 'complete_text').length} · 已采用 {state.sources.filter((s) => s.status === 'adopted').length}</p>
      <section><h2>AI 综合要点</h2><p className="hint">以下是 AI 综合，目前只核对每个来源的前三个片段，存在阅读遗漏。引用仅表示可追溯，不等于结论普遍正确。</p>
        {state.synthesis?.points.map((point, i) => <article className="panel" key={i}><p>{point.text}</p>{point.citations.map((citation, j) => <p key={j}><a href={`#fragment-${citation.fragment_id}`} onClick={() => setSelected(citation.fragment_id)}>查看支持片段</a>：{citation.quote}</p>)}</article>)}
        {state.synthesis?.gaps.map((gap, i) => <p key={i}>缺口或冲突：{gap}</p>)}
        {state.synthesis?.examples.map((example, i) => <p key={i}>自拟例子（非外部原文）：{example}</p>)}
      </section>
    </>}
    {sources.length > 0 && <><h2>{state ? '实际来源' : '已获准保留的历史来源'}</h2>{sources.map((source) => <SourceViewer key={source.id} source={source} select={selected} disabled={pending || !!active || inactive || !session.device.scopes.includes('knowledge:write')} decide={(kind) => void decide(source, kind)} />)}</>}
  </>
}
