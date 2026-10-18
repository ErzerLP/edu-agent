import { useEffect, useId, useRef, useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { List, X } from 'lucide-react'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { topicAnchorPageSchema, topicPageSchema, type MessageAnchor } from '@/api/conversation-topics'
import { mentorReceiptSchema } from '@/api/mentor'
import type { TutorConversation } from '@/api/conversations'
import type { Space } from '@/api/runtime'
import { useIdentity } from '@/lib/session'
import { useSourceSelection } from './source-selection'
import { Button } from '../ui/button'
import { ErrorState } from '../common'

const states = {
  understood_in_chat: '对话中表示理解', question_open: '仍有疑问',
  exercise_error_observed: '讨论过练习中的错误', unassessed: '尚未评估',
}
export function ConversationTopics({ conversation, space, canWrite, onLocate }: {
  conversation: TutorConversation; space: Space; canWrite: boolean
  onLocate: (anchor: MessageAnchor) => Promise<void>
}) {
  const { session, prefix, drafts } = useIdentity()
  const { selection } = useSourceSelection()
  const [open, setOpen] = useState(false)
  const [selected, setSelected] = useState<string>()
  const [anchor, setAnchor] = useState<MessageAnchor>()
  const [position, setPosition] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [destination, setDestination] = useState('')
  const trigger = useRef<HTMLButtonElement>(null)
  const panel = useRef<HTMLDivElement>(null)
  const alive = useRef(true)
  const panelId = useId()
  const id = conversation.id
  const header = { 'X-Learning-Space-ID': space.id }
  const api = () => learningClient(session, space.id)
  const cacheKey = [...prefix, space.id, 'conversation-topics', id]
  const data = useInfiniteQuery({
    queryKey: cacheKey, enabled: open || !!selected, retry: false, gcTime: 0,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam, signal }) => {
      const page = await unwrap(api().GET('/v1/learning/conversations/{conversationID}/topics', {
        params: { header, path: { conversationID: id }, query: { cursor: pageParam, limit: 20 } }, signal,
      }), topicPageSchema)
      if (page.conversation_id !== id || page.privacy_generation !== session.generation) throw new ApiError(502, 'invalid_response')
      return page
    },
    getNextPageParam: page => page.next_cursor,
    refetchInterval: 1500,
  })
  const head = data.data?.pages[0]
  const topics = data.error ? [] : data.data?.pages.flatMap(page => page.items) ?? []
  const topic = topics.find(item => item.id === selected)
  const indexing = head?.status === 'queued' || head?.status === 'running'
  const operationKey = JSON.stringify([...cacheKey, 'organize'])
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  useEffect(() => {
    if (open) panel.current?.querySelector<HTMLButtonElement>('[data-topic],button')?.focus()
  }, [open])
  const close = () => { setOpen(false); trigger.current?.focus() }
  const organize = async () => {
    if (!head || busy || !selection) return
    setBusy(true); setError(undefined)
    const payload = {
      expected_index_revision: head.revision, privacy_generation: session.generation,
      direction_version: space.direction_version!, source_selection: selection,
      ...(destination ? { confirm_destination: destination } : {}),
    }
    try {
      await unwrap(api().POST('/v1/learning/conversations/{conversationID}/topic-index', {
        params: { header, path: { conversationID: id } },
        body: { ...payload, operation_id: drafts.operation(operationKey, payload) },
      }), mentorReceiptSchema)
      if (!alive.current) return
      drafts.delete(operationKey)
      await data.refetch()
    } catch (e) {
      if (alive.current) { setError(e); void data.refetch() }
    } finally { if (alive.current) setBusy(false) }
  }
  const navigate = async (topicId: string, direction: 'first' | 'previous' | 'next') => {
    if (busy) return
    setBusy(true); setError(undefined)
    try {
      const page = await unwrap(api().GET('/v1/learning/conversations/{conversationID}/topics/{topicID}/anchors', {
        params: { header, path: { conversationID: id, topicID: topicId }, query: {
          limit: 1, cursor: direction === 'first' ? 0 : anchor!.ordinal, before: direction === 'previous',
        } },
      }), topicAnchorPageSchema)
      if (page.conversation_id !== id || page.privacy_generation !== session.generation) throw new ApiError(502, 'invalid_response')
      if (!page.items.length) throw new ApiError(404, 'topic_position_unavailable')
      if (!alive.current) return
      await onLocate(page.items[0])
      if (!alive.current) return
      setSelected(topicId); setAnchor(page.items[0])
      setPosition(direction === 'first' ? 1 : position + (direction === 'next' ? 1 : -1))
      setOpen(false)
    } catch (e) { if (alive.current) setError(e) }
    finally { if (alive.current) setBusy(false) }
  }
  return <div className="chat-topics">
    <Button variant="ghost" ref={trigger} aria-label="查看话题" aria-expanded={open} aria-controls={open ? panelId : undefined}
      onClick={() => setOpen(!open)}><List size={17} aria-hidden="true" /><span>话题</span></Button>
    {open && <div id={panelId} className="chat-topics-panel" role="region" aria-label="对话话题" ref={panel}
      onKeyDown={event => {
        if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close() }
        if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key) && (event.target as HTMLElement).hasAttribute('data-topic')) {
          event.preventDefault()
          const buttons = Array.from(panel.current!.querySelectorAll<HTMLButtonElement>('[data-topic]'))
          const index = buttons.indexOf(document.activeElement as HTMLButtonElement)
          const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (index + (event.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length
          buttons[next]?.focus()
        }
      }}>
      <div className="chat-topics-heading"><strong>对话话题</strong><Button variant="ghost" aria-label="关闭话题列表" onClick={close}><X size={16} /></Button></div>
      {data.error ? <ErrorState error={data.error} retry={() => void data.refetch()} /> : !head ? <p role="status">正在读取话题…</p> : <>
        <p role="status">{indexing ? '正在整理话题…' : head.status === 'failed' ? '暂时未能整理，已有话题仍可查看。' : head.status === 'not_started' ? '尚未整理话题' : '已有话题'}
          {head.total_messages > 0 && ` · 已整理 ${head.covered_messages} / ${head.total_messages} 条消息`}
          {head.unmapped_turns > 0 && ` · 还有 ${head.unmapped_turns} 轮旧对话待整理`}</p>
        {!indexing && head.covered_messages < head.total_messages && <p>仍有消息未整理。仅整理本次所选来源和学习方向对应的内容。</p>}
        <p className="chat-topics-hint">理解状态仅反映对话中的观察。练习结果以学习记录为准。</p>
        <ul className="chat-topics-list">{topics.map(item => <li key={item.id}>
          <button type="button" data-topic disabled={busy || item.anchor_count === 0} onClick={() => void navigate(item.id, 'first')}>
            <strong>{item.title}</strong><span>{item.anchor_count} 处</span>
          </button>
          <ul>{item.points.map((point, i) => <li key={i}>{point.text}</li>)}</ul>
          {item.observations.slice(-3).map((observation, i) => <p key={i}>
            <span>{states[observation.state]}</span> · {observation.evidence === 'user_report' ? '用户自述' : '对话观察'}：{observation.text}
          </p>)}
        </li>)}</ul>
        {!topics.length && head.status === 'ready' && <p>本次范围内还没有可显示的话题。</p>}
        {data.hasNextPage && <Button variant="ghost" disabled={data.isFetchingNextPage} onClick={() => void data.fetchNextPage()}>更多话题</Button>}
        {conversation.confirmation_required && <label><input type="checkbox" checked={destination === conversation.destination} onChange={e => setDestination(e.target.checked ? conversation.destination : '')} />
          同意将本次范围内的对话发送到 {conversation.destination_provider}（{conversation.destination_endpoint}）整理话题</label>}
        <Button disabled={!canWrite || !selection || !space.direction_version || busy || indexing || conversation.confirmation_required && destination !== conversation.destination}
          onClick={() => void organize()}>{indexing ? '正在整理…' : '整理话题'}</Button>
      </>}
      {error ? <ErrorState error={error} /> : null}
    </div>}
    {topic && anchor && <nav className="chat-topic-navigation" aria-label="话题位置">
      <span>{topic.title} · 第 {position} / {topic.anchor_count} 处</span>
      <Button variant="ghost" disabled={busy || position <= 1} onClick={() => void navigate(topic.id, 'previous')}>上一处</Button>
      <Button variant="ghost" disabled={busy || position >= topic.anchor_count} onClick={() => void navigate(topic.id, 'next')}>下一处</Button>
      <Button variant="ghost" onClick={() => setOpen(true)}>返回话题列表</Button>
      {!open && error ? <p role="alert">这个位置暂时无法读取，请返回话题列表重试。</p> : null}
    </nav>}
  </div>
}
