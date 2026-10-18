import { useEffect, useRef, useState } from 'react'
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query'
import { Copy, FileText, Plus, Sparkles, StickyNote } from 'lucide-react'
import { createPortal } from 'react-dom'
import { ApiError, learningClient, unwrap } from '@/api/client'
import {
  checkConversation,
  conversationCreated,
  conversationsSchema,
  turnsSchema,
  type TurnSubmission,
} from '@/api/conversations'
import {
  isMentorTerminal,
  mentorReceiptSchema,
  mentorSnapshotSchema,
  mentorStatus,
  stopMentor,
} from '@/api/mentor'
import { settingsSchema } from '@/api/settings'
import { studioAPI, studioRequest } from '@/api/studio'
import type { Space } from '@/api/runtime'
import { useIdentity } from '@/lib/session'
import { MentorAnswer } from '../mentor-answer'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { useSourceSelection } from './source-selection'
import { ChatOverview } from './chat-overview'
import { SpaceDialog } from './space-dialog'
import { useNotebookChrome } from './notebook-chrome'
import { InlineExerciseCard } from './inline-exercise-card'
import { ChatInput } from './chat-input'
import { ConversationSwitcher } from './conversation-switcher'
import { SubmissionNotice } from './submission-notice'
import { RunResultNotice } from '../run-result-notice'

