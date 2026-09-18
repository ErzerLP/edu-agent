import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { useIdentity } from './lib/session'
import { ApiError, learningClient, unwrap } from './api/client'
import {
  goalResult,
  goalSchema,
  labels,
  pageOf,
  spaceDraftSchema,
  spaceSchema,
  type Goal,
  type Space,
} from './api/runtime'
import type { components } from './api/schema'
import { GoalComposer } from './components/goal-composer'
import { Button } from './components/ui/button'
import { TutorHistory } from './components/tutor-history'
import { ChangePanel } from './components/change-panel'
import { SessionPicker } from './teaching-page'
import { Confirm, EmptyState, ErrorState, Pagination } from './components/common'
import { HomeProgress, ProgressPanel } from './progress-page'
import { progressSearch } from './api/progress'

const defaultSpace = '00000000-0000-4000-8000-000000000001'

function usePage() {
  const [cursors, setCursors] = useState([''])
  return {
    cursor: cursors.at(-1)!,
    page: cursors.length,
    next: (c: string) => setCursors([...cursors, c]),
    previous: cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined,
    reset: () => setCursors(['']),
  }
}

function SpaceEditor({ space }: { space?: Space }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const key = JSON.stringify([...prefix, space?.id ?? 'new-space', 'edit'])
  const form = useForm({
    resolver: zodResolver(spaceDraftSchema),
    defaultValues: drafts.get<{ name: string; description: string }>(key) ?? {
      name: space?.name ?? '',
      description: space?.description ?? '',
    },
  })
  const [base, setBase] = useState(space)
  const [error, setError] = useState<unknown>()
  const [message, setMessage] = useState('')
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  useEffect(() => {
    const sub = form.watch(() => drafts.set(key, form.getValues()))
    return () => sub.unsubscribe()
  }, [key])
  const mutate = async (
    values: { name: string; description: string },
    status = base?.status ?? 'active',
  ) => {
    if (pending) return
    setPending(true)
    setError(undefined)
    setMessage('')
    const payload = {
      ...values,
      status: status as 'active' | 'archived',
      expected_version: base?.version ?? 0,
    }
    const body = { ...payload, operation_id: drafts.operation(key, payload) }
    try {
      const client = learningClient(session)
      const result = await unwrap(
        base
          ? client.PUT('/v1/learning-spaces/{spaceID}', {
              params: { path: { spaceID: base.id } },
              body,
            })
          : client.POST('/v1/learning-spaces', { body }),
        spaceSchema,
      )
      if (JSON.stringify(drafts.get(key)) === JSON.stringify(values)) drafts.delete(key)
      if (mounted.current) {
        setBase(space ? result : undefined)
        form.reset(space ? values : { name: '', description: '' })
        drafts.delete(key)
        setMessage(space ? '学习区已更新。' : '学习区已创建，可从列表进入。')
      }
      void query.invalidateQueries({ queryKey: prefix })
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setPending(false)
    }
  }
  return (
    <details className="panel compact">
      <summary>{space ? '编辑学习区' : '新建学习区'}</summary>
      <form
        onSubmit={form.handleSubmit((v) => mutate(v))}
        onKeyDown={(e) => {
          if (
            e.key === 'Enter' &&
            (e.nativeEvent.isComposing || e.target instanceof HTMLInputElement)
          )
            e.preventDefault()
        }}
      >
        <fieldset disabled={pending || !session.capabilities.save_goal}>
          <label>
            学习区名称
            <input {...form.register('name')} maxLength={120} />
          </label>
          <label>
            学习区说明
            <textarea rows={2} {...form.register('description')} maxLength={2000} />
          </label>
          {form.formState.errors.name && <p role="alert">{form.formState.errors.name.message}</p>}
          <div className="actions">
            <Button variant="outline" type="submit">
              {space ? '保存学习区' : '创建学习区'}
            </Button>
            {base && (
              <Confirm
                label={base.status === 'archived' ? '恢复学习区' : '归档学习区'}
                title="确认变更学习区状态？"
                disabled={pending}
                onConfirm={() =>
                  void mutate(form.getValues(), base.status === 'archived' ? 'active' : 'archived')
                }
              >
                归档会停止此区的新写入，已有目标和历史仍会保留。
              </Confirm>
            )}
          </div>
        </fieldset>
        {!!error && <ErrorState error={error} />}
        {error instanceof ApiError && error.status === 409 && base && (
          <Button
            variant="outline"
            onClick={async () => {
              try {
                setBase(
                  await unwrap(
                    learningClient(session).GET('/v1/learning-spaces/{spaceID}', {
                      params: { path: { spaceID: base.id } },
                    }),
                    spaceSchema,
                  ),
                )
                setError(undefined)
                setMessage('已读取最新版本，请核对输入后保存。')
              } catch (e) {
                setError(e)
              }
            }}
          >
            读取最新版本并保留输入
          </Button>
        )}
        {message && <p role="status">{message}</p>}
      </form>
    </details>
  )
}

