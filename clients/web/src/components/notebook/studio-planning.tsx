import { useEffect, useRef, useState, type PointerEvent } from 'react'
import { ArrowDown, ArrowUp, GripVertical, Trash2 } from 'lucide-react'
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import type { Space } from '@/api/runtime'
import { settingsSchema } from '@/api/settings'
import {
  studioJobsAPI,
  studioJobStatus,
  type StudioJob,
  type StudioJobCreate,
  type StudioPlanOperation,
} from '@/api/studio-jobs'
import type { StudioRevision } from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { Button } from '../ui/button'
import { studioNames } from './studio-generation'
import { useSourceSelection } from './source-selection'
import { StudioJobProgress } from './studio-job-progress'

export function StudioJobs({ spaceId, onOpen }: { spaceId: string; onOpen: (id: string) => void }) {
  const { session, prefix, drafts } = useIdentity()
  const jobs = useInfiniteQuery({
    queryKey: [...prefix, spaceId, 'studio-jobs'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) => studioJobsAPI(session, spaceId).list(pageParam, signal),
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
    gcTime: 0,
    refetchInterval: 3000,
  })
  useEffect(() => {
    if (deniedError(jobs.error))
      drafts.deletePrefix(
        JSON.stringify([...prefix, spaceId, 'studio-planning']).slice(0, -1) + ',',
      )
  }, [jobs.error, drafts, prefix, spaceId])
  return (
    <section aria-label="生成任务">
      <div className="studio-section-head">
        <h3>生成任务</h3>
      </div>
      {jobs.isPending && <p role="status">正在读取任务…</p>}
      {jobs.error ? (
        <ErrorState error={jobs.error} retry={() => void jobs.refetch()} />
      ) : (
        <div className="studio-notes-list">
          {jobs.data?.pages
            .flatMap((page) => page.items)
            .filter((job) => job.status !== 'deleted')
            .map((job) => (
              <Button
                key={job.id}
                variant="outline"
                className="studio-note-row"
                onClick={() => onOpen(job.id)}
              >
                <span className="studio-output-info">
                  <span className="studio-output-title">
                    {job.status === 'invalidated'
                      ? `${studioNames[job.kind]}任务`
                      : job.plan?.title || `${studioNames[job.kind]}大纲`}
                  </span>
                  <small>{studioJobStatus[job.status]}</small>
                </span>
              </Button>
            ))}
          {jobs.data?.pages[0]?.items.length === 0 && <p className="hint">还没有生成任务。</p>}
          {jobs.hasNextPage && (
            <Button
              variant="outline"
              disabled={jobs.isFetchingNextPage}
              onClick={() => void jobs.fetchNextPage()}
            >
              更多任务
            </Button>
          )}
        </div>
      )}
    </section>
  )
}