type PendingTurn = { id: string; body: TurnSubmission }
export function ChatPanel({
  space,
  conversationId,
  onConversation,
  onCitation,
  onSource,
}: {
  space: Space
  conversationId?: string
  onConversation: (id?: string) => void
  onCitation: (run: string, id: string) => void
  onSource: () => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const { menu } = useNotebookChrome()
  const query = useQueryClient()
  const { selection } = useSourceSelection()
  const key = JSON.stringify([...prefix, space.id, 'study-draft', conversationId ?? 'new'])
  const [prompt, setPrompt] = useState(() => drafts.get<string>(key) ?? '')
  const [pendingTurn, setPendingTurn] = useState(() => drafts.get<PendingTurn>(key + ':pending'))
  const [recovering, setRecovering] = useState(!!pendingTurn)
  const recoveryStarted = useRef(false)
  const [stopping, setStopping] = useState(false)
  const [busy, setBusy] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<unknown>()
  const [notice, setNotice] = useState('')
  const [needSources, setNeedSources] = useState(false)
  const [destination, setDestination] = useState('')
  const live = useRef(true),
    stream = useRef<HTMLDivElement>(null),
    follow = useRef(true)
  const contentAllowed = useRef(true)
  const header = { 'X-Learning-Space-ID': space.id }
  const api = () => learningClient(session, space.id)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  const conversations = useInfiniteQuery({
    queryKey: [...prefix, space.id, 'study-conversations'],
    gcTime: 0,
    retry: false,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) =>
      unwrap(
        api().GET('/v1/learning/conversations', {
          params: { header, query: { mode: 'study', limit: 50, cursor: pageParam } },
          signal,
        }),
        conversationsSchema,
      ),
    getNextPageParam: (page) => page.next_cursor,
  })
  const history = useInfiniteQuery({
    queryKey: [...prefix, space.id, 'study-turns', conversationId],
    enabled: !!conversationId,
    gcTime: 0,
    retry: false,
    initialPageParam: 0,
    queryFn: async ({ pageParam, signal }) => {
      const page = await unwrap(
        api().GET('/v1/learning/conversations/{conversationID}', {
          params: {
            header,
            path: { conversationID: conversationId! },
            query: { after: pageParam, limit: 50 },
          },
          signal,
        }),
        turnsSchema,
      )
      checkConversation(page.conversation, space.id, session.generation, conversationId)
      if (page.conversation.mode !== 'study') throw new ApiError(409, 'wrong_conversation_mode')
      return page
    },
    getNextPageParam: (page) => page.next_cursor,
    refetchInterval: 1500,
  })
  const configuration = useQuery({
    queryKey: [...prefix, 'settings'],
    queryFn: ({ signal }) =>
      unwrap(learningClient(session).GET('/v1/settings', { signal }), settingsSchema),
  })
  const conversation = history.data?.pages[0]?.conversation
  const turns = history.data?.pages.flatMap((p) => p.items) ?? []
  const currentRun = useQuery({
    queryKey: [...prefix, space.id, 'study-run', conversation?.current_run_id],
    enabled: !!conversation?.current_run_id,
    gcTime: 0,
    retry: false,
    queryFn: ({ signal }) =>
      unwrap(
        api().GET('/v1/learning/runs/{runID}', {
          params: { header, path: { runID: conversation!.current_run_id! } },
          signal,
        }),
        mentorSnapshotSchema,
      ),
    refetchInterval: (q) => (q.state.data && isMentorTerminal(q.state.data.status) ? false : 1500),
  })
  const run = currentRun.data
  const active = turns.some(
    (turn) => !['succeeded', 'partial', 'failed', 'cancelled'].includes(turn.status),
  )
  const canWrite =
    session.capabilities.runs && session.capabilities.save_goal && space.status === 'active'
  const sourceReadable = session.device.scopes.includes('knowledge:read')
  const failure = history.error || conversations.error
  const denied =
    !session.device.scopes.includes('learning:read') ||
    (failure instanceof ApiError &&
      ([401, 403, 404].includes(failure.status) ||
        ['content_redacted', 'privacy_clear_in_progress'].includes(failure.code)))
  contentAllowed.current = !denied && sourceReadable
  useEffect(() => {
    if (denied || !sourceReadable) {
      drafts.deletePrefix(JSON.stringify([...prefix, space.id, 'study-exercise']).slice(0, -1))
      drafts.delete(key)
      drafts.delete(key + ':pending')
      setPrompt('')
      setPendingTurn(undefined)
      setNotice('')
    }
  }, [denied, sourceReadable, drafts, key])
  const ready =
    canWrite &&
    !!space.direction_version &&
    !!selection &&
    !busy &&
    !active &&
    !pendingTurn &&
    !!configuration.data?.effective_mentor.configured &&
    !!configuration.data?.effective_mentor.enabled &&
    !!conversations.data?.pages[0].save_available &&
    (!conversationId || !!conversation?.writable) &&
    (!conversation?.confirmation_required || destination === conversation.destination)
  useEffect(() => {
    if ((turns.length > 0 || pendingTurn) && follow.current && stream.current)
      stream.current.scrollTop = stream.current.scrollHeight
  }, [turns.length, turns.at(-1)?.output, pendingTurn])
  const refresh = () => {
    void query.invalidateQueries({ queryKey: [...prefix, space.id, 'study-turns'] })
    void query.invalidateQueries({ queryKey: [...prefix, space.id, 'study-conversations'] })
  }
  const accept = (id: string) => {
    if (!live.current || !contentAllowed.current) return
    drafts.delete(key)
    drafts.delete(key + ':pending')
    drafts.delete(key + ':new')
    drafts.delete(key + ':turn')
    setPrompt('')
    setPendingTurn(undefined)
    setDestination('')
    refresh()
    onConversation(id)
  }
  const send = async (text = prompt, retry = false) => {
    if (busy || (!retry && (!ready || !text.trim()))) return
    if (!retry && text.includes('出一道题')) {
      const waiting = stream.current?.querySelector<HTMLElement>('.chat-practice-card[data-pending="true"]')
      if (waiting) {
        waiting.scrollIntoView({ block: 'center' })
        waiting.querySelector<HTMLTextAreaElement>('textarea')?.focus({ preventScroll: true })
        setNotice('先完成这道题，或稍后再做。')
        return
      }
    }
    if (!retry && text.includes('出一道题') && selection?.items.length === 0) {
      setNeedSources(true)
      return
    }
    setNeedSources(false)
    setBusy(true)
    setSubmitting(true)
    setError(undefined)
    setNotice('')
    try {
      let target = conversation
      if (!target && !pendingTurn) {
        const body = { mode: 'study' as const, saved: true }
        const created = await unwrap(
          api().POST('/v1/learning/conversations', {
            params: { header },
            body: { ...body, id: drafts.operation(key + ':new', body) },
          }),
          conversationCreated,
        )
        if (!live.current || !contentAllowed.current) return
        const page = await unwrap(
          api().GET('/v1/learning/conversations/{conversationID}', {
            params: { header, path: { conversationID: created.id } },
          }),
          turnsSchema,
        )
        target = checkConversation(page.conversation, space.id, session.generation, created.id)
      }
      if (!live.current || !contentAllowed.current) return
      const payload = {
        expected_version: target?.version ?? 1,
        direction_version: space.direction_version!,
        source_selection: selection!,
        prompt: text,
        ...(destination ? { confirm_destination: destination } : {}),
      }
      const pending = pendingTurn ?? {
        id: target!.id,
        body: { ...payload, operation_id: drafts.operation(key + ':turn', payload) },
      }
      drafts.set(key + ':pending', pending)
      setPendingTurn(pending)
      const receipt = await unwrap(
        api().POST('/v1/learning/conversations/{conversationID}/turns', {
          params: { header, path: { conversationID: pending.id } },
          body: pending.body,
        }),
        mentorReceiptSchema,
      )
      if (receipt.session_id !== pending.id) throw new ApiError(502, 'invalid_response')
      accept(pending.id)
    } catch (e) {
      if (live.current) {
        if (e instanceof ApiError && e.status < 500) {
          drafts.delete(key + ':pending')
          setPendingTurn(undefined)
        }
        setError(e)
      }
    } finally {
      if (live.current) {
        setBusy(false)
        setSubmitting(false)
      }
    }
  }
  const reconcile = async () => {
    if (!pendingTurn || busy) return
    setBusy(true)
    setError(undefined)
    try {
      const receipt = await unwrap(
        api().GET('/v1/learning/operations/{operationID}', {
          params: { header, path: { operationID: pendingTurn.body.operation_id } },
        }),
        mentorReceiptSchema,
      )
      if (receipt.session_id !== pendingTurn.id) throw new ApiError(502, 'invalid_response')
      accept(pendingTurn.id)
    } catch (e) {
      if (live.current) setError(e)
    } finally {
      if (live.current) {
        setBusy(false)
        setRecovering(false)
      }
    }
  }
  useEffect(() => {
    if (recovering && !recoveryStarted.current && !denied && sourceReadable) {
      recoveryStarted.current = true
      void reconcile()
    }
  }, [recovering, denied, sourceReadable])
  useEffect(() => {
    if (run && isMentorTerminal(run.status)) setStopping(false)
  }, [run?.status])
  const command = async (kind: 'stop' | 'resume') => {
    if (!run || busy) return
    setBusy(true)
    if (kind === 'stop') setStopping(true)
    setError(undefined)
    try {
      if (kind === 'stop') {
        await stopMentor(session, space.id, run.run_id, payload => drafts.operation(key + ':command', payload))
      } else {
        for (let attempt = 0; attempt < 2; attempt++) {
          const current = await unwrap(api().GET('/v1/learning/runs/{runID}', {
            params: { header, path: { runID: run.run_id } },
          }), mentorSnapshotSchema)
          if (isMentorTerminal(current.status) || current.status === 'cancelling') break
          const payload = {
            expected_version: current.version,
            kind,
          }
          try {
            await unwrap(api().POST('/v1/learning/runs/{runID}/commands', {
              params: { header, path: { runID: run.run_id } },
              body: { ...payload, operation_id: drafts.operation(key + ':command', payload) },
            }), mentorReceiptSchema)
            break
          } catch (e) {
            if (!(e instanceof ApiError) || e.status !== 409 || attempt === 1) throw e
          }
        }
      }
      if (live.current) {
        void currentRun.refetch()
        refresh()
      }
    } catch (e) {
      if (live.current) {
        setError(e)
        setStopping(false)
      }
    } finally {
      if (live.current) setBusy(false)
    }
  }
  const saveNote = async (turn: (typeof turns)[number]) => {
    if (busy || !canWrite) return
    setBusy(true)
    setError(undefined)
    const noteKey = JSON.stringify([...prefix, space.id, 'chat-note', turn.run_id])
    const markdown =
      turn.output +
      (turn.citations?.length
        ? '\n\n' +
          turn.citations
            .map(
              (c) => `[${c.id}] ${c.source.path}\n\n> ${c.fragment.text.replaceAll('\n', '\n> ')}`,
            )
            .join('\n\n')
        : '')
    const request = studioRequest(
      drafts,
      noteKey,
      {
        title:
          '对话笔记：' +
          (turn.messages.find((m) => m.role === 'user')?.content ?? space.name).slice(0, 40),
        markdown,
        baseVersion: 0,
        goalId: null,
      },
      session.generation,
    )
    try {
      await studioAPI(session, space.id).save(undefined, request)
      if (live.current) {
        setNotice('已保存到 Studio 笔记')
        void query.invalidateQueries({ queryKey: [...prefix, space.id, 'studio-list'] })
      }
    } catch (e) {
      if (live.current) setError(e)
    } finally {
      if (live.current) setBusy(false)
    }
  }
  const options = conversations.data?.pages.flatMap((p) => p.items) ?? []
  const lastTurn = turns.at(-1)
  const posting = submitting && !!pendingTurn
  const thinking = !!(lastTurn?.body_available && ['queued', 'running'].includes(lastTurn.status) && !lastTurn.output)
  const readableTurn = lastTurn?.body_available &&
    (!lastTurn.source_manifest?.entries.length || sourceReadable)
  const turnLabel = `学习导师第 ${lastTurn?.ordinal} 轮`
  const announcement = !sourceReadable ? '' : thinking ? '学习导师正在思考' : readableTurn && isMentorTerminal(lastTurn.status as keyof typeof mentorStatus)
    ? lastTurn.status === 'cancelled' ? `${turnLabel}已停止生成` : lastTurn.status === 'failed' ? `${turnLabel}未完成` : `${turnLabel}回答已完成` : ''
  if (denied && !failure) return <p role="status">当前设备无法读取对话。</p>
  if (failure)
    return (
      <ErrorState
        error={failure}
        retry={() => {
          void history.refetch()
          void conversations.refetch()
        }}
      />
    )
  return (
    <div className="chat-panel">
      <p className="sr-only" role="status" aria-live="polite" aria-atomic="true">
        {announcement}
      </p>
      <header className="chat-toolbar">
        <ConversationSwitcher
          options={options}
          conversation={conversation}
          conversationId={conversationId}
          busy={busy}
          onConversation={onConversation}
        />
        <Button
          variant="ghost"
          aria-label="新建对话"
          disabled={busy}
          onClick={() => onConversation()}
        >
          <Plus size={18} />
        </Button>
        {conversations.hasNextPage && (
          <Button variant="ghost" onClick={() => void conversations.fetchNextPage()}>
            更多对话
          </Button>
        )}
        <span className="chat-saved">{conversation ? '已保存' : '学习助手'}</span>
      </header>
      <div
        className="chat-stream"
        ref={stream}
        onScroll={() => {
          const el = stream.current
          if (el) follow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60
        }}
      >
        {conversationId && !history.data ? (
          <p role="status">正在读取对话…</p>
        ) : turns.length === 0 ? (
          <ChatOverview
            space={space}
            disabled={!ready}
            onAsk={(text) => void send(text)}
            onContinue={
              options.find((c) => c.current_run_id)
                ? () => onConversation(options.find((c) => c.current_run_id)!.id)
                : undefined
            }
            onSource={onSource}
          />
        ) : (
          <>
            {!space.direction_version && <SpaceDialog space={space} confirmDirection />}
            {turns.map((turn) => (
              <article className="chat-turn" key={turn.run_id}>
                {turn.body_available &&
                (!turn.source_manifest?.entries.length || sourceReadable) ? (
                  <>
                    {turn.messages
                      .filter((m) => m.role === 'user')
                      .map((m, i) => (
                        <div className="chat-user" key={i}>
                          <div>{m.content}</div>
                        </div>
                      ))}
                    <div className="chat-ai">
                      <div className="chat-author">
                        <Sparkles size={17} />
                        学习导师
                      </div>
                      {turn.output ? (
                        <>
                          <div className="chat-answer" data-generating={!isMentorTerminal(turn.status as keyof typeof mentorStatus) || undefined}>
                            <MentorAnswer
                              renderMath
                              spaceId={space.id}
                              runId={turn.run_id}
                              text={turn.output}
                              citations={turn.citations}
                              onCitation={onCitation}
                            />
                          </div>
                          {turn.status === 'cancelled' && <p role="status">已停止生成</p>}
                          <div className="chat-message-actions">
                            <Button
                              variant="ghost"
                              disabled={busy || !canWrite}
                              onClick={() => void saveNote(turn)}
                            >
                              <StickyNote size={14} />
                              保存为笔记
                            </Button>
                            <Button
                              variant="ghost"
                              onClick={() => {
                                void navigator.clipboard.writeText(turn.output).then(
                                  () => {
                                    if (live.current) setNotice('已复制')
                                  },
                                  () => {
                                    if (live.current) setNotice('复制失败，请选中正文复制')
                                  },
                                )
                              }}
                            >
                              <Copy size={14} />
                              复制
                            </Button>
                          </div>
                        </>
                      ) : ['queued', 'running'].includes(turn.status) ? (
                        <ThinkingIndicator />
                      ) : (
                        <p>
                          {mentorStatus[turn.status as keyof typeof mentorStatus] ?? turn.status}
                        </p>
                      )}
                    </div>
                    {turn.exercise_id && <InlineExerciseCard
                      spaceId={space.id} conversationId={conversationId!}
                      exerciseId={turn.exercise_id} selection={selection} disabled={!canWrite}
                      onSource={onSource}
                    />}
                    {turn.output === '先添加或选择来源，再试一道题。' && <Button variant="outline" onClick={onSource}>添加或选择来源</Button>}
                  </>
                ) : (
                  <p role="status">此轮正文不可用。</p>
                )}
              </article>
            ))}
            {history.hasNextPage && (
              <Button variant="outline" onClick={() => void history.fetchNextPage()}>
                加载后续消息
              </Button>
            )}
          </>
        )}
        {posting && <article className="chat-turn">
          <div className="chat-user"><div>{pendingTurn.body.prompt}</div></div>
          <div className="chat-ai">
            <div className="chat-author"><Sparkles size={17} aria-hidden="true" />学习导师</div>
            <ThinkingIndicator />
          </div>
        </article>}
      </div>
      <div className="chat-composer">
        {needSources && <p role="status">先选一份资料。<Button variant="ghost" onClick={onSource}>添加或选择来源</Button></p>}
        {!!notice && <p role="status">{notice}</p>}
        {!!error && !pendingTurn && <ErrorState error={error} retry={refresh} />}
        {configuration.error && <ErrorState error={configuration.error} />}
        {configuration.data && !configuration.data.effective_mentor.configured && (
          <p>请在设置中配置教学模型。</p>
        )}
        <SubmissionNotice pending={!!pendingTurn} inFlight={busy || recovering}>
            提交结果待核对。
            <Button variant="ghost" onClick={() => void reconcile()}>
              核对
            </Button>
            <Button variant="ghost" onClick={() => void send('', true)}>
              重试
            </Button>
        </SubmissionNotice>
        {conversation?.confirmation_required && (
          <label className="check-label">
            <input
              type="checkbox"
              checked={destination === conversation.destination}
              onChange={(e) => setDestination(e.target.checked ? conversation.destination : '')}
            />
            同意将当前方向、所选来源和同范围历史发送到 {conversation.destination_endpoint}
          </label>
        )}
        {run && <RunResultNotice run={run} className="submission-notice" role="status">模型请求结果未知，未自动重发。</RunResultNotice>}
        {run?.status === 'failed' && <p role="alert">本轮未完成：{run.reason}</p>}
        <div className="chat-quick">
          {['换个讲法', '举个例子', '出一道题', '总结一下'].map((text) => (
            <button
              key={text}
              disabled={!ready}
              onClick={() => {
                if (text === '出一道题') { void send(text); return }
                setPrompt(text)
                drafts.set(key, text)
              }}
            >
              {text}
            </button>
          ))}
        </div>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void send()
          }}
        >
          <ChatInput value={prompt} onChange={value => {
            setPrompt(value)
            drafts.set(key, value)
          }} onSend={() => void send()} onStop={() => void command('stop')}
            disabled={!canWrite || !!pendingTurn} ready={ready}
            running={active} stopping={stopping || run?.status === 'cancelling'}
            stopDisabled={busy || !run || !canWrite} />
          <div className="chat-input-footer">
            <FileText size={12} aria-hidden="true" />
            <span>基于 {selection?.items.length ?? '…'} 个来源</span>
          </div>
        </form>
        {menu &&
          createPortal(
            <details>
              <summary>对话设置</summary>
              <p>{configuration.data?.effective_mentor.endpoint}</p>
            </details>,
            menu,
          )}
        {!!run && ['paused_budget', 'paused_tool_steps'].includes(run.status) && (
          <div>
          <p role="status">{mentorStatus[run.status]}</p>
          <Button
            disabled={busy || !canWrite || !!conversation?.confirmation_required}
            onClick={() => void command('resume')}
          >
            继续
          </Button>
          </div>
        )}
      </div>
    </div>
  )
}

function ThinkingIndicator() {
  return <div className="chat-thinking" aria-hidden="true">
    <span className="chat-thinking-dots"><i /><i /><i /></span>
    <span>正在思考…</span>
  </div>
}
