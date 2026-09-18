import { useEffect, useRef, useState, type CSSProperties } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './api/client'
import { goalSchema, spaceSchema, type Goal } from './api/runtime'
import {
  answerable,
  contentCapabilities,
  contentHeader,
  contentHistory,
  contentSchema,
  helpLabels,
  operation,
  operationReceipt,
  operationResult,
  propose,
  sessionPage,
  stateLabels,
  teachingSchema,
  type Action,
  type Content,
  type Help,
  type Teaching,
} from './api/teaching'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { ErrorState, Pagination } from './components/common'
import { ContentBlocks, SafeMarkdown, SourceViewer } from './components/content-blocks'
import { TutorHistory } from './components/tutor-history'
import { ChangePanel } from './components/change-panel'
import { ReferenceLink } from './knowledge-page'
import { KnowledgeStructure } from './components/knowledge-structure'
import { knowledgeContextSchema } from './api/start'
import { ContentEditor, useContentSelection } from './components/content-editor'
import { ContentTools } from './components/content-tools'
import { preferenceSchema } from './api/content'

export function SessionPicker({ goal, archived }: { goal: Goal; archived: boolean }) {
  const { session, prefix, drafts } = useIdentity()
  const navigate = useNavigate()
  const [cursors, setCursors] = useState([''])
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const client = () => learningClient(session, goal.learning_space_id)
  const sessions = useQuery({
    queryKey: [
      ...prefix,
      goal.learning_space_id,
      goal.goal_id,
      'teaching-sessions',
      cursors.at(-1),
    ],
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/tutoring/sessions', {
          params: { query: { goal_id: goal.goal_id, limit: 10, cursor: cursors.at(-1) } },
          signal,
        }),
        sessionPage,
      ),
    gcTime: 0,
  })
  const start = async () => {
    if (pending) return
    setPending(true)
    setError(undefined)
    const key = JSON.stringify([
      ...prefix,
      goal.learning_space_id,
      goal.goal_id,
      goal.goal_revision_id,
      'new-teaching',
    ])
    const sessionID = drafts.get<string>(key) ?? crypto.randomUUID()
    drafts.set(key, sessionID)
    const payload = {
      payload_schema_version: 1 as const,
      aggregate_type: 'session' as const,
      aggregate_id: sessionID,
      expected_version: 0 as const,
      goal_revision_id: goal.goal_revision_id,
    }
    try {
      await unwrap(
        client().POST('/v1/tutoring/sessions', {
          body: { ...payload, operation_id: drafts.operation(key, payload) },
        }),
        operationResult,
      )
      drafts.delete(key)
      if (mounted.current)
        await navigate({
          to: '/spaces/$spaceId/learn/$sessionId',
          params: { spaceId: goal.learning_space_id, sessionId: sessionID },
        })
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setPending(false)
    }
  }
  return (
    <section className="panel section" id="teaching-sessions" aria-label="教学会话">
      <h2>继续学习</h2>
      <p className="hint">选择真实教学会话，会保留原活动和反馈。新建会话使用当前目标修订。</p>
      {sessions.data?.items.map((item) => (
        <div className="session-row" key={item.session_id}>
          <Link
            to="/spaces/$spaceId/learn/$sessionId"
            params={{ spaceId: goal.learning_space_id, sessionId: item.session_id }}
          >
            {item.name} · {stateLabels[item.state] ?? item.state}
          </Link>
          <span className="hint">{item.position}</span>
        </div>
      ))}
      {sessions.data?.items.length === 0 && <p>还没有教学会话。</p>}
      <Pagination
        page={cursors.length}
        previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
        next={
          sessions.data?.next_cursor
            ? () => setCursors([...cursors, sessions.data!.next_cursor!])
            : undefined
        }
      />
      <Button
        disabled={
          pending ||
          archived ||
          !['active', 'draft'].includes(goal.management.status) ||
          !session.device.scopes.includes('learning:write')
        }
        onClick={() => void start()}
      >
        新建教学会话
      </Button>
      {!!(error || sessions.error) && (
        <ErrorState error={error || sessions.error} retry={() => void sessions.refetch()} />
      )}
    </section>
  )
}

