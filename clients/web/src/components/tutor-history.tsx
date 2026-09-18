import { useEffect, useState } from 'react'
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { useIdentity } from '@/lib/session'
import { learningClient, unwrap } from '@/api/client'
import { checkConversation, conversationConfirmed, conversationCreated, conversationDraftKey, conversationSelectionKey, conversationsSchema, turnsSchema, type TutorConversation } from '@/api/conversations'
import { isMentorTerminal, mentorStatus } from '@/api/mentor'
import { MentorPanel } from './mentor-panel'
import { Button } from './ui/button'
import { Confirm, ErrorState } from './common'

export function TutorHistory({ spaceId, goalId, teachingSessionId, conversationId }: { spaceId: string; goalId?: string; teachingSessionId?: string; conversationId?: string }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const navigate = useNavigate()
  const [binding, setBinding] = useState<TutorConversation>()
  const contextGoal = goalId ?? (conversationId ? binding?.goal_id : undefined)
  const contextTeaching = teachingSessionId ?? (conversationId ? binding?.teaching_session_id : undefined)
  const selectionKey = conversationSelectionKey(prefix, spaceId, contextGoal, contextTeaching)
  const [selected, setSelected] = useState(conversationId ?? drafts.get<string>(selectionKey))
  const [search, setSearch] = useState('')
  const [allContexts, setAllContexts] = useState(false)
  const [save, setSave] = useState(true)
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const [notice, setNotice] = useState('')
  const header = { 'X-Learning-Space-ID': spaceId }
  const client = () => learningClient(session, spaceId)
  const listKey = [...prefix, spaceId, 'tutor-list']
  const list = useInfiniteQuery({
    queryKey: [...listKey, contextGoal, contextTeaching, search, allContexts], initialPageParam: '', gcTime: 0,
    enabled: !conversationId || !!binding,
    queryFn: async ({ pageParam, signal }) => {
      const page = await unwrap(client().GET('/v1/learning/conversations', { params: { header, query: { goal_id: contextGoal, teaching_session_id: contextTeaching, search: search || undefined, all_contexts: allContexts, cursor: pageParam || undefined, limit: 20 } }, signal }), conversationsSchema)
      page.items.forEach((c) => checkConversation(c, spaceId, session.generation))
      return page
    },
    getNextPageParam: (page) => page.next_cursor,
  })
  const select = (id: string) => {
    setSelected(id); drafts.set(selectionKey, id); setNotice('')
    if (conversationId && id !== conversationId) void navigate({ to: '/spaces/$spaceId/chat/$conversationId', params: { spaceId, conversationId: id } })
  }
  useEffect(() => {
    if (selected === undefined && !search && !allContexts && list.data?.pages[0].items[0]) select(list.data.pages[0].items[0].id)
  }, [list.data, selected, search, allContexts])
  const refresh = () => { void query.invalidateQueries({ queryKey: listKey }); void query.invalidateQueries({ queryKey: [...prefix, spaceId, 'tutor-detail'] }) }
  const create = async () => {
    if (pending) return
    setPending(true); setError(undefined)
    const payload = { goal_id: contextGoal, teaching_session_id: contextTeaching, saved: save }
    const key = selectionKey + ':new'
    try {
      const result = await unwrap(client().POST('/v1/learning/conversations', { params: { header }, body: { ...payload, id: drafts.operation(key, payload) } }), conversationCreated)
      drafts.delete(key); select(result.id); refresh()
    } catch (e) { setError(e) } finally { setPending(false) }
  }
  const remove = async (c: TutorConversation) => {
    setPending(true); setError(undefined)
    try {
      await unwrap(client().DELETE('/v1/learning/conversations/{conversationID}', { params: { header, path: { conversationID: c.id } }, body: { expected_version: c.version, confirmed: true } }), conversationConfirmed)
      drafts.delete(conversationDraftKey(prefix, c.id))
      drafts.delete(conversationDraftKey(prefix, c.id) + ':unconfirmed')
      drafts.delete(conversationDraftKey(prefix, c.id) + ':turn')
      if (selected === c.id) { setSelected(''); drafts.set(selectionKey, '') }
      query.removeQueries({ queryKey: [...prefix, spaceId, 'tutor-detail', c.id] })
      setNotice('服务器已确认删除对话；目标、内容、活动和 Evidence 保留。'); refresh()
    } catch (e) { setError(e); refresh() } finally { setPending(false) }
  }
  return <section className="tutor-history" aria-label="导师对话管理">
    <h2>导师对话</h2>
    <p className="hint">保存的聊天位于你的自托管服务器，使用服务端可解密的静态加密，不是端到端加密。提交时会把本对话历史与明确绑定的上下文发送到模型端点；仅查看和展开不发送。</p>
    <div className="filters">
      <label>搜索对话标题<input value={search} maxLength={80} onChange={(e) => setSearch(e.target.value)} /></label>
      <label className="check-label"><input type="checkbox" checked={allContexts} onChange={(e) => setAllContexts(e.target.checked)} />查看本学习区其他上下文</label>
    </div>
    {list.error && <ErrorState error={list.error} retry={() => void list.refetch()} />}
    {!list.error && <ul className="conversation-list">
      {list.data?.pages.flatMap((page) => page.items).map((c) => <li key={c.id}>
        <Button variant={selected === c.id ? 'default' : 'outline'} onClick={() => select(c.id)}>{c.title}</Button>
        <small> · {c.saved ? '保存到服务器' : '本次临时'} · {new Date(c.updated_at).toLocaleString('zh-CN')}{c.storage_state.endsWith('unavailable') || c.storage_state.endsWith('unsupported') ? ' · 无法恢复正文' : ''}</small>
        {session.capabilities.save_goal && <Confirm label={`删除对话：${c.title}`} title="删除这段导师对话？" disabled={pending} onConfirm={() => void remove(c)}>删除已保存轮次、标题和运行残留并停止运行；不会删除生成内容、归档目标或清除长期记忆。</Confirm>}
      </li>)}
    </ul>}
    {list.hasNextPage && <Button variant="outline" disabled={list.isFetchingNextPage} onClick={() => void list.fetchNextPage()}>更多对话</Button>}
    <div className="actions">
      <label className="check-label"><input type="checkbox" checked={save} onChange={(e) => setSave(e.target.checked)} />新对话保存到服务器</label>
      <Button disabled={pending || !session.capabilities.save_goal || !list.data || save && !list.data.pages[0].save_available} onClick={() => void create()}>新建对话</Button>
    </div>
    {save ? <p className="hint">已提交聊天轮次保留至主动删除；标题在服务器本地从首条文字提取，不为标题额外调用模型。</p> : <p className="hint">临时正文、标题和摘要不会持久化。当前服务进程可短暂恢复；服务重启后不可恢复，刷新后以服务器实际状态为准。已明确实施的目标、学习和内容操作仍是正式业务事实。</p>}
    {list.data && !list.data.pages[0].save_available && <p role="status">未配置历史加密密钥，可选择临时对话。</p>}
    {notice && <p role="status">{notice}</p>}
    {!!error && <ErrorState error={error} />}
    {selected && <ConversationView key={selected} spaceId={spaceId} id={selected} expanded={!!conversationId} onChange={refresh} onBinding={setBinding} />}
  </section>
}