type CreationDraft = { focus: string; pending?: StudioJobCreate }
export function StudioPlanCreator({
  space,
  kind,
  base,
  onBack,
  onCreated,
}: {
  space: Space
  kind: StudioJob['kind']
  base?: StudioRevision
  onBack: () => void
  onCreated: (job: StudioJob) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const { selection } = useSourceSelection()
  const key = JSON.stringify([
    ...prefix,
    space.id,
    'studio-planning',
    'create',
    base?.artifact_id ?? kind,
  ])
  const [draft, setDraft] = useState<CreationDraft>(() => drafts.get(key) ?? { focus: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const api = studioJobsAPI(session, space.id)
  const suggested = useQuery({
    queryKey: [...prefix, space.id, 'studio-focus'],
    queryFn: ({ signal }) => api.focus(signal),
    retry: false,
    gcTime: 0,
  })
  const settings = useQuery({
    queryKey: [...prefix, 'settings'],
    queryFn: ({ signal }) =>
      unwrap(learningClient(session).GET('/v1/settings', { signal }), settingsSchema),
    retry: false,
  })
  const model = settings.data?.effective_mentor
  const denied = deniedError(error) || deniedError(suggested.error)
  useEffect(() => {
    if (denied) {
      drafts.delete(key)
      setDraft({ focus: '' })
    }
  }, [denied, drafts, key])
  const persist = (next: CreationDraft) => {
    setDraft(next)
    if (next.focus || next.pending) drafts.set(key, next)
    else drafts.delete(key)
  }
  const create = async () => {
    if (busy || !selection || selection.mode !== 'items') return
    const body = draft.pending ?? {
      operation_id: crypto.randomUUID(),
      privacy_generation: session.generation,
      kind,
      source_selection: selection,
      direction_version: space.direction_version ?? 0,
      ...(draft.focus.trim() ? { focus: draft.focus.trim() } : {}),
      ...(base ? { artifact_id: base.artifact_id, expected_artifact_version: base.version } : {}),
    }
    persist({ ...draft, pending: body })
    setBusy(true)
    setError(undefined)
    try {
      const job = await api.create(body)
      if (!alive.current) return
      drafts.delete(key)
      onCreated(job)
    } catch (e) {
      if (!alive.current) return
      setError(e)
      if (e instanceof ApiError && e.status >= 400 && e.status < 500)
        persist({ focus: draft.focus })
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  if (denied)
    return (
      <section>
        <Button onClick={onBack}>返回 Studio</Button>
        <p>当前无法读取规划所需内容，已清除草稿。</p>
      </section>
    )
  return (
    <section className="studio-planning" aria-label="生成大纲设置">
      <Button variant="outline" onClick={onBack}>
        返回 Studio
      </Button>
      <h3>{studioNames[kind]}大纲</h3>
      <p>先查看并修改大纲，确认后保存为待生成。</p>
      <p className="hint">学习方向：{space.direction || '尚未设置，本次按所选资料规划'}</p>
      <p className="hint">
        使用当前勾选的 {selection?.items.length ?? 0}{' '}
        个来源和本学习区的学习记录。任务保存后，后续勾选不会改变这份大纲。
      </p>
      <label>
        侧重点（可选）
        <textarea
          rows={3}
          maxLength={2000}
          value={draft.focus}
          disabled={busy || !!draft.pending}
          placeholder="例如：先复习容易混淆的概念"
          onChange={(event) => persist({ focus: event.target.value })}
        />
      </label>
      {!!suggested.data?.suggested_focus.length && (
        <div className="studio-focus-tags" aria-label="侧重点建议">
          {suggested.data.suggested_focus.map((tag) => (
            <Button
              key={tag}
              variant="outline"
              disabled={busy || !!draft.pending}
              onClick={() =>
                persist({ focus: draft.focus ? `${draft.focus}；${tag}`.slice(0, 2000) : tag })
              }
            >
              {tag}
            </Button>
          ))}
        </div>
      )}
      {(suggested.error || suggested.data?.coverage === 'unavailable') && (
        <p className="hint">学习记录暂不可用，本次按资料生成。</p>
      )}
      <p className="hint">未发送的文字只在本标签页保留，刷新会丢失。已发送的修改随任务保存。</p>
      {model?.configured && (
        <p className="hint">
          大纲与修改将发送到已配置的模型：{model.model}（{model.endpoint}）。
        </p>
      )}
      {!model?.enabled || !model.configured ? <p>请先在设置中启用并配置模型。</p> : null}
      {(!selection || selection.mode === 'none') && <p>请先勾选至少一个可读取的来源。</p>}
      {draft.pending && !busy && (
        <p role="status">提交结果待核对。重试将使用同一请求和原来源选择。</p>
      )}
      {!!error && <ErrorState error={error} />}
      <Button
        disabled={busy || !model?.enabled || !model.configured || !selection?.items.length}
        onClick={() => void create()}
      >
        {busy ? '正在提交…' : draft.pending ? '核对并重试原请求' : '生成大纲'}
      </Button>
    </section>
  )
}

type MessageDraft = { text: string; pending?: StudioPlanOperation & { text: string } }
const deniedError = (error: unknown) =>
  error instanceof ApiError &&
  ([401, 403, 404].includes(error.status) ||
    ['content_redacted', 'privacy_clear_in_progress'].includes(error.code))
const failureReasons: Record<string, string> = {
  source_unavailable: '所选资料已不可读取，请检查来源后重新建立任务。',
  invalid_source_citation: '引用未通过核验，当前部分未保存，可以重试。',
  schema_mismatch: '模型返回的格式不完整，可以重试。',
  output_budget_exceeded: '模型达到单次输出上限，请调整单次输出配置后重试。',
  response_not_completed: '模型未完成输出，可以重试。',
  context_changed: '生成设置或权限已变化，请核对后继续；更换服务地址需重新建立任务。',
}

export function StudioPlanning({
  spaceId,
  jobId,
  canWrite,
  onBack,
  onOpen,
}: {
  spaceId: string
  jobId: string
  canWrite: boolean
  onBack: () => void
  onOpen: (id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const api = studioJobsAPI(session, spaceId)
  const queryKey = [...prefix, spaceId, 'studio-job', jobId]
  const key = JSON.stringify([...prefix, spaceId, 'studio-planning', jobId])
  const [draft, setDraft] = useState<MessageDraft>(() => drafts.get(key) ?? { text: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [notice, setNotice] = useState('')
  const [dragging, setDragging] = useState<string>()
  const [dragTarget, setDragTarget] = useState<string>()
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const job = useQuery({
    queryKey,
    queryFn: ({ signal }) => api.get(jobId, signal),
    retry: false,
    gcTime: 0,
    refetchInterval: (q) =>
      ['planning', 'queued', 'generating', 'summarizing', 'stopping'].includes(
        q.state.data?.status ?? '',
      )
        ? 700
        : 5000,
  })
  const state = job.data
  const persist = (next: MessageDraft) => {
    setDraft(next)
    if (next.text || next.pending) drafts.set(key, next)
    else drafts.delete(key)
  }
  const denied = deniedError(job.error) || deniedError(error)
  const unavailable = state?.status === 'invalidated' || state?.status === 'deleted'
  useEffect(() => {
    if (denied || unavailable) {
      drafts.delete(key)
      setDraft({ text: '' })
    }
  }, [denied, unavailable, drafts, key])
  useEffect(() => {
    const sent = state?.messages.find(
      (message) => message.operation_id === draft.pending?.operation_id,
    )
    if (sent?.status === 'succeeded') persist({ text: '' })
    else if (sent?.status === 'failed') persist({ text: draft.text })
  }, [state?.messages, draft.pending?.operation_id])
  const update = (next: StudioJob) => {
    query.setQueryData(queryKey, next)
    void query.invalidateQueries({ queryKey: [...prefix, spaceId, 'studio-jobs'] })
  }
  const operation = (action: string, payload: object = {}): StudioPlanOperation => {
    const body = {
      expected_version: state!.version,
      plan_revision: state!.active_plan_revision,
      ...payload,
    }
    return { ...body, operation_id: drafts.operation(`${key}:${action}`, body) }
  }
  const mutate = async (fn: () => Promise<StudioJob>, success?: string) => {
    if (busy || !canWrite) return
    setBusy(true)
    setError(undefined)
    setNotice('')
    try {
      await query.cancelQueries({ queryKey })
      const next = await fn()
      if (!alive.current) return
      update(next)
      if (success) setNotice(success)
      if (next.status === 'deleted') {
        drafts.delete(key)
        onBack()
      }
    } catch (e) {
      if (!alive.current) return
      setError(e)
      if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
        const currentDraft = drafts.get<MessageDraft>(key)
        if (currentDraft?.pending) persist({ text: currentDraft.text })
      }
      if (e instanceof ApiError && e.status === 409) {
        setNotice('大纲已在其他页面修改。你的输入已保留，正在读取最新内容，请核对后再操作。')
      }
      void job.refetch()
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  const sections = state?.plan?.sections ?? []
  const editable = canWrite && state?.status === 'awaiting_confirmation' && !busy && !draft.pending
  const move = (id: string, targetId: string) => {
    if (!editable || id === targetId) return
    const ids = sections.map((section) => section.id)
    const from = ids.indexOf(id),
      to = ids.indexOf(targetId)
    if (from < 0 || to < 0) return
    ids.splice(to, 0, ...ids.splice(from, 1))
    void mutate(
      () => api.patch(jobId, { ...operation('reorder', { ordered_ids: ids }), ordered_ids: ids }),
      '大纲顺序已保存。',
    )
  }
  const pointerTarget = (event: PointerEvent<HTMLButtonElement>) =>
    document
      .elementFromPoint(event.clientX, event.clientY)
      ?.closest<HTMLElement>('[data-outline-section]')?.dataset.outlineSection
  const send = () => {
    if (!state || !draft.text.trim()) return
    const body = draft.pending ?? {
      ...operation('message', { text: draft.text.trim() }),
      text: draft.text.trim(),
    }
    persist({ ...draft, pending: body })
    void mutate(() => api.message(jobId, body))
  }
  if (denied)
    return (
      <section>
        <Button onClick={onBack}>返回 Studio</Button>
        <p>当前无法读取此任务，已隐藏内容并清除草稿。</p>
      </section>
    )
  if (job.error)
    return (
      <section>
        <Button onClick={onBack}>返回 Studio</Button>
        <ErrorState error={job.error} retry={() => void job.refetch()} />
      </section>
    )
  if (!state) return <p role="status">正在读取大纲…</p>
  return (
    <section className="studio-planning" aria-label="Studio 大纲">
      <Button variant="outline" onClick={onBack}>
        返回 Studio
      </Button>
      <h3>
        {unavailable
          ? `${studioNames[state.kind]}任务`
          : state.plan?.title || `${studioNames[state.kind]}大纲`}
      </h3>
      <p role="status">{studioJobStatus[state.status]}</p>
      {state.confirmed_plan_revision > 0 && !unavailable && (
        <StudioJobProgress
          spaceId={spaceId}
          job={state}
          busy={busy}
          canWrite={canWrite}
          onOpen={onOpen}
          onCommand={(kind) => void mutate(() => api.command(jobId, { ...operation(kind), kind }))}
        />
      )}
      {state.status === 'planning' && <p>正在依据所选资料整理大纲，离开后仍可重开查看。</p>}
      {state.status === 'failed' && (
        <p>
          {failureReasons[state.reason ?? ''] ??
            (state.confirmed_plan_revision
              ? '本次生成未完成，已保留通过核验的部分。'
              : '本次大纲修改未完成，已保留之前的大纲和已发送的修改。')}
        </p>
      )}
      {unavailable ? (
        <p>此任务已不可继续。请返回 Studio 查看其他任务。</p>
      ) : (
        <>
          {state.plan && (
            <>
              <p className="hint">
                共 {sections.length} 部分 · 预计{' '}
                {sections.reduce((sum, section) => sum + section.unit_count, 0)}{' '}
                小段。更改来源勾选或学习方向不会改变本次任务。
              </p>
              {state.plan.coverage === 'partial' && (
                <p className="studio-coverage-note">大纲仅覆盖所选资料的部分范围。</p>
              )}
              {!!state.plan.uncovered.length && (
                <details>
                  <summary>未覆盖的范围</summary>
                  <ul>
                    {state.plan.uncovered.map((item, index) => (
                      <li key={index}>{item}</li>
                    ))}
                  </ul>
                </details>
              )}
              <ol className="studio-outline" aria-label="大纲部分">
                {sections.map((section, index) => (
                  <li
                    key={section.id}
                    data-outline-section={section.id}
                    data-dragging={dragging === section.id}
                    data-drop-target={dragTarget === section.id}
                  >
                    <div className="studio-outline-title">
                      <Button
                        className="studio-drag-handle"
                        variant="ghost"
                        disabled={!editable}
                        aria-label={`拖动排序：${section.topic}`}
                        aria-describedby={
                          state.status === 'awaiting_confirmation' ? 'studio-sort-help' : undefined
                        }
                        onPointerDown={(event) => {
                          if (!editable || event.button !== 0) return
                          event.currentTarget.setPointerCapture(event.pointerId)
                          setDragging(section.id)
                        }}
                        onPointerMove={(event) => {
                          if (dragging) setDragTarget(pointerTarget(event))
                        }}
                        onPointerCancel={() => {
                          setDragging(undefined)
                          setDragTarget(undefined)
                        }}
                        onPointerUp={(event) => {
                          const target = pointerTarget(event)
                          if (dragging && target) move(dragging, target)
                          setDragging(undefined)
                          setDragTarget(undefined)
                        }}
                        onKeyDown={(event) => {
                          const target =
                            event.key === 'ArrowUp'
                              ? sections[index - 1]
                              : event.key === 'ArrowDown'
                                ? sections[index + 1]
                                : undefined
                          if (event.key === 'ArrowUp' || event.key === 'ArrowDown')
                            event.preventDefault()
                          if (target) move(section.id, target.id)
                        }}
                      >
                        <GripVertical size={18} aria-hidden="true" />
                      </Button>
                      <h4>{section.topic}</h4>
                    </div>
                    <p>{section.objective}</p>
                    <p className="hint">
                      章节：{section.chapters.map((chapter) => chapter.title).join('、')}
                    </p>
                    {section.unit_count > 1 && (
                      <p className="hint">预计分为 {section.unit_count} 小段</p>
                    )}
                    {state.status === 'awaiting_confirmation' && (
                      <div className="studio-outline-actions">
                        <Button
                          variant="ghost"
                          disabled={!editable || index === 0}
                          aria-label={`上移：${section.topic}`}
                          onClick={() => move(section.id, sections[index - 1].id)}
                        >
                          <ArrowUp size={15} aria-hidden="true" />
                          上移
                        </Button>
                        <Button
                          variant="ghost"
                          disabled={!editable || index === sections.length - 1}
                          aria-label={`下移：${section.topic}`}
                          onClick={() => move(section.id, sections[index + 1].id)}
                        >
                          <ArrowDown size={15} aria-hidden="true" />
                          下移
                        </Button>
                        <Button
                          variant="ghost"
                          disabled={!editable || sections.length <= 1}
                          aria-label={`删除：${section.topic}`}
                          onClick={() =>
                            void mutate(
                              () =>
                                api.patch(jobId, {
                                  ...operation('delete-section', { delete_ids: [section.id] }),
                                  delete_ids: [section.id],
                                }),
                              '已删除该部分。',
                            )
                          }
                        >
                          <Trash2 size={15} aria-hidden="true" />
                          删除
                        </Button>
                      </div>
                    )}
                  </li>
                ))}
              </ol>
              {state.status === 'awaiting_confirmation' && (
                <p id="studio-sort-help" className="hint">
                  拖动手柄排序，也可聚焦手柄后按上下方向键，或使用上移、下移按钮。
                </p>
              )}
            </>
          )}
          {!!state.messages.length && (
            <section className="studio-plan-messages" aria-label="大纲修改记录">
              <h4>修改记录</h4>
              {state.messages.map((message) => (
                <article key={message.operation_id}>
                  <p className="studio-plan-request">{message.text}</p>
                  {message.explanation && <p>{message.explanation}</p>}
                  {message.status !== 'succeeded' && (
                    <small>
                      {message.status === 'pending' ? '正在修改…' : '这次修改未完成，可重试。'}
                    </small>
                  )}
                </article>
              ))}
            </section>
          )}
          {state.status === 'failed' && !state.confirmed_plan_revision && canWrite && (
            <Button
              disabled={busy}
              onClick={() =>
                void mutate(() => api.command(jobId, { ...operation('retry'), kind: 'retry' }))
              }
            >
              重试大纲
            </Button>
          )}
          {!!error && <ErrorState error={error} />}
          {notice && <p role="status">{notice}</p>}
          {!state.confirmed_plan_revision &&
            (state.status === 'awaiting_confirmation' ||
              state.status === 'planning' ||
              state.status === 'failed') && (
              <div className="studio-plan-composer">
                <label>
                  修改大纲
                  <textarea
                    rows={3}
                    maxLength={4000}
                    value={draft.text}
                    placeholder="例如：先讲基础概念，把最后两节合并"
                    disabled={!canWrite || busy || state.status === 'planning' || !!draft.pending}
                    onChange={(event) => persist({ text: event.target.value })}
                  />
                </label>
                <p className="hint">修改只用于这份大纲。未发送文字只保留在本标签页，刷新会丢失。</p>
                <div className="studio-plan-buttons">
                  <Button
                    variant="outline"
                    disabled={
                      !canWrite ||
                      busy ||
                      state.status !== 'awaiting_confirmation' ||
                      !draft.text.trim()
                    }
                    onClick={send}
                  >
                    {draft.pending ? '核对并重试修改' : '发送修改'}
                  </Button>
                  <Button
                    disabled={!editable || !sections.length || !!draft.text.trim()}
                    onClick={() => void mutate(() => api.confirm(jobId, operation('confirm')))}
                  >
                    确认大纲
                  </Button>
                </div>
                {!!draft.text.trim() && !draft.pending && (
                  <p className="hint">请先发送或清空修改，再确认大纲。</p>
                )}
              </div>
            )}
        </>
      )}
      {canWrite && state.status !== 'deleted' && (
        <details className="studio-job-delete">
          <summary>删除任务</summary>
          <p>将删除这份大纲和已经发送的修改记录。</p>
          <Button
            variant="destructive"
            disabled={busy}
            onClick={() =>
              void mutate(() => api.command(jobId, { ...operation('delete'), kind: 'delete' }))
            }
          >
            确认删除任务
          </Button>
        </details>
      )}
    </section>
  )
}
