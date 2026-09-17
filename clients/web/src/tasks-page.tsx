import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { learningClient, unwrap, ApiError } from './api/client'
import { goalSchema, pageOf, spaceSchema } from './api/runtime'
import { taskAdapters, taskKinds, taskRunStatus, type TaskKind, type RunKind } from './api/tasks'
import { collectionSchema, importAPI, importStatus, type ImportJob } from './api/import-jobs'
import {
  mentorSnapshotSchema,
  mentorReceiptSchema,
  isMentorTerminal,
  observeMentor,
  type MentorSnapshot,
} from './api/mentor'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { ErrorState, Pagination, Confirm } from './components/common'
import { ImportCounts, ImportError } from './import-page'
import { SafeMarkdown } from './components/content-blocks'

export function TasksPage({ spaceId, collectionId }: { spaceId: string; collectionId?: string }) {
  const { session, prefix } = useIdentity()
  const [kind, setKind] = useState<TaskKind | ''>('')
  const spaces = useQuery({
    queryKey: [...prefix, 'task-spaces'],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces', {
          params: { query: { limit: 100 } },
          signal,
        }),
        pageOf(spaceSchema),
      ),
  })
  const collections = useQuery({
    queryKey: [...prefix, spaceId, 'task-collections'],
    enabled: session.capabilities.import_jobs && session.device.scopes.includes('knowledge:read'),
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/collections', { signal }),
        pageOf(collectionSchema),
      ),
  })
  const collection = collectionId ?? collections.data?.items[0]?.id
  return (
    <>
      <h1>任务中心</h1>
      <p>按原业务状态查看、恢复和核对任务。关闭详情只停止观察，停止任务需要明确取消。</p>
      <nav aria-label="任务学习区" className="actions">
        {spaces.data?.items.map((space) => (
          <Link
            key={space.id}
            to="/runs"
            search={{ space: space.id }}
            aria-current={space.id === spaceId ? 'page' : undefined}
          >
            {space.name}
            {space.status === 'archived' ? '（已归档）' : ''}
          </Link>
        ))}
      </nav>
      {spaces.data?.next_cursor && <Link to="/">更多学习区请从学习区列表进入</Link>}
      <ImportError error={spaces.error || collections.error} />
      <label>
        任务类型
        <select value={kind} onChange={(e) => setKind(e.target.value as TaskKind | '')}>
          <option value="">全部已接入类型</option>
          {Object.entries(taskKinds)
            .filter(([key]) =>
              key === 'import_job' ? session.capabilities.import_jobs : session.capabilities.runs,
            )
            .map(([key, label]) => (
              <option key={key} value={key}>
                {label}
              </option>
            ))}
        </select>
      </label>
      {session.capabilities.import_jobs && (!kind || kind === 'import_job') && (
        <section aria-label="资料导入任务">
          <h2>资料导入</h2>
          <nav aria-label="导入集合" className="actions">
            {collections.data?.items.map((item) => (
              <Link
                key={item.id}
                to="/runs"
                search={{ space: spaceId, collection: item.id }}
                aria-current={item.id === collection ? 'page' : undefined}
              >
                {item.name}
              </Link>
            ))}
          </nav>
          {collection && (
            <>
              <Link
                className="button"
                to="/runs/import/$jobId"
                params={{ jobId: crypto.randomUUID() }}
                search={{ space: spaceId, collection, draft: true }}
              >
                新建可恢复导入
              </Link>
              <ImportTaskList
                key={`${spaceId}:${collection}`}
                spaceId={spaceId}
                collectionId={collection}
              />
            </>
          )}
          {collections.data?.items.length === 0 && (
            <p>当前区没有可用资料集合，请先通过资料集合入口关联集合。</p>
          )}
        </section>
      )}
      {session.capabilities.runs && kind !== 'import_job' && (
        <RunList key={`${spaceId}:${kind}`} spaceId={spaceId} kind={kind || undefined} />
      )}
    </>
  )
}