function ConversationView({ spaceId, id, expanded, onChange, onBinding }: { spaceId: string; id: string; expanded: boolean; onChange: () => void; onBinding: (c: TutorConversation) => void }) {
  const { session, prefix, drafts } = useIdentity()
  const [title, setTitle] = useState('')
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const [after, setAfter] = useState(0)
  const header = { 'X-Learning-Space-ID': spaceId }
  const detail = useQuery({
    queryKey: [...prefix, spaceId, 'tutor-detail', id, after], gcTime: 0, refetchInterval: 5000,
    queryFn: async ({ signal }) => {
      const page = await unwrap(learningClient(session, spaceId).GET('/v1/learning/conversations/{conversationID}', { params: { header, path: { conversationID: id }, query: { after, limit: 20 } }, signal }), turnsSchema)
      checkConversation(page.conversation, spaceId, session.generation, id)
      return page
    },
  })
  const c = !detail.error ? detail.data?.conversation : undefined
  useEffect(() => {
    if (!c) return
    drafts.set(conversationSelectionKey(prefix, c.space_id, c.goal_id, c.teaching_session_id), c.id)
    onBinding(c)
  }, [c])
  const rename = async () => {
    if (!c || !title.trim()) return
    setPending(true); setError(undefined)
    try {
      await unwrap(learningClient(session, spaceId).PATCH('/v1/learning/conversations/{conversationID}', { params: { header, path: { conversationID: id } }, body: { expected_version: c.version, title } }), conversationConfirmed)
      setTitle(''); onChange()
    } catch (e) { setError(e); onChange() } finally { setPending(false) }
  }
  if (detail.error) return <ErrorState error={detail.error} retry={() => void detail.refetch()} />
  if (!c) return <p role="status">正在向服务器核对历史…</p>
  return <section aria-label="当前导师对话">
    <h3>{c.title}</h3>
    <p>{c.saved ? '保存到服务器' : '本次临时'} · 学习区 {c.space_id} · {c.goal_id ? `目标 ${c.goal_id}` : '未绑定目标'}{c.teaching_session_id && ` · 课堂 ${c.teaching_session_id}`}</p>
    {!expanded && <Link to="/spaces/$spaceId/chat/$conversationId" params={{ spaceId, conversationId: id }}>展开导师对话</Link>}
    {expanded && c.teaching_session_id && <Link to="/spaces/$spaceId/learn/$sessionId" params={{ spaceId, sessionId: c.teaching_session_id }}>返回原课堂侧栏</Link>}
    {expanded && c.goal_id && !c.teaching_session_id && <Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId: c.goal_id }}>返回原目标</Link>}
    {c.saved && session.capabilities.save_goal && <form onSubmit={(e) => { e.preventDefault(); void rename() }}>
      <label>对话新名称<input value={title} onChange={(e) => setTitle(e.target.value)} maxLength={80} /></label>
      <Button type="submit" variant="outline" disabled={pending || !title.trim()}>重命名对话</Button>
    </form>}
    {!!error && <ErrorState error={error} />}
    <div aria-label="已提交聊天轮次">
      {detail.data?.items.map((turn) => <article key={turn.run_id}>
        <p>第 {turn.ordinal} 轮 · {mentorStatus[turn.status as keyof typeof mentorStatus] ?? turn.status}</p>
        {!turn.body_available ? <p>该轮正文未保存或已清除，不能恢复。</p> : <>
          {turn.messages.filter((m) => m.role === 'user').map((m, i) => <p className="conversation-message" key={i}>你：{m.content}</p>)}
          {(turn.run_id !== c.current_run_id || isMentorTerminal(turn.status as Parameters<typeof isMentorTerminal>[0])) && turn.output && <p className="conversation-message">导师：{turn.output}</p>}
          {turn.messages.some((m) => m.role === 'tool') && <details><summary>本轮工具与来源记录（只读）</summary>{turn.messages.filter((m) => m.role === 'tool').map((m, i) => <pre key={i}>{m.tool_call_id}：{m.content}</pre>)}</details>}
        </>}
      </article>)}
    </div>
    <div className="actions">{after > 0 && <Button variant="outline" onClick={() => setAfter(0)}>最早轮次</Button>}{detail.data?.next_cursor && <Button variant="outline" onClick={() => setAfter(detail.data!.next_cursor!)}>后续轮次</Button>}</div>
    {c.storage_state === 'temporary_unavailable' && <p role="alert">临时正文无法恢复。请新建对话，正式学习事实仍然保留。</p>}
    <MentorPanel goal={{ learning_space_id: c.space_id, goal_id: c.goal_id ?? '00000000-0000-0000-0000-000000000000', revision: c.goal_version, management: { status: c.writable ? 'active' : 'paused' } }} archivedSpace={!c.writable} teachingSessionId={c.teaching_session_id} conversation={c} onChange={onChange} />
  </section>
}
