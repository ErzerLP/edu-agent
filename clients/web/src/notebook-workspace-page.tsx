import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './api/client'
import { spaceSchema } from './api/runtime'
import { teachingSchema } from './api/teaching'
import { useIdentity } from './lib/session'
import { ErrorState } from './components/common'
import { NotebookHeader } from './components/notebook/notebook-header'
import { NotebookLayout, type NotebookPanel } from './components/notebook/notebook-layout'
import { SourcesPanel } from './components/notebook/sources-panel'
import { ClassroomPanel } from './components/notebook/classroom-panel'
import { StudioPanel } from './components/notebook/studio-panel'

export const notebookSearchSchema = z.object({
  goal: z.uuid().optional(),
  panel: z.enum(['classroom', 'sources', 'studio']).optional(),
})
export type NotebookSearch = z.infer<typeof notebookSearchSchema>

export function NotebookWorkspacePage({
  spaceId,
  sessionId,
  search,
}: {
  spaceId: string
  sessionId?: string
  search: NotebookSearch
}) {
  if (!z.uuid().safeParse(spaceId).success || (sessionId && !z.uuid().safeParse(sessionId).success))
    return <ErrorState error={new ApiError(404, 'invalid_request')} />
  return (
    <NotebookWorkspace
      key={`${spaceId}:${sessionId ?? ''}`}
      spaceId={spaceId}
      sessionId={sessionId}
      search={search}
    />
  )
}

function NotebookWorkspace({
  spaceId,
  sessionId,
  search,
}: {
  spaceId: string
  sessionId?: string
  search: NotebookSearch
}) {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const space = useQuery({
    queryKey: [...prefix, spaceId, 'space'],
    gcTime: 0,
    enabled: session.capabilities.spaces,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces/{spaceID}', {
          params: { path: { spaceID: spaceId } },
          signal,
        }),
        spaceSchema,
      ),
  })
  const teaching = useQuery({
    queryKey: [...prefix, spaceId, sessionId, 'teaching'],
    gcTime: 0,
    enabled: !!sessionId && !!space.data,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/tutoring/sessions/{sessionID}', {
          params: { path: { sessionID: sessionId! } },
          signal,
        }),
        teachingSchema,
      ),
  })
  const sessionGoal = teaching.data?.work_item?.goal_revision
  if (!session.capabilities.spaces) return <p role="status">当前服务尚不支持学习区。</p>
  if (space.error || teaching.error)
    return (
      <ErrorState
        error={space.error || teaching.error}
        retry={() => {
          void space.refetch()
          if (sessionId) void teaching.refetch()
        }}
      />
    )
  if (!space.data || (sessionId && !teaching.data))
    return <p role="status">正在读取学习区与课堂…</p>
  if (
    space.data.id !== spaceId ||
    (sessionId &&
      (teaching.data?.session.session_id !== sessionId ||
        (sessionGoal?.learning_space_id && sessionGoal.learning_space_id !== spaceId)))
  )
    return <ErrorState error={new ApiError(404, 'wrong_space')} />
  if (sessionId && search.goal && sessionGoal?.goal_id !== search.goal)
    return <p role="alert">地址中的目标与课堂不一致。请移除目标参数或返回此课堂的原目标。</p>
  const goalId = sessionId ? sessionGoal?.goal_id : search.goal
  const changePanel = (panel: NotebookPanel) => {
    if (sessionId)
      void navigate({
        to: '/spaces/$spaceId/learn/$sessionId',
        params: { spaceId, sessionId },
        search: { ...search, panel },
        replace: true,
      })
    else
      void navigate({
        to: '/spaces/$spaceId',
        params: { spaceId },
        search: { ...search, panel },
        replace: true,
      })
  }
  return (
    <div className="notebook-workspace">
      <NotebookHeader space={space.data} />
      <NotebookLayout
        panel={search.panel ?? 'classroom'}
        onPanelChange={changePanel}
        sources={<SourcesPanel spaceId={spaceId} archived={space.data.status === 'archived'} />}
        classroom={
          <ClassroomPanel
            space={space.data}
            goalId={goalId}
            sessionId={sessionId}
            onGoalSelect={(goal) =>
              void navigate({
                to: '/spaces/$spaceId',
                params: { spaceId },
                search: { goal, panel: 'classroom' },
              })
            }
          />
        }
        studio={<StudioPanel spaceId={spaceId} />}
      />
    </div>
  )
}