function ImportTaskList({ spaceId, collectionId }: { spaceId: string; collectionId: string }) {
  const { session, prefix } = useIdentity()
  const [cursors, setCursors] = useState<(string | undefined)[]>([undefined])
  const [page, setPage] = useState(0)
  const cursor = cursors.at(-1)
  const query = useQuery({
    queryKey: [...prefix, spaceId, collectionId, 'import-jobs', cursor],
    queryFn: ({ signal }) => taskAdapters.imports(session, spaceId, collectionId, cursor, signal),
    gcTime: 0,
  })
  if (query.error) return <ErrorState error={query.error} retry={() => void query.refetch()} />
  const items = query.data?.items ?? []
  return (
    <>
      <Button variant="outline" onClick={() => void query.refetch()}>
        刷新导入列表
      </Button>
      {query.isPending && <p role="status">正在读取导入任务…</p>}
      {!query.isPending && items.length === 0 && <p>此设备在该集合没有导入任务。</p>}
      {items.slice(page * 10, page * 10 + 10).map((job) => (
        <ImportTaskCard key={job.id} job={job} />
      ))}
      <Pagination
        page={(cursors.length - 1) * 10 + page + 1}
        previous={
          page || cursors.length > 1
            ? () => {
                if (page) setPage(page - 1)
                else {
                  setCursors(cursors.slice(0, -1))
                  setPage(9)
                }
              }
            : undefined
        }
        next={
          (page + 1) * 10 < items.length || query.data?.next_cursor
            ? () => {
                if ((page + 1) * 10 < items.length) setPage(page + 1)
                else {
                  setCursors([...cursors, query.data?.next_cursor])
                  setPage(0)
                }
              }
            : undefined
        }
      />
    </>
  )
}
function ImportTaskCard({ job }: { job: ImportJob }) {
  const { session, prefix } = useIdentity()
  const current = useQuery({
    queryKey: [...prefix, job.space_id, job.collection_id, 'import-job', job.id],
    queryFn: ({ signal }) =>
      importAPI(session, job.space_id, job.collection_id, signal).get(job.id),
    gcTime: 0,
  })
  const value = current.data ?? job
  return (
    <article className="panel">
      <h3>资料导入 · {importStatus[value.status]}</h3>
      <p>任务：{job.id}</p>
      <p>
        学习区 {job.space_id} · 集合 {job.collection_id}
      </p>
      <ImportError error={current.error} />
      {current.data && !current.error && (
        <>
          <ImportCounts job={current.data} />
          {current.data.batches
            .filter((batch) => batch.error)
            .slice(0, 3)
            .map((batch) => (
              <p role="alert" key={batch.operation_id}>
                {batch.item.path}：{batch.error}
              </p>
            ))}
        </>
      )}
      <Link
        to="/runs/import/$jobId"
        params={{ jobId: job.id }}
        search={{ space: job.space_id, collection: job.collection_id }}
      >
        查看导入与核对原操作
      </Link>
    </article>
  )
}
function RunList({ spaceId, kind }: { spaceId: string; kind?: RunKind }) {
  const { session, prefix } = useIdentity()
  const [status, setStatus] = useState<MentorSnapshot['status'] | ''>('')
  const [cursors, setCursors] = useState<(string | undefined)[]>([undefined])
  const query = useQuery({
    queryKey: [...prefix, spaceId, 'run-list', kind, status, cursors.at(-1)],
    queryFn: ({ signal }) =>
      taskAdapters.runs(session, spaceId, kind, status || undefined, cursors.at(-1), signal),
    gcTime: 0,
  })
  return (
    <section aria-label="研究与内容运行">
      <h2>研究与内容运行</h2>
      <label>
        运行状态
        <select
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as typeof status)
            setCursors([undefined])
          }}
        >
          <option value="">全部原始状态</option>
          {Object.entries(taskRunStatus).map(([value, label]) => (
            <option key={value} value={value}>
              {label}（{value}）
            </option>
          ))}
        </select>
      </label>
      <Button variant="outline" onClick={() => void query.refetch()}>
        刷新运行列表
      </Button>
      {query.error && <ErrorState error={query.error} retry={() => void query.refetch()} />}
      {query.isPending && <p role="status">正在读取运行…</p>}
      {!query.error &&
        query.data?.items.map((run) => (
          <article className="panel" key={run.run_id}>
            <h3>
              {taskKinds[run.kind ?? 'mentor']} · {taskRunStatus[run.status]}
            </h3>
            <p>
              阶段：{run.stage} · 学习区 {run.space_id} · 目标 {run.goal_id}
            </p>
            <p>{run.reason}</p>
            <p>
              请求已用 {run.requests_used} · 剩余 {run.requests_left}
            </p>
            {run.result_unknown && <p role="alert">结果未知，需核对原运行；不承诺自动重试。</p>}
            {run.cost_unknown && <p>提供商费用尚未核实。</p>}
            <Link to="/runs/run/$runId" params={{ runId: run.run_id }} search={{ space: spaceId }}>
              查看原运行
            </Link>
          </article>
        ))}
      {query.data?.items.length === 0 && <p>此筛选下没有当前设备的运行。</p>}
      <Pagination
        page={cursors.length}
        previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
        next={
          query.data?.next_cursor
            ? () => setCursors([...cursors, query.data?.next_cursor])
            : undefined
        }
      />
    </section>
  )
}

