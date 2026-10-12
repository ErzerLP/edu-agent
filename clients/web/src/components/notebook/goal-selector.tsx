import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { learningClient, unwrap } from '@/api/client'
import { goalSchema, labels, pageOf, type Goal } from '@/api/runtime'
import { useIdentity } from '@/lib/session'
import { usePage } from '@/pages'
import { ErrorState, Pagination } from '../common'
import { Button } from '../ui/button'

export function GoalSelector({
  spaceId,
  goalId,
  onSelect,
}: {
  spaceId: string
  goalId?: string
  onSelect: (goalId: string) => void
}) {
  const { session, prefix } = useIdentity()
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<Goal['management']['status'] | ''>('')
  const pages = usePage()
  const goals = useQuery({
    queryKey: [...prefix, spaceId, 'goals', search, status, pages.cursor],
    enabled: session.capabilities.goals,
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals', {
          params: { query: { search, status, limit: 10, cursor: pages.cursor } },
          signal,
        }),
        pageOf(goalSchema),
      ),
  })
  return (
    <details className="notebook-goal-selector" open={!goalId}>
      <summary>{goalId ? '选择其他目标' : '选择学习目标'}</summary>
      <div className="filters">
        <label>
          搜索目标
          <input
            type="search"
            value={search}
            onChange={(event) => {
              setSearch(event.target.value)
              pages.reset()
            }}
          />
        </label>
        <label>
          目标状态
          <select
            value={status}
            onChange={(event) => {
              setStatus(event.target.value as typeof status)
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
      {goals.isPending && <p role="status">正在读取目标…</p>}
      {goals.error && <ErrorState error={goals.error} retry={() => void goals.refetch()} />}
      {!goals.error && goals.data?.items.length === 0 && (
        <p>还没有匹配的目标。写下想学会的内容，就能先保存。</p>
      )}
      {!goals.error &&
        goals.data?.items.map((goal) => (
          <div className="notebook-goal-row" key={goal.goal_id}>
            <Button
              variant="ghost"
              onClick={() => onSelect(goal.goal_id)}
              aria-pressed={goalId === goal.goal_id}
            >
              {goal.management.details.name}
            </Button>
            <span className="hint">{labels[goal.management.status]}</span>
          </div>
        ))}
      {goals.data && (
        <Pagination
          page={pages.page}
          previous={pages.previous}
          next={goals.data.next_cursor ? () => pages.next(goals.data!.next_cursor!) : undefined}
        />
      )}
    </details>
  )
}