export function HomePage() {
  const { session, prefix } = useIdentity()
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<'active' | 'archived' | 'all'>('active')
  const pages = usePage()
  const spaces = useQuery({
    queryKey: [...prefix, 'spaces', search, status, pages.cursor],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces', {
          params: {
            query: {
              search,
              status: status === 'all' ? undefined : status,
              cursor: pages.cursor,
              limit: 10,
            },
          },
          signal,
        }),
        pageOf(spaceSchema),
      ),
    enabled: session.capabilities.spaces,
  })
  return (
    <>
      <HomeProgress />
      <section className="intro">
        <span className="eyebrow">你的学习，从一个目标开始</span>
        <h1>今天想学会什么？</h1>
        <p>写下想做到的事。慢慢细化，也可以先留住这个念头。</p>
      </section>
      {session.capabilities.goals ? (
        <>
          <p className="hint">首页新目标保存到默认学习区；也可以进入其他学习区再创建。</p>
          <GoalComposer spaceId={defaultSpace} />
        </>
      ) : (
        <EmptyState>当前服务尚不支持目标管理。</EmptyState>
      )}
      <section className="section">
        <div className="section-heading">
          <div>
            <h2>你的学习区</h2>
            <p className="hint">按兴趣组织目标，在不同方向之间自由切换。</p>
          </div>
          <Link to="/spaces/$spaceId" params={{ spaceId: defaultSpace }}>
            进入默认学习区 →
          </Link>
        </div>
        <div className="filters">
          <label>
            搜索学习区
            <input
              type="search"
              value={search}
              onChange={(e) => {
                setSearch(e.target.value)
                pages.reset()
              }}
            />
          </label>
          <label>
            学习区状态
            <select
              value={status}
              onChange={(e) => {
                setStatus(e.target.value as typeof status)
                pages.reset()
              }}
            >
              <option value="active">进行中</option>
              <option value="archived">已归档</option>
              <option value="all">全部</option>
            </select>
          </label>
        </div>
        {spaces.isPending && session.capabilities.spaces && <p role="status">正在加载学习区…</p>}
        {spaces.error && <ErrorState error={spaces.error} retry={() => void spaces.refetch()} />}
        {!session.capabilities.spaces && <EmptyState>当前服务尚不支持学习区。</EmptyState>}
        {spaces.data?.items.length === 0 && (
          <EmptyState>没有匹配的学习区，试试其他搜索条件或新建一个。</EmptyState>
        )}
        <div className="space-grid">
          {spaces.data?.items.map((space) => (
            <article className="panel space-card" key={space.id}>
              <span className="badge">{space.status === 'archived' ? '已归档' : '学习区'}</span>
              <h3>
                <Link to="/spaces/$spaceId" params={{ spaceId: space.id }}>
                  {space.name}
                </Link>
              </h3>
              <p>{space.description || '为一个想探索的方向，留一片空间。'}</p>
            </article>
          ))}
        </div>
        {spaces.data && (
          <Pagination
            page={pages.page}
            previous={pages.previous}
            next={spaces.data.next_cursor ? () => pages.next(spaces.data!.next_cursor!) : undefined}
          />
        )}
        {session.capabilities.spaces && <SpaceEditor />}
      </section>
    </>
  )
}