export function RunPage({ spaceId, runId }: { spaceId: string; runId: string }) {
  const { session, prefix, drafts } = useIdentity()
  const [run, setRun] = useState<MentorSnapshot>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [requests, setRequests] = useState(1)
  const [tokens, setTokens] = useState(30000)
  const [answer, setAnswer] = useState('')
  const mounted = useRef(true)
  const controller = useRef<AbortController | null>(null)
  const header = { 'X-Learning-Space-ID': spaceId }
  const path = { runID: runId }
  const load = async (signal?: AbortSignal) => {
    const next = await unwrap(
      learningClient(session, spaceId).GET('/v1/learning/runs/{runID}', {
        params: { path, header },
        signal,
      }),
      mentorSnapshotSchema,
    )
    if (
      next.space_id !== spaceId ||
      next.run_id !== runId ||
      next.privacy_generation !== session.generation
    )
      throw new ApiError(403, 'run_forbidden')
    return next
  }
  const query = useQuery({
    queryKey: [...prefix, spaceId, 'task-run', runId],
    queryFn: ({ signal }) => load(signal),
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
  const goal = useQuery({
    queryKey: [...prefix, spaceId, run?.goal_id, 'latest'],
    enabled: !!run,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: run!.goal_id } },
          signal,
        }),
        goalSchema,
      ),
  })
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      controller.current?.abort()
    }
  }, [])
  useEffect(() => {
    const next = query.data
    if (next)
      setRun((previous) => (previous && previous.watermark > next.watermark ? previous : next))
  }, [query.data])
  useEffect(() => {
    if (run)
      return observeMentor(session, run, (next, failure) => {
        setRun(next)
        setError(failure)
      })
  }, [run?.run_id, session])
  const command = async (
    kind: 'stop' | 'continue_budget' | 'retry_start' | 'respond',
    response?: string,
  ) => {
    if (!run || controller.current) return
    const active = new AbortController()
    controller.current = active
    setBusy(true)
    setError(undefined)
    const payload = {
      expected_version: run.version,
      kind,
      ...(['continue_budget', 'retry_start'].includes(kind)
        ? { request_budget: requests, token_budget: tokens }
        : {}),
      ...(kind === 'respond' ? { interaction_id: run.interaction?.id, answer: response } : {}),
    }
    const operation_id = drafts.operation(
      JSON.stringify([...prefix, spaceId, runId, 'task-command']),
      payload,
    )
    try {
      await unwrap(
        learningClient(session, spaceId).POST('/v1/learning/runs/{runID}/commands', {
          params: { path, header },
          body: { ...payload, operation_id },
          signal: active.signal,
        }),
        mentorReceiptSchema,
      )
      const next = await load(active.signal)
      if (mounted.current) setRun(next)
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) {
        setBusy(false)
        controller.current = null
      }
    }
  }
  const forbidden =
    query.error || (error instanceof ApiError && [401, 403, 404, 503].includes(error.status))
  const write = session.device.scopes.includes('learning:write')
  const canContinue =
    write &&
    space.data?.status === 'active' &&
    !!goal.data &&
    ['active', 'draft'].includes(goal.data.management.status) &&
    goal.data.revision === run?.goal_version &&
    !!run?.body_available
  return (
    <>
      <Link to="/runs" search={{ space: spaceId }}>
        ← 关闭详情并返回任务中心
      </Link>
      <h1>原运行详情</h1>
      {(query.error || error) && (
        <ErrorState error={query.error || error} retry={() => void query.refetch()} />
      )}
      {run && !forbidden && (
        <>
          <h2>
            {taskKinds[run.kind ?? 'mentor']} · {taskRunStatus[run.status]}
          </h2>
          <p>
            运行：{run.run_id} · 阶段：{run.stage}
          </p>
          <p>原因：{run.reason || '无'}</p>
          <Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId: run.goal_id }}>
            查看来源目标
          </Link>
          <p>
            请求已用 {run.requests_used} · 剩余 {run.requests_left} · 剩余 Token {run.tokens_left}
          </p>
          {run.result_unknown && (
            <p role="alert">外部结果未知。先核对原运行；新请求可能产生额外费用。</p>
          )}
          {run.cost_unknown && <p>提供商费用未知。</p>}
          {!run.body_available && <p>恢复正文已过期、已清除或临时副本不可用；原运行状态仍保留。</p>}
          <div className="actions task-actions">
            <Button variant="outline" disabled={busy} onClick={() => void query.refetch()}>
              核对原运行
            </Button>
            <Confirm
              label="停止原运行"
              title="停止此原运行？"
              disabled={
                busy || !write || isMentorTerminal(run.status) || run.status === 'cancelling'
              }
              onConfirm={() => void command('stop')}
            >
              已保存结果会保留。关闭页面本身不会停止任务。
            </Confirm>
          </div>
          {(run.status === 'paused_budget' ||
            (run.kind === 'start_learning' && ['partial', 'failed'].includes(run.status))) &&
            run.body_available && (
              <section className="panel">
                <h3>明确追加本次预算</h3>
                {!canContinue && <p>当前权限、学习区、目标版本或生命周期不允许继续。</p>}
                <label>
                  请求预算
                  <input
                    type="number"
                    min={1}
                    max={1000}
                    value={requests}
                    onChange={(e) => setRequests(Number(e.target.value))}
                  />
                </label>
                <label>
                  Token 预算
                  <input
                    type="number"
                    min={1}
                    max={10000000}
                    value={tokens}
                    onChange={(e) => setTokens(Number(e.target.value))}
                  />
                </label>
                <Button
                  disabled={busy || !canContinue || requests < 1 || tokens < 1}
                  onClick={() =>
                    void command(run.status === 'paused_budget' ? 'continue_budget' : 'retry_start')
                  }
                >
                  追加预算并继续原运行
                </Button>
              </section>
            )}
          {run.interaction && (
            <section className="panel">
              <h3>{run.interaction.question}</h3>
              {run.interaction.choices.map((choice) => (
                <Button
                  key={choice}
                  disabled={busy || !canContinue}
                  onClick={() => void command('respond', choice)}
                >
                  {choice}
                </Button>
              ))}
              {run.interaction.choices.length === 0 && (
                <>
                  <label>
                    回应
                    <input
                      maxLength={8000}
                      value={answer}
                      onChange={(e) => setAnswer(e.target.value)}
                    />
                  </label>
                  <Button
                    disabled={busy || !canContinue || !answer.trim()}
                    onClick={() => void command('respond', answer)}
                  >
                    提交回应
                  </Button>
                </>
              )}
            </section>
          )}
          <SafeMarkdown text={run.output} />
          {run.research && (
            <section>
              <h3>研究来源与结果</h3>
              <p>
                候选 {run.research.sources.length} · 已采纳{' '}
                {run.research.sources.filter((source) => source.status === 'adopted').length}
              </p>
              {run.research.sources.map((source) => (
                <details key={source.id}>
                  <summary>
                    {source.title} · {source.status}
                  </summary>
                  <p>{source.locator}</p>
                  <p>{source.failure}</p>
                  <pre>{source.text}</pre>
                </details>
              ))}
              {run.research.synthesis?.points.map((point, index) => (
                <p key={index}>{point.text}</p>
              ))}
            </section>
          )}
          {run.content_edit && <p>内容阶段：{run.content_edit.reason}</p>}
          {run.content_edit?.result && (
            <Link
              to="/content/$artifactId"
              params={{ artifactId: run.content_edit.result.artifact_id }}
              search={{ space: spaceId, version: run.content_edit.result.version }}
            >
              查看正式内容版本 {run.content_edit.result.version}
            </Link>
          )}
          {run.start_learning?.result && (
            <Link
              to="/spaces/$spaceId/learn/$sessionId"
              params={{ spaceId, sessionId: run.start_learning.result.session_id }}
            >
              进入已准备的课堂
            </Link>
          )}
        </>
      )}
    </>
  )
}