export function TeachingPage({ spaceId, sessionId }: { spaceId: string; sessionId: string }) {
  const { session, prefix } = useIdentity()
  const view = useQuery({
    queryKey: [...prefix, spaceId, sessionId, 'teaching'],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/tutoring/sessions/{sessionID}', {
          params: { path: { sessionID: sessionId } },
          signal,
        }),
        teachingSchema,
      ),
    gcTime: 0,
  })
  const space = useQuery({
    queryKey: [...prefix, spaceId, 'space'],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces/{spaceID}', {
          params: { path: { spaceID: spaceId } },
          signal,
        }),
        spaceSchema,
      ),
  })
  if (view.error || space.error)
    return (
      <ErrorState
        error={view.error || space.error}
        retry={() => {
          void view.refetch()
          void space.refetch()
        }}
      />
    )
  if (!view.data || !space.data) return <p role="status">正在恢复教学会话…</p>
  if (
    view.data.session.session_id !== sessionId ||
    (view.data.work_item?.goal_revision?.learning_space_id &&
      view.data.work_item.goal_revision.learning_space_id !== spaceId)
  )
    return <ErrorState error={new ApiError(404, 'wrong_session')} />
  return (
    <TeachingWorkspace
      spaceId={spaceId}
      spaceName={space.data.name}
      archived={space.data.status === 'archived'}
      view={view.data}
      refresh={async () => {
        const result = await view.refetch()
        if (result.error) throw result.error
      }}
    />
  )
}

type Pending = {
  operationID: string
  action: string
  answerKey?: string
  answer?: string
  chatKey?: string
  chat?: string
}
type AnswerDraft = { answer: string; help: Help }

