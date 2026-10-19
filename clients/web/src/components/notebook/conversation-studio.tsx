import { useEffect, useId, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { z } from 'zod'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { conversationTopicSchema } from '@/api/conversation-topics'
import type { TutorConversation } from '@/api/conversations'
import { mentorReceiptSchema } from '@/api/mentor'
import type { Space } from '@/api/runtime'
import { studioJobsAPI, type StudioJobCreate } from '@/api/studio-jobs'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { useSourceSelection } from './source-selection'

const optionsSchema = z.object({
  conversation_id: z.uuid(),
  privacy_generation: z.number().int().positive(),
  index_revision: z.number().int().nonnegative(),
  through_message: z.uuid().optional(),
  status: z.enum(['not_started', 'queued', 'running', 'ready', 'failed']),
  partial: z.boolean(),
  items: z.array(conversationTopicSchema),
  next_cursor: z.uuid().optional(),
})

export function ConversationStudio({
  conversation,
  space,
  canWrite,
  onSource,
  onCreated,
}: {
  conversation: TutorConversation
  space: Space
  canWrite: boolean
  onSource: () => void
  onCreated: (id: string) => void
}) {
  const [open, setOpen] = useState(false)
  const { selection } = useSourceSelection()
  const trigger = useRef<HTMLButtonElement>(null)
  const id = useId()
  return (
    <div className="chat-topics">
      <Button
        ref={trigger}
        variant="ghost"
        disabled={!canWrite}
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen(!open)}
      >
        整理本次对话
      </Button>
      {open && (
        <div
          id={id}
          className="chat-topics-panel"
          role="region"
          aria-label="整理本次对话"
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              e.stopPropagation()
              setOpen(false)
              trigger.current?.focus()
            }
          }}
        >
          <ConversationStudioChooser
            key={JSON.stringify([selection, space.direction_version])}
            conversation={conversation}
            space={space}
            onSource={() => {
              setOpen(false)
              onSource()
            }}
            onCreated={(id) => {
              setOpen(false)
              onCreated(id)
            }}
            onClose={() => {
              setOpen(false)
              trigger.current?.focus()
            }}
          />
        </div>
      )}
    </div>
  )
}