export function SpacePage({ spaceId }: { spaceId: string }) {
  const { session, prefix } = useIdentity()
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<Goal['management']['status'] | ''>('')
  const pages = usePage()
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
  const goals = useQuery({
    queryKey: [...prefix, spaceId, 'goals', search, status, pages.cursor],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals', {
          params: { query: { search, status, limit: 10, cursor: pages.cursor } },
          signal,
        }),
        pageOf(goalSchema),
      ),
    enabled: !!space.data && session.capabilities.goals,
  })
  if (space.error) return <ErrorState error={space.error} retry={() => void space.refetch()} />
  if (!space.data) return <p role="status">正在读取学习区…</p>
  return (
    <>
      <Link to="/">← 全部学习区</Link>
      <section className="intro">
        <span className="eyebrow">学习区</span>
        <h1>{space.data.name}</h1>
        <p>{space.data.description || '从一个具体的目标开始，逐步学会你想做的事。'}</p>
      </section>
      <SpaceEditor key={spaceId} space={space.data} />
      {!session.capabilities.goals ? (
        <EmptyState>当前服务尚不支持目标管理。</EmptyState>
      ) : space.data.status === 'archived' ? (
        <p className="notice">此学习区已归档，恢复后可继续保存目标。</p>
      ) : (
        <GoalComposer spaceId={spaceId} />
      )}
      <section className="section">
        <h2>学习目标</h2>
        <div className="filters">
          <label>
            搜索目标
            <input
              type="search"
              value={search}
              onChange={(e) => {
                setSearch(e.target.value)
                pages.reset()
              }}
            />
          </label>
          <label>
            目标状态
            <select
              value={status}
              onChange={(e) => {
                setStatus(e.target.value as typeof status)
                pages.reset()
              }}
            >
              <option value="">全部</option>
              {Object.entries(labels).map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
          </label>
        </div>
        {goals.error && <ErrorState error={goals.error} retry={() => void goals.refetch()} />}
        {goals.isPending && session.capabilities.goals && <p role="status">正在读取目标…</p>}
        {goals.data?.items.length === 0 && (
          <EmptyState>
            <h3>这里还没有匹配的目标</h3>
            <p>在上方写下想学会的内容，无需模型或资料就能保存。</p>
          </EmptyState>
        )}
        <div className="goal-list">
          {goals.data?.items.map((goal) => (
            <article key={goal.goal_id} className="panel goal-row">
              <div>
                <span className="badge">{labels[goal.management.status]}</span>
                <h3>
                  <Link
                    to="/spaces/$spaceId/goals/$goalId"
                    params={{ spaceId, goalId: goal.goal_id }}
                  >
                    {goal.management.details.name}
                  </Link>
                </h3>
                <p>{goal.text}</p>
              </div>
              <span className="hint">修订 {goal.revision}</span>
            </article>
          ))}
        </div>
        {goals.data && (
          <Pagination
            page={pages.page}
            previous={pages.previous}
            next={goals.data.next_cursor ? () => pages.next(goals.data!.next_cursor!) : undefined}
          />
        )}
      </section>
    </>
  )
}

function GoalLifecycle({ goal, archivedSpace }: { goal: Goal; archivedSpace: boolean }) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const reasonKey = JSON.stringify([
    ...prefix,
    goal.learning_space_id,
    goal.goal_id,
    'completion-reason',
  ])
  const [reason, setReason] = useState(drafts.get<string>(reasonKey) ?? '')
  const [error, setError] = useState<unknown>()
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const run = async (action: string) => {
    setPending(true)
    setError(undefined)
    const payload = {
      payload_schema_version: 1 as const,
      aggregate_type: 'goal' as const,
      aggregate_id: goal.goal_id,
      expected_version: goal.revision,
      text: goal.text,
      source: 'web',
      previous_revision_id: goal.goal_revision_id,
      action: action as components['schemas']['LearningGoalRequest']['action'],
      ...(action === 'complete' ? { completion_reason: reason.trim() } : {}),
    }
    const body = {
      ...payload,
      operation_id: drafts.operation(
        JSON.stringify([...prefix, goal.learning_space_id, goal.goal_id, 'action']),
        payload,
      ),
    }
    try {
      await unwrap(
        learningClient(session, goal.learning_space_id).PUT('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: goal.goal_id } },
          body,
        }),
        goalResult,
      )
      void query.invalidateQueries({ queryKey: [...prefix, goal.learning_space_id] })
      if (action === 'complete') {
        drafts.delete(reasonKey)
        if (mounted.current) setReason('')
      }
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setPending(false)
    }
  }
  const status = goal.management.status
  return (
    <section className="panel">
      <h2>目标状态</h2>
      <p>当前：{labels[status]}。手动完成记录你的依据，不代表已经验证掌握。</p>
      <div className="actions">
        {(
          [
            ['start', 'draft', '标为进行中'],
            ['pause', 'active', '暂停'],
            ['resume', 'paused', '恢复'],
          ] as const
        )
          .filter(([, from]) => from === status)
          .map(([action, , label]) => (
            <Button
              variant="outline"
              key={action}
              disabled={pending || archivedSpace || !session.capabilities.save_goal}
              onClick={() => void run(action)}
            >
              {label}
            </Button>
          ))}
        <Confirm
          label={status === 'archived' ? '恢复目标' : '归档目标'}
          title="确认变更目标状态？"
          disabled={pending || archivedSpace || !session.capabilities.save_goal}
          onConfirm={() => void run(status === 'archived' ? 'restore' : 'archive')}
        >
          历史修订和已保存的学习事实会保留。
        </Confirm>
      </div>
      {!['completed', 'archived'].includes(status) && (
        <div className="completion">
          <label>
            手动完成依据
            <textarea
              rows={2}
              value={reason}
              disabled={pending || archivedSpace}
              onChange={(e) => {
                setReason(e.target.value)
                if (e.target.value) drafts.set(reasonKey, e.target.value)
                else drafts.delete(reasonKey)
              }}
              maxLength={4000}
            />
          </label>
          <Confirm
            label="手动完成"
            title="将目标标为手动完成？"
            disabled={pending || archivedSpace || !reason.trim() || !session.capabilities.save_goal}
            onConfirm={() => void run('complete')}
          >
            依据：{reason || '请先填写依据。'} 此操作不产生掌握度。
          </Confirm>
        </div>
      )}
      {goal.management.completion && <p>完成依据：{goal.management.completion.reason}</p>}
      {!!error && (
        <ErrorState
          error={error}
          retry={() =>
            void query.invalidateQueries({ queryKey: [...prefix, goal.learning_space_id] })
          }
        />
      )}
    </section>
  )
}

