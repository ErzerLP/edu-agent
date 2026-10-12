import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { goalSchema, labels, type Space } from '@/api/runtime'
import { useIdentity } from '@/lib/session'
import { TeachingPage, SessionPicker } from '@/teaching-page'
import { GoalComposer } from '../goal-composer'
import { ErrorState } from '../common'
import { GoalSelector } from './goal-selector'

export function ClassroomPanel({
  space,
  goalId,
  sessionId,
  onGoalSelect,
}: {
  space: Space
  goalId?: string
  sessionId?: string
  onGoalSelect: (goalId: string) => void
}) {
  const { session, prefix } = useIdentity()
  const goal = useQuery({
    queryKey: [...prefix, space.id, goalId, 'latest'],
    enabled: !!goalId && session.capabilities.goals,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, space.id).GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: goalId! } },
          signal,
        }),
        goalSchema,
      ),
    gcTime: 0,
  })
  if (goal.error) return <ErrorState error={goal.error} retry={() => void goal.refetch()} />
  if (goal.data && (goal.data.learning_space_id !== space.id || goal.data.goal_id !== goalId))
    return <ErrorState error={new ApiError(404, 'wrong_space')} />
  const archived = space.status === 'archived'
  return (
    <>
      {archived && (
        <p className="notice">此学习区已归档，仅可查看历史。请从管理页恢复后继续写入。</p>
      )}
      {!session.capabilities.goals ? (
        <p role="status">当前服务尚不支持目标管理。</p>
      ) : (
        <>
          {goal.data && (
            <div className="notebook-current-goal">
              <h3>{goal.data.management.details.name}</h3>
              <span className="badge">{labels[goal.data.management.status]}</span>
              <Link
                to="/spaces/$spaceId/goals/$goalId"
                params={{ spaceId: space.id, goalId: goal.data.goal_id }}
              >
                目标详情与历史 →
              </Link>
            </div>
          )}
          <GoalSelector spaceId={space.id} goalId={goalId} onSelect={onGoalSelect} />
          {goalId && !goal.data && <p role="status">正在读取选定目标…</p>}
          {!sessionId && goal.data && (
            <>
              <SessionPicker key={goal.data.goal_id} goal={goal.data} archived={archived} />
              <details className="notebook-goal-compose">
                <summary>细化此目标与开始学习</summary>
                <GoalComposer
                  key={goal.data.goal_id}
                  spaceId={space.id}
                  goal={goal.data}
                  disabled={archived || !['draft', 'active'].includes(goal.data.management.status)}
                />
              </details>
            </>
          )}
          {!sessionId && !archived && (
            <details className="notebook-goal-compose" open={!goalId}>
              <summary>建立学习目标</summary>
              <GoalComposer key={space.id} spaceId={space.id} />
            </details>
          )}
        </>
      )}
      {sessionId && (
        <TeachingPage
          key={`${space.id}:${sessionId}`}
          spaceId={space.id}
          sessionId={sessionId}
          embedded
        />
      )}
    </>
  )
}