function ConversationStudioChooser({
  conversation,
  space,
  onSource,
  onCreated,
  onClose,
}: {
  conversation: TutorConversation
  space: Space
  onSource: () => void
  onCreated: (id: string) => void
  onClose: () => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const { selection } = useSourceSelection()
  const query = useQueryClient()
  const [excluded, setExcluded] = useState<Set<string>>(new Set())
  const [kind, setKind] = useState<'note' | 'quiz'>('note')
  const [attempted, setAttempted] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [destination, setDestination] = useState(false)
  const key = JSON.stringify([...prefix, space.id, conversation.id, 'studio-conversation-create'])
  const [pending, setPending] = useState<StudioJobCreate | undefined>(() => drafts.get(key))
  const alive = useRef(true)
  const panel = useRef<HTMLDivElement>(null)
  useEffect(() => {
    alive.current = true
    panel.current?.querySelector<HTMLButtonElement>('button')?.focus()
    return () => {
      alive.current = false
    }
  }, [])
  const api = learningClient(session, space.id)
  const params = {
    header: { 'X-Learning-Space-ID': space.id },
    path: { conversationID: conversation.id },
  }
  const hasSources = selection?.mode === 'items' && selection.items.length > 0
  const scope = useQuery({
    queryKey: [
      ...prefix,
      space.id,
      conversation.id,
      'studio-conversation-topics',
      selection,
      space.direction_version,
    ],
    enabled: hasSources,
    gcTime: 0,
    retry: false,
    queryFn: async ({ signal }) => {
      const read = (cursor?: string, revision?: number) =>
        unwrap(
          api.POST('/v1/learning/conversations/{conversationID}/studio-scope', {
            params,
            body: {
              source_selection: selection!,
              direction_version: space.direction_version ?? 0,
              cursor,
              index_revision: revision,
            },
            signal,
          }),
          optionsSchema,
        )
      const first = await read()
      if (
        first.conversation_id !== conversation.id ||
        first.privacy_generation !== session.generation
      )
        throw new ApiError(502, 'invalid_response')
      let cursor = first.next_cursor
      while (cursor) {
        const page = await read(cursor, first.index_revision)
        first.items.push(...page.items)
        first.partial ||= page.partial
        cursor = page.next_cursor
      }
      return first
    },
    refetchInterval: (q) =>
      ['queued', 'running'].includes(q.state.data?.status ?? '') ? 1200 : false,
  })
  const head = scope.data
  const indexing = head?.status === 'queued' || head?.status === 'running'
  const selected = head?.items.filter((t) => !excluded.has(t.id)) ?? []
  const needsConfirmation = conversation.confirmation_required && !destination
  const organize = async () => {
    if (!head || busy || !selection || needsConfirmation || pending) return
    setAttempted(true)
    setBusy(true)
    setError(undefined)
    const payload = {
      expected_index_revision: head.index_revision,
      privacy_generation: session.generation,
      direction_version: conversation.mode === 'study' ? space.direction_version : undefined,
      source_selection: selection,
      ...(destination ? { confirm_destination: conversation.destination } : {}),
    }
    const operationKey = key + ':index'
    try {
      await unwrap(
        api.POST('/v1/learning/conversations/{conversationID}/topic-index', {
          params,
          body: { ...payload, operation_id: drafts.operation(operationKey, payload) },
        }),
        mentorReceiptSchema,
      )
      if (!alive.current) return
      drafts.delete(operationKey)
      await scope.refetch()
    } catch (e) {
      if (alive.current) {
        setError(e)
        void scope.refetch()
      }
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  useEffect(() => {
    if (head?.partial && !attempted && !busy && !indexing && !needsConfirmation && !pending)
      void organize()
  }, [head, attempted, busy, indexing, needsConfirmation, pending])
  const create = async (acceptPartial: boolean) => {
    if (busy || !head?.through_message || !hasSources || !selected.length || selected.length > 64)
      return
    setBusy(true)
    setError(undefined)
    const body: StudioJobCreate = pending ?? {
      operation_id: crypto.randomUUID(),
      privacy_generation: session.generation,
      kind,
      source_selection: selection,
      direction_version: space.direction_version ?? 0,
      conversation_scope: {
        conversation_id: conversation.id,
        topic_ids: selected.map((t) => t.id),
        index_revision: head.index_revision,
        through_message: head.through_message,
        accept_partial: acceptPartial,
      },
    }
    drafts.set(key, body)
    setPending(body)
    try {
      const job = await studioJobsAPI(session, space.id).create(body)
      if (!alive.current) return
      drafts.delete(key)
      query.setQueryData([...prefix, space.id, 'studio-job', job.id], job)
      void query.invalidateQueries({ queryKey: [...prefix, space.id, 'studio-jobs'] })
      onCreated(job.id)
    } catch (e) {
      if (!alive.current) return
      setError(e)
      if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
        drafts.delete(key)
        setPending(undefined)
        void scope.refetch()
      }
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  return (
    <div ref={panel}>
      <div className="chat-topics-heading">
        <strong>整理本次对话</strong>
        <Button variant="ghost" onClick={onClose}>
          关闭
        </Button>
      </div>
      <p>话题只限定知识点和疑问；笔记与测验的事实依据来自当前所选资料。</p>
      {!hasSources ? (
        <>
          <p role="status">请先选择至少一份资料，再整理本次对话。</p>
          <Button onClick={onSource}>选择资料</Button>
        </>
      ) : (
        <>
          {scope.error ? (
            <ErrorState error={scope.error} retry={() => void scope.refetch()} />
          ) : !head ? (
            <p role="status">正在读取可用话题…</p>
          ) : (
            <>
              {indexing || (busy && !pending) ? (
                <p role="status">正在整理尚未覆盖的内容…</p>
              ) : (
                head.partial && (
                  <p role="status">
                    未覆盖最新内容。你可以先重试整理，也可以按当前已整理的范围继续。
                  </p>
                )
              )}
              {head.items.length === 0 && !indexing && (
                <p>当前资料和学习方向下还没有可用话题。请整理话题，或选择本次对话使用的资料。</p>
              )}
              <fieldset disabled={busy || indexing || !!pending}>
                <legend>选择话题</legend>
                <label>
                  <input
                    type="checkbox"
                    checked={head.items.length > 0 && selected.length === head.items.length}
                    onChange={(e) =>
                      setExcluded(
                        e.target.checked ? new Set() : new Set(head.items.map((t) => t.id)),
                      )
                    }
                  />
                  全部可用话题
                </label>
                {head.items.map((topic) => (
                  <label key={topic.id} className="chat-studio-topic">
                    <input
                      type="checkbox"
                      checked={!excluded.has(topic.id)}
                      onChange={(e) =>
                        setExcluded((old) => {
                          const next = new Set(old)
                          if (e.target.checked) next.delete(topic.id)
                          else next.add(topic.id)
                          return next
                        })
                      }
                    />
                    {topic.title}
                  </label>
                ))}
                {selected.length > 64 && (
                  <p role="alert">每次最多选择 64 个话题，请去掉部分话题后继续。</p>
                )}
                <label>
                  生成类型
                  <select value={kind} onChange={(e) => setKind(e.target.value as 'note' | 'quiz')}>
                    <option value="note">笔记</option>
                    <option value="quiz">复习测验</option>
                  </select>
                </label>
              </fieldset>
              {kind === 'quiz' && !space.direction_version && (
                <p>请先在学习区设置学习方向，再生成复习测验。</p>
              )}
              {conversation.confirmation_required && (
                <label>
                  <input
                    type="checkbox"
                    checked={destination}
                    onChange={(e) => setDestination(e.target.checked)}
                  />
                  同意将本次范围内的对话发送到 {conversation.destination_provider}（
                  {conversation.destination_endpoint}）整理话题
                </label>
              )}
              {(head.partial || !head.items.length) && !pending && (
                <Button
                  variant="outline"
                  disabled={busy || indexing || needsConfirmation}
                  onClick={() => void organize()}
                >
                  重试整理话题
                </Button>
              )}
              {pending && <p role="status">正在核对原请求；重新发送会使用原来的话题和资料选择。</p>}
              <Button
                disabled={
                  busy ||
                  indexing ||
                  !!scope.error ||
                  !selected.length ||
                  selected.length > 64 ||
                  (kind === 'quiz' && !space.direction_version)
                }
                onClick={() => void create(!!head.partial)}
              >
                {pending
                  ? '重新发送原请求'
                  : head.partial
                    ? '按已整理范围继续'
                    : kind === 'note'
                      ? '生成笔记大纲'
                      : '生成题目规划'}
              </Button>
            </>
          )}
          {!!error && <ErrorState error={error} />}
        </>
      )}
    </div>
  )
}