export function GoalPage({ spaceId, goalId }: { spaceId: string; goalId: string }) {
  const { session, prefix } = useIdentity()
  const pages = usePage()
  const goal = useQuery({
    queryKey: [...prefix, spaceId, goalId, 'latest'],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: goalId } },
          signal,
        }),
        goalSchema,
      ),
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
  const history = useQuery({
    queryKey: [...prefix, spaceId, goalId, goal.data?.revision, 'history', pages.cursor],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}/revisions', {
          params: { path: { goalID: goalId }, query: { limit: 10, cursor: pages.cursor } },
          signal,
        }),
        pageOf(goalSchema),
      ),
    enabled: !!goal.data,
  })
  if (goal.error || space.error)
    return (
      <ErrorState
        error={goal.error || space.error}
        retry={() => {
          void goal.refetch()
          void space.refetch()
        }}
      />
    )
  if (!goal.data || !space.data) return <p role="status">正在读取目标…</p>
  if (goal.data.learning_space_id !== spaceId || goal.data.goal_id !== goalId)
    return <ErrorState error={new ApiError(404, 'wrong_space')} />
  return (
    <>
      <Link to="/spaces/$spaceId" params={{ spaceId }}>
        ← 返回{space.data.name}
      </Link>
      <section className="intro">
        <span className="eyebrow">目标 · 修订 {goal.data.revision}</span>
        <h1>{goal.data.management.details.name}</h1>
      </section>
      <GoalComposer
        spaceId={spaceId}
        goal={goal.data}
        disabled={space.data.status === 'archived' || goal.data.management.status === 'archived'}
      />
      <GoalLifecycle goal={goal.data} archivedSpace={space.data.status === 'archived'} />
      <SessionPicker goal={goal.data} archived={space.data.status === 'archived'} />
      <section className="section">
        <h2>此目标的活动、证据与复习</h2>
        <ProgressPanel key={`${spaceId}:${goalId}`} search={progressSearch.parse({ space: spaceId, goal: goalId, status: 'all' })} />
        <Link to="/progress" search={progressSearch.parse({ space: spaceId, goal: goalId, status: 'all' })}>筛选此目标进度 →</Link>
      </section>
      <ChangePanel key={`${spaceId}:${goalId}`} goal={goal.data} archived={space.data.status === 'archived'} />
      <p><Link to="/spaces/$spaceId/goals/$goalId/research" params={{ spaceId, goalId }}>研究相关知识与查看来源 →</Link></p>
      <p><Link to="/spaces/$spaceId/goals/$goalId/research" params={{ spaceId, goalId }} search={{ start: true }}>开学过程与失败恢复 →</Link></p>
      <TutorHistory key={`${spaceId}:${goalId}`} spaceId={spaceId} goalId={goalId} />
      <section className="section">
        <h2>修订历史</h2>
        {history.error && <ErrorState error={history.error} retry={() => void history.refetch()} />}
        {history.data?.items.map((item) => (
          <details className="panel compact" key={item.goal_revision_id}>
            <summary>
              修订 {item.revision} · {labels[item.management.status]} ·{' '}
              {new Date(item.created_at).toLocaleString('zh-CN')}
            </summary>
            <p>{item.text}</p>
            <dl>
              {Object.entries(item.management.details).map(([key, value]) => (
                <div key={key}>
                  <dt>
                    {
                      (
                        {
                          name: '名称',
                          expected_outcome: '期望结果',
                          scope: '范围',
                          exclusions: '暂不学习',
                          self_assessment: '当前基础',
                          purpose: '用途',
                          completion_criteria: '完成标准',
                          priority: '优先级',
                          timezone: '时区',
                          deadline: '截止时间',
                          weekly_minutes: '每周分钟',
                          scope_snapshot_id: '资料范围',
                        } as Record<string, string>
                      )[key]
                    }
                  </dt>
                  <dd>{String(value || '未填写')}</dd>
                </div>
              ))}
            </dl>
            {item.management.completion && <p>手动完成依据：{item.management.completion.reason}</p>}
          </details>
        ))}
        {history.data && (
          <Pagination
            page={pages.page}
            previous={pages.previous}
            next={
              history.data.next_cursor ? () => pages.next(history.data!.next_cursor!) : undefined
            }
          />
        )}
      </section>
    </>
  )
}

export { SettingsPage } from './settings-page'