function TeachingWorkspace({
  spaceId,
  spaceName,
  archived,
  view,
  refresh,
}: {
  spaceId: string
  spaceName: string
  archived: boolean
  view: Teaching
  refresh: () => Promise<void>
}) {
  const { session, prefix, drafts } = useIdentity()
  const item = view.work_item
  const activity = item?.activity
  const sessionId = view.session.session_id
  const base = JSON.stringify([...prefix, spaceId, sessionId])
  const answerKey = `${base}:${activity?.activity_id}:${activity?.revision}:answer`
  const chatKey = `${base}:chat`
  const pendingKey = `${base}:pending`
  const [answer, setAnswer] = useState(() => drafts.get<AnswerDraft>(answerKey)?.answer ?? '')
  const [help, setHelp] = useState<Help>(() => drafts.get<AnswerDraft>(answerKey)?.help ?? 'none')
  const [chat, setChat] = useState(() => drafts.get<string>(chatKey) ?? '')
  const [pending, setPending] = useState(() => drafts.get<Pending>(pendingKey))
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)
  const [error, setError] = useState<unknown>()
  const [notice, setNotice] = useState('')
  const [tab, setTab] = useState('learn')
  const [aux, setAux] = useState('mentor')
  const [knowledgeWidth, setKnowledgeWidth] = useState(272)
  const [mentorWidth, setMentorWidth] = useState(360)
  const [content, setContent] = useState<Content>()
  const selection = useContentSelection(content)
  const updateContent = (next: Content) => {
    const anchor = selection.selected
      ? document.getElementById(`block-${selection.selected.location.block_id}`)
      : undefined
    const top = anchor?.getBoundingClientRect().top
    setContent(next)
    selection.clear()
    setNotice('本段已更新，原答案草稿已保留。')
    requestAnimationFrame(() => {
      if (anchor && top !== undefined) window.scrollBy(0, anchor.getBoundingClientRect().top - top)
    })
  }
  const [contentError, setContentError] = useState<unknown>()
  const [contentRefresh, setContentRefresh] = useState(0)
  const answerInput = useRef<HTMLTextAreaElement>(null)
  const learningColumn = useRef<HTMLDivElement>(null)
  const restoreFocusRequested = useRef('')
  const mounted = useRef(true)
  const currentDraft = useRef({ answerKey, answer, chat })
  currentDraft.current = { answerKey, answer, chat }
  const output = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  useEffect(() => {
    const draft = drafts.get<AnswerDraft>(answerKey)
    setAnswer(draft?.answer ?? '')
    setHelp(draft?.help ?? 'none')
  }, [answerKey])
  useEffect(() => {
    if (follow.current && output.current) output.current.scrollTop = output.current.scrollHeight
  }, [item?.free_answer?.text])
  const client = () => learningClient(session, spaceId)
  const knowledgeContext = useQuery({
    queryKey: [
      ...prefix,
      spaceId,
      sessionId,
      'knowledge-context',
      view.session.focus.knowledge_revision_id,
    ],
    enabled: session.device.scopes.includes('knowledge:read'),
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/tutoring/sessions/{sessionID}/knowledge-context', {
          params: { path: { sessionID: sessionId }, header: { 'X-Learning-Space-ID': spaceId } },
          signal,
        }),
        z.object({ knowledge_context: knowledgeContextSchema.nullable() }),
      ),
  })
  const capabilities = useQuery({
    queryKey: [...prefix, 'content-capabilities'],
    queryFn: ({ signal }) =>
      unwrap(client().GET('/v1/learning/content/capabilities', { signal }), contentCapabilities),
  })
  const goal = useQuery({
    queryKey: [...prefix, spaceId, item?.goal_revision?.goal_id, 'latest'],
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: item!.goal_revision!.goal_id } },
          signal,
        }),
        goalSchema,
      ),
    enabled: !!item?.goal_revision?.goal_id,
  })
  useEffect(() => {
    let active = true
    setContent(undefined)
    setContentError(undefined)
    if (!activity || !capabilities.data?.available || capabilities.data.protocol_version !== 1)
      return
    const controller = new AbortController()
    void unwrap(
      client().POST('/v1/tutoring/sessions/{sessionID}/content', {
        params: { path: { sessionID: sessionId }, header: contentHeader(spaceId) },
        body: { protocol_version: 1, activity_id: activity.activity_id },
        signal: controller.signal,
      }),
      contentSchema,
    )
      .then((result) => {
        if (
          active &&
          result.session_id === sessionId &&
          result.activity_id === activity.activity_id &&
          result.learning_space_id === spaceId &&
          result.privacy_generation === session.generation
        )
          setContent(result)
      })
      .catch((e) => {
        if (active) setContentError(e)
      })
    return () => {
      active = false
      controller.abort()
    }
  }, [activity?.activity_id, activity?.revision, capabilities.data, view.session.aggregate_version, contentRefresh])
  useEffect(() => {
    if (restoreFocusRequested.current && content?.activity_id === restoreFocusRequested.current && content.activity_id === activity?.activity_id) {
      const target = answerInput.current && !answerInput.current.disabled ? answerInput.current : learningColumn.current
      target?.focus()
      restoreFocusRequested.current = ''
    }
  }, [content, activity?.activity_id])
  const write = (value: string, level = help) => {
    setAnswer(value)
    setHelp(level)
    drafts.set(answerKey, { answer: value, help: level })
  }
  const finish = (record: Pending) => {
    if (record.answerKey && drafts.get<AnswerDraft>(record.answerKey)?.answer === record.answer)
      drafts.delete(record.answerKey)
    if (record.chatKey && drafts.get<string>(record.chatKey) === record.chat)
      drafts.delete(record.chatKey)
    drafts.delete(pendingKey)
    if (mounted.current) {
      setPending(undefined)
      if (
        record.answerKey === currentDraft.current.answerKey &&
        record.answer === currentDraft.current.answer
      )
        setAnswer('')
      if (record.chatKey === chatKey && record.chat === currentDraft.current.chat) setChat('')
    }
  }
  const reconcile = async (record = pending) => {
    if (!record) return
    try {
      const receipt = await unwrap(
        client().GET('/v1/tutoring/sessions/{sessionID}/operations/{operationID}', {
          params: {
            path: { sessionID: sessionId, operationID: record.operationID },
            header: { 'X-Learning-Space-ID': spaceId },
          },
        }),
        operationReceipt,
      )
      if (receipt.session_id !== sessionId || receipt.operation_id !== record.operationID)
        throw new ApiError(502, 'invalid_response')
      if (receipt.status === 'succeeded') finish(record)
      else {
        drafts.delete(pendingKey)
        if (mounted.current) setPending(undefined)
      }
      if (mounted.current) {
        setNotice(
          receipt.status === 'succeeded'
            ? '已核对原操作：提交已保存。'
            : '原操作已拒绝，输入已保留。',
        )
        await refresh()
      }
    } catch (e) {
      if (mounted.current) {
        setNotice('尚未确认原操作结果。请继续核对；不会换操作编号补交。')
        setError(e)
        await refresh().catch(setError)
      }
    }
  }
  const allowed = (name: string) => item?.allowed_actions.includes(name) ?? false
  const perform = async (
    fields: Omit<
      Action,
      | 'operation_id'
      | 'payload_schema_version'
      | 'aggregate_type'
      | 'aggregate_id'
      | 'expected_version'
    > &
      Record<string, unknown>,
    isAnswer = false,
  ) => {
    if (busyRef.current || pending || !allowed(fields.action)) return
    busyRef.current = true
    setBusy(true)
    setError(undefined)
    setNotice('')
    const record: Pending = {
      operationID: crypto.randomUUID(),
      action: fields.action,
      ...(isAnswer ? { answerKey, answer } : {}),
      ...(fields.action === 'ask_free_question' && fields.question === chat
        ? { chatKey, chat }
        : {}),
    }
    drafts.set(pendingKey, record)
    setPending(record)
    try {
      const body = { ...operation(view, record.operationID), ...fields } as Action
      const result =
        isAnswer && content
          ? await unwrap(
              client().POST('/v1/learning/content/{artifactID}/answers', {
                params: {
                  path: { artifactID: content.artifact_id },
                  header: contentHeader(spaceId),
                },
                body: {
                  ...operation(view, record.operationID),
                  action: 'submit_attempt',
                  answer,
                  help,
                  content_version: content.version,
                },
              }),
              operationResult,
            )
          : await unwrap(
              client().POST('/v1/tutoring/sessions/{sessionID}/actions', {
                params: { path: { sessionID: sessionId } },
                body,
              }),
              operationResult,
            )
      if (result.aggregate_id !== sessionId) throw new ApiError(502, 'invalid_response')
      finish(record)
      if (mounted.current) {
        setNotice('操作已保存。')
        await refresh()
      }
    } catch (e) {
      if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
        drafts.delete(pendingKey)
        if (mounted.current) {
          setPending(undefined)
          setError(e)
          await refresh().catch(setError)
        }
      } else if (mounted.current) {
        setError(e)
        await reconcile(record)
      }
    } finally {
      busyRef.current = false
      if (mounted.current) setBusy(false)
    }
  }
  const generate = async (
    kind: 'route' | 'activity' | 'assessment' | 'free_answer',
    action: 'apply_route' | 'issue_activity' | 'record_assessment' | 'record_free_answer',
  ) => {
    if (busyRef.current || pending) return
    busyRef.current = true
    setBusy(true)
    setError(undefined)
    setNotice('正在生成草稿；完整校验与正式保存前不可作答。')
    const requestID = drafts.operation(`${base}:proposal`, {
      kind,
      version: view.session.aggregate_version,
    })
    try {
      const proposalID = await propose(session, spaceId, view, kind, requestID)
      busyRef.current = false
      if (mounted.current) await perform({ action, proposal_id: proposalID })
    } catch (e) {
      if (mounted.current) {
        setError(e)
        setNotice('生成未完成，当前活动未被替换。')
        await refresh().catch(setError)
      }
    } finally {
      busyRef.current = false
      if (mounted.current) setBusy(false)
    }
  }
  const canWrite = session.device.scopes.includes('learning:write') && !busy && !pending
  const inactive =
    archived || (goal.data && !['active', 'draft'].includes(goal.data.management.status))
  const refs = content?.body.references ?? activity?.knowledge_references ?? []
  const draftContent = content?.status !== 'committed'
  const submit = () => {
    if (canWrite && answerable(content) && answer.trim())
      void perform({ action: 'submit_attempt', answer, help }, true)
  }
  const ask = (question = chat) => {
    if (canWrite && question.trim()) void perform({ action: 'ask_free_question', question })
  }
  const rootStyle = {
    '--knowledge-width': `${knowledgeWidth}px`,
    '--mentor-width': `${mentorWidth}px`,
  } as CSSProperties
  return (
    <section className="teaching-workspace" data-tab={tab} data-aux={aux} style={rootStyle}>
      <div className="teaching-toolbar">
        <header className="teaching-header">
          <div>
            <Link to="/spaces/$spaceId" params={{ spaceId }}>
              {spaceName}
            </Link>
            <span> / </span>
            {item?.goal_revision ? (
              <Link
                to="/spaces/$spaceId/goals/$goalId"
                params={{ spaceId, goalId: item.goal_revision.goal_id }}
              >
                {item.goal_revision.management?.details.name ?? item.goal_revision.text}
              </Link>
            ) : (
              <span>目标修订：{view.session.focus.goal_revision_id}</span>
            )}
          </div>
          <h1>{stateLabels[view.session.state] ?? '当前版本尚不支持的教学阶段'}</h1>
          <p role="status">
            {busy
              ? '正在处理…'
              : pending
                ? '提交结果待核对'
                : content
                  ? `内容第 ${content.version} 版已保存`
                  : '已读取服务端会话'}{' '}
            ·{' '}
            {activity
              ? `当前活动：${activity.type === 'explanation' ? '阅读' : '练习'}`
              : '当前焦点：学习路线'}
          </p>
        </header>
        <nav className="workspace-tabs" aria-label="工作区切换">
          {[
            ['learn', '学习'],
            ['knowledge', '知识'],
            ['mentor', '导师'],
          ].map(([value, label]) => (
            <Button
              key={value}
              variant={tab === value ? 'default' : 'outline'}
              aria-pressed={tab === value}
              onClick={() => {
                setTab(value)
                if (value !== 'learn') setAux(value)
              }}
            >
              {label}
            </Button>
          ))}
        </nav>
      </div>
      <div className="teaching-columns">
        <aside className="knowledge-column panel" aria-label="知识与来源">
          <h2>知识与来源</h2>
          {goal.data && <KnowledgeStructure key={`${spaceId}:${sessionId}`} spaceId={spaceId} goalId={goal.data.goal_id} sessionId={sessionId} compact />}
          <p className="hint">当前会话的真实范围，不表示概念掌握度。</p>
          {knowledgeContext.error && (
            <ErrorState
              error={knowledgeContext.error}
              retry={() => void knowledgeContext.refetch()}
            />
          )}
          {knowledgeContext.data?.knowledge_context && (
            <section aria-label="当前知识概念">
              {knowledgeContext.data.knowledge_context.concepts.map((concept) => (
                <details key={concept.revision_id}>
                  <summary>{concept.name} · 有来源支持</summary>
                  <p className="hint">支持状态独立于学习表现，引用可追溯不表示结论普遍正确。</p>
                  {concept.support.map((support, i) => (
                    <blockquote key={i}>
                      {support.quote}
                      <small style={{ display: 'block', overflowWrap: 'anywhere' }}>
                        来源修订：{support.revision_id} · 片段：{support.fragment_id}
                      </small>
                    </blockquote>
                  ))}
                </details>
              ))}
            </section>
          )}
          {item?.route_revision?.steps.map((step, i) => (
            <p
              key={step.route_step_id}
              aria-current={
                step.route_step_id === view.session.focus.route_step_id ? 'step' : undefined
              }
            >
              {i + 1}. {step.teaching_intent}
            </p>
          ))}
          {!item?.route_revision && <p>路线尚未生成。</p>}
          {refs.map((ref, i) => (
            <SourceViewer
              key={ref.node_revision_id}
              reference={ref}
              content={content}
              label={`原资料依据 ${i + 1}`}
            />
          ))}
          {!refs.length && <p className="hint">当前活动还没有可查看的资料引用。</p>}
          <label className="column-size">
            知识栏宽度
            <input
              type="range"
              min={256}
              max={300}
              value={knowledgeWidth}
              onChange={(e) => setKnowledgeWidth(Number(e.target.value))}
            />
          </label>
        </aside>
        <div className="learning-column panel" aria-label="当前学习" ref={learningColumn} tabIndex={-1}>
          {content ? (
            <>
              <div className="content-meta">
                <Link
                  to="/content/$artifactId"
                  params={{ artifactId: content.artifact_id }}
                  search={{ space: spaceId, version: content.version }}
                >
                  内容与版本历史
                </Link>
              </div>
              <ContentBlocks
                blocks={content.body.blocks}
                references={content.body.references}
                content={content}
                onSelect={(block, start, end) => {
                  void selection.select(block, start, end)
                  setAux('mentor')
                }}
              />
              {selection.selected && (
                <div className="selection-chip">
                  <span>已选正文，可交给导师解释或改写。</span>
                  <Button
                    variant="outline"
                    onClick={() => {
                      setTab('mentor')
                      setAux('mentor')
                    }}
                  >
                    处理此选段
                  </Button>
                  <Button variant="ghost" onClick={selection.clear}>
                    移除正文选区
                  </Button>
                </div>
              )}
            </>
          ) : activity ? (
            <SafeMarkdown text={activity.prompt} />
          ) : (
            <>
              <h2>当前学习</h2>
              <p>
                {view.session.state === 'Completed'
                  ? '本次会话已完成。可返回学习区，选择其他教学会话继续。'
                  : (item?.route_revision?.steps.find(
                      (s) => s.route_step_id === view.session.focus.route_step_id,
                    )?.teaching_intent ?? '从目标出发，开始本次教学。')}
              </p>
            </>
          )}
          {!capabilities.data?.available && activity && (
            <p className="notice">
              服务器尚未配置正文加密密钥，当前仅可阅读原活动；配置后可启用版本化作答。
            </p>
          )}
          {!!(contentError || capabilities.error) && (
            <ErrorState
              error={contentError || capabilities.error}
              retry={() => void refresh().catch(setError)}
            />
          )}
          {allowed('present_activity') && (
            <Button
              disabled={!canWrite || !content}
              onClick={() => void perform({ action: 'present_activity' })}
            >
              开始当前活动
            </Button>
          )}
          {allowed('submit_attempt') && activity && activity.type !== 'explanation' && (
            <form
              onSubmit={(e) => {
                e.preventDefault()
                submit()
              }}
            >
              <fieldset disabled={!canWrite || !answerable(content)}>
                <legend>正式答案</legend>
                {content?.body.interaction.kind === 'single_choice' ? (
                  <div role="radiogroup" aria-label="选择正式答案">
                    {content.body.interaction.choices?.map((choice) => (
                      <label className="check-label" key={choice.value}>
                        <input
                          type="radio"
                          name="formal-answer"
                          value={choice.value}
                          checked={answer === choice.value}
                          onChange={() => write(choice.value)}
                        />
                        {choice.label}
                      </label>
                    ))}
                  </div>
                ) : (
                  <label>
                    我的正式答案
                    <textarea
                      ref={answerInput}
                      rows={6}
                      maxLength={262144}
                      value={answer}
                      onChange={(e) => write(e.target.value)}
                      onKeyDown={(e) => {
                        if (
                          e.key === 'Enter' &&
                          e.ctrlKey &&
                          !e.nativeEvent.isComposing &&
                          e.keyCode !== 229
                        ) {
                          e.preventDefault()
                          submit()
                        }
                      }}
                    />
                  </label>
                )}
                <label>
                  作答帮助等级
                  <select value={help} onChange={(e) => write(answer, e.target.value as Help)}>
                    {activity.allowed_help.map((level) => (
                      <option key={level} value={level}>
                        {helpLabels[level]}
                      </option>
                    ))}
                  </select>
                </label>
                <p className="hint">
                  按实际获得的帮助选择；刷新后需重新确认。答案与聊天分开提交；Ctrl+Enter 提交答案。
                </p>
                <Button type="submit" disabled={!answer.trim()}>
                  提交正式答案
                </Button>
              </fieldset>
              {!draftContent && content && !answerable(content) && (
                <p role="alert">此内容的作答类型需要升级客户端，当前禁止提交。</p>
              )}
            </form>
          )}
          {item?.attempt && (
            <section className="notice">
              <h3>已保存的答案</h3>
              <p className="answer-text">{item.attempt.answer}</p>
              <p className="hint">
                帮助状态：{helpLabels[item.attempt.help as Help] ?? item.attempt.help}
              </p>
              <Link to="/spaces/$spaceId/feedback/$attemptId" params={{ spaceId, attemptId: item.attempt.attempt_id }}>查看原答案、接收回执与评估详情</Link>
            </section>
          )}
          {item?.assessment && (
            <section aria-label="教学反馈">
              <h2>本次反馈</h2>
              <p>
                反馈状态：
                {(
                  {
                    accepted: '已接纳',
                    provisional: '待复核',
                    overridden: '已人工更正',
                    voided: '已作废',
                  } as Record<string, string>
                )[item.assessment_decision?.disposition ?? ''] ?? '待核对'}
              </p>
              {item.assessment.items.map((entry) => (
                <p key={entry.rubric_item_id}>
                  {(
                    {
                      pass: '达到要求',
                      partial: '部分达到',
                      fail: '尚未达到',
                      unassessed: '未评估',
                    } as Record<string, string>
                  )[entry.conclusion] ?? entry.conclusion}
                  ：{entry.answer_quote}
                </p>
              ))}
              {item.assessment_decision?.disposition === 'provisional' && (
                <p className="hint">
                  此反馈尚未形成正式学习证据。请展开评估详情查看依据并进行正式复核。
                </p>
              )}
            </section>
          )}
          <div className="actions">
            {allowed('start_diagnostic') && (
              <Button
                disabled={!canWrite || !!inactive}
                onClick={() => void perform({ action: 'start_diagnostic' })}
              >
                开始学习
              </Button>
            )}
            {view.session.state === 'Diagnostic' && allowed('apply_route') && (
              <Button
                disabled={!canWrite || !!inactive}
                onClick={() => void generate('route', 'apply_route')}
              >
                生成学习路线
              </Button>
            )}
            {allowed('issue_activity') && (
              <Button
                disabled={!canWrite || !!inactive}
                onClick={() => void generate('activity', 'issue_activity')}
              >
                生成下一活动
              </Button>
            )}
            {allowed('present_review') && (
              <Button disabled={!canWrite || !!inactive} onClick={() => void generate('activity', 'present_review')}>
                生成本任务复习活动
              </Button>
            )}
            {allowed('record_assessment') && (
              <Button
                disabled={!canWrite}
                onClick={() =>
                  activity?.type === 'objective'
                    ? void perform({ action: 'record_assessment' })
                    : void generate('assessment', 'record_assessment')
                }
              >
                获取教学反馈
              </Button>
            )}
            {allowed('acknowledge_feedback') && (
              <Button
                disabled={!canWrite}
                onClick={() => void perform({ action: 'acknowledge_feedback' })}
              >
                查看完毕，继续学习
              </Button>
            )}
            {allowed('end_activity') && (
              <Button
                variant="outline"
                disabled={!canWrite}
                onClick={() => void perform({ action: 'end_activity' })}
              >
                {activity?.type === 'explanation' ? '阅读完成，继续' : '结束当前活动'}
              </Button>
            )}
            {allowed('complete_session') && (
              <Button
                variant="outline"
                disabled={!canWrite}
                onClick={() => void perform({ action: 'complete_session' })}
              >
                结束本次学习
              </Button>
            )}
          </div>
          {pending && (
            <div className="notice" role="alert">
              <p>正在核对原会话的提交结果，答案不会自动补发。</p>
              <Button disabled={busy} onClick={() => void reconcile()}>
                核对提交状态
              </Button>
            </div>
          )}
          {notice && <p role="status">{notice}</p>}
          {!!error && <ErrorState error={error} retry={() => void refresh().catch(setError)} />}
          <p className="hint">
            活动草稿仅保留在本标签页内存。切页或切换主题可保留；刷新会丢失未保存答案。
          </p>
        </div>
        <aside className="tutor-column panel" aria-label="AI 导师">
          <h2>AI 导师</h2>
          <p className="hint">讨论当前活动。聊天不会作为正式答案。</p>
          {content && goal.data && (
            <ContentEditor
              key={content.artifact_id}
              content={content}
              goal={goal.data}
              selection={selection}
              onUpdate={updateContent}
              disabled={archived}
            />
          )}
          <div
            className="teaching-discussion"
            ref={output}
            onScroll={() => {
              const el = output.current
              if (el) follow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
            }}
          >
            {item?.free_question && (
              <div>
                <h3>我的问题</h3>
                <p className="answer-text">{item.free_question.text}</p>
              </div>
            )}
            {item?.free_answer && (
              <div>
                <h3>导师回复</h3>
                <SafeMarkdown text={item.free_answer.text} />
              </div>
            )}
          </div>
          {allowed('record_free_answer') && (
            <Button
              disabled={!canWrite}
              onClick={() => void generate('free_answer', 'record_free_answer')}
            >
              请导师回答
            </Button>
          )}
          {allowed('resume_focus') && (
            <Button
              disabled={!canWrite}
              variant="outline"
              onClick={() => void perform({ action: 'resume_focus' })}
            >
              返回原学习焦点
            </Button>
          )}
          {allowed('ask_free_question') && (
            <form
              onSubmit={(e) => {
                e.preventDefault()
                ask()
              }}
            >
              <label>
                和导师讨论
                <textarea
                  rows={4}
                  maxLength={4000}
                  disabled={!canWrite || !!inactive}
                  value={chat}
                  onChange={(e) => {
                    setChat(e.target.value)
                    drafts.set(chatKey, e.target.value)
                  }}
                  onKeyDown={(e) => {
                    if (
                      e.key === 'Enter' &&
                      !e.shiftKey &&
                      !e.nativeEvent.isComposing &&
                      e.keyCode !== 229
                    ) {
                      e.preventDefault()
                      ask()
                    }
                  }}
                />
              </label>
              <Button type="submit" disabled={!canWrite || !!inactive || !chat.trim()}>
                发送讨论
              </Button>
              <p className="hint">Enter 发送，Shift+Enter 换行。</p>
              {activity && (
                <div className="actions">
                  {activity.allowed_help
                    .filter((level) => level !== 'none')
                    .map((level) => (
                      <Button
                        key={level}
                        variant="outline"
                        disabled={!canWrite || !!inactive}
                        onClick={() => {
                          const order: Help[] = ['none', 'hint', 'scaffold', 'answer_revealed']
                          const actual = order[Math.max(order.indexOf(help), order.indexOf(level))]
                          write(answer, actual)
                          ask(
                            (
                              {
                                hint: '请给当前题目一个提示，不揭示答案。',
                                scaffold: '请分步引导我理解当前题目。',
                                answer_revealed: '请解释当前题目的答案。',
                              } as Record<string, string>
                            )[level],
                          )
                        }}
                      >
                        {
                          (
                            {
                              hint: '请求提示',
                              scaffold: '请求分步引导',
                              answer_revealed: '查看答案讲解',
                            } as Record<string, string>
                          )[level]
                        }
                      </Button>
                    ))}
                </div>
              )}
            </form>
          )}
          {goal.data && (
            <ReferenceLink spaceId={spaceId} goalId={goal.data.goal_id} sessionId={sessionId} status />
          )}
          {goal.data && (
            <details>
              <summary>目标内持续交流</summary>
              <p className="hint">导师调用正式变更服务后，具体差异和生效状态显示在下方。</p>
              <TutorHistory key={sessionId} spaceId={spaceId} goalId={goal.data.goal_id} teachingSessionId={sessionId} />
            </details>
          )}
          {goal.data && <ChangePanel key={`${spaceId}:${sessionId}`} goal={goal.data} teachingSessionId={sessionId} archived={archived} onChanged={() => setContentRefresh((v) => v + 1)} onRestore={(activityId) => { restoreFocusRequested.current = activityId }} />}
          <label className="column-size">
            导师栏宽度
            <input
              type="range"
              min={340}
              max={400}
              value={mentorWidth}
              onChange={(e) => setMentorWidth(Number(e.target.value))}
            />
          </label>
        </aside>
      </div>
    </section>
  )
}

export function ContentPage({
  artifactId,
  spaceId,
  version,
}: {
  artifactId: string
  spaceId: string
  version?: number
}) {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const client = () => learningClient(session, spaceId)
  const preference = useQuery({
    queryKey: [...prefix, spaceId, artifactId, 'content-preference'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content/{artifactID}/preferences', {
          params: { path: { artifactID: artifactId }, header: contentHeader(spaceId) },
          signal,
        }),
        preferenceSchema,
      ),
  })
  const readingVersion = version ?? preference.data?.pinned_version ?? undefined
  const capabilities = useQuery({
    queryKey: [...prefix, 'content-capabilities'],
    queryFn: ({ signal }) =>
      unwrap(client().GET('/v1/learning/content/capabilities', { signal }), contentCapabilities),
  })
  const content = useQuery({
    queryKey: [...prefix, spaceId, artifactId, readingVersion, 'content'],
    enabled:
      capabilities.data?.protocol_version === 1 && capabilities.data.available && !!preference.data,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content/{artifactID}', {
          params: {
            path: { artifactID: artifactId },
            header: contentHeader(spaceId),
            query: { version: readingVersion },
          },
          signal,
        }),
        contentSchema,
      ),
    gcTime: 0,
  })
  const history = useQuery({
    queryKey: [...prefix, spaceId, artifactId, 'content-history'],
    enabled: !!content.data,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content/{artifactID}/revisions', {
          params: { path: { artifactID: artifactId }, header: contentHeader(spaceId) },
          signal,
        }),
        contentHistory,
      ),
    gcTime: 0,
  })
  const selection = useContentSelection(content.data)
  const goal = useQuery({
    queryKey: [...prefix, spaceId, content.data?.goal_id, 'latest'],
    enabled: !!content.data,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: content.data!.goal_id } },
          signal,
        }),
        goalSchema,
      ),
  })
  if (content.error || capabilities.error || preference.error)
    return <ErrorState error={content.error || capabilities.error || preference.error} />
  if (
    capabilities.data &&
    (!capabilities.data.available || capabilities.data.protocol_version !== 1)
  )
    return <p role="alert">此服务器尚未启用受支持的版本化内容协议。</p>
  if (!content.data) return <p role="status">正在读取内容版本…</p>
  const value = content.data
  if (value.learning_space_id !== spaceId || value.privacy_generation !== session.generation)
    return <ErrorState error={new ApiError(404, 'wrong_content')} />
  return (
    <section className="panel content-page">
      <Link
        to="/spaces/$spaceId/learn/$sessionId"
        params={{ spaceId, sessionId: value.session_id }}
      >
        ← 返回教学会话
      </Link>
      <h1>学习内容 · 第 {value.version} 版</h1>
      <Link to="/spaces/$spaceId/studio" params={{ spaceId }}>
        查看 Studio 内容库
      </Link>
      {preference.data?.pinned_version && (
        <p className="notice">
          已固定阅读第 {preference.data.pinned_version} 版。新的加工版本不会自动替换固定版本。
        </p>
      )}
      <p role="status">
        {
          (
            {
              committed: '正式版本',
              draft: '未提交草稿',
              failed: '生成失败，保留部分输出',
            } as Record<string, string>
          )[value.status]
        }
        。此页面只供阅读，正式作答请返回原会话。
      </p>
      <ContentBlocks
        blocks={value.body.blocks}
        references={value.body.references}
        content={value}
        onSelect={(b, start, end) => void selection.select(b, start, end)}
      />
      {goal.data && (
        <ContentEditor
          key={value.artifact_id}
          content={value}
          goal={goal.data}
          selection={selection}
          onUpdate={() => {
            void history.refetch()
            if (!readingVersion) void content.refetch()
          }}
          onRelocate={(next) =>
            void navigate({
              to: '/content/$artifactId',
              params: { artifactId: next.artifact_id },
              search: { space: spaceId, version: next.version },
            })
          }
        />
      )}
      <ContentTools
        content={value}
        preference={preference.data!}
        refreshPreference={() => void preference.refetch()}
      />
      <section aria-label="内容版本历史">
        <h2>版本历史</h2>
        {history.error && <ErrorState error={history.error} />}
        {history.data?.items.map((item) => (
          <p key={item.version}>
            <Link
              to="/content/$artifactId"
              params={{ artifactId }}
              search={{ space: spaceId, version: item.version }}
            >
              第 {item.version} 版 ·{' '}
              {item.status === 'committed' ? '正式' : item.status === 'draft' ? '草稿' : '失败'}
              {' · '}
              {new Date(item.created_at).toLocaleString('zh-CN')}
            </Link>
          </p>
        ))}
      </section>
      <details>
        <summary>生成与来源依据</summary>
        <p>
          模型：{value.body.model_id}
          <br />
          输入指纹：{value.body.input_fingerprint}
          <br />
          原活动语义指纹：{value.body.semantic_fingerprint}
        </p>
      </details>
    </section>
  )
}
