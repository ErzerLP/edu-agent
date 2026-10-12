import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useIdentity } from '@/lib/session'
import { learningClient, unwrap } from '@/api/client'
import { checkConversation, conversationCreated, turnsSchema } from '@/api/conversations'
import { MentorPanel } from '../mentor-panel'
import { ErrorState } from '../common'
import { useSourceSelection } from './source-selection'

export function MentorComposer({
  spaceId,
  goalId,
  archived,
  onCitation,
}: {
  spaceId: string
  goalId?: string
  archived: boolean
  onCitation: (runId: string, id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const { selection } = useSourceSelection()
  const key = JSON.stringify([...prefix, spaceId, goalId, 'notebook-mentor'])
  const [id, setId] = useState(() => drafts.get<string>(key + ':conversation'))
  const header = { 'X-Learning-Space-ID': spaceId }
  const read = async (conversationId: string, signal?: AbortSignal) => {
    const page = await unwrap(
      learningClient(session, spaceId).GET('/v1/learning/conversations/{conversationID}', {
        params: { path: { conversationID: conversationId }, header },
        signal,
      }),
      turnsSchema,
    )
    checkConversation(page.conversation, spaceId, session.generation, conversationId)
    return page.conversation
  }
  const detail = useQuery({
    queryKey: [...prefix, spaceId, goalId, 'notebook-conversation', id],
    gcTime: 0,
    enabled: !!id,
    refetchInterval: 5000,
    queryFn: ({ signal }) => read(id!, signal),
  })
  if (!session.capabilities.runs || !session.capabilities.save_goal)
    return <p role="status">来源问答需要导师运行与学习写入权限。</p>
  if (!session.device.scopes.includes('knowledge:read'))
    return <p role="status">来源问答需要来源读取权限，请从设置与能力查看配对要求。</p>
  if (detail.error) return <ErrorState error={detail.error} retry={() => void detail.refetch()} />
  if (id && !detail.data) return <p role="status">正在核对来源问答会话…</p>
  const ensureConversation = async (saved: boolean) => {
    if (id) return read(id)
    const payload = { goal_id: goalId, saved }
    const result = await unwrap(
      learningClient(session, spaceId).POST('/v1/learning/conversations', {
        params: { header },
        body: { ...payload, id: drafts.operation(key + ':new', payload) },
      }),
      conversationCreated,
    )
    drafts.set(key + ':conversation', result.id)
    // Read before mounting the existing conversation so version/destination come from the server.
    const conversation = await read(result.id)
    query.setQueryData(
      [...prefix, spaceId, goalId, 'notebook-conversation', result.id],
      conversation,
    )
    setId(result.id)
    return conversation
  }
  return (
    <>
      <MentorPanel
        goal={{
          learning_space_id: spaceId,
          goal_id: goalId ?? '00000000-0000-0000-0000-000000000000',
          revision: detail.data?.goal_version ?? 1,
          management: { status: detail.data && !detail.data.writable ? 'paused' : 'active' },
        }}
        archivedSpace={archived}
        conversation={detail.data}
        onChange={() =>
          void query.invalidateQueries({
            queryKey: [...prefix, spaceId, goalId, 'notebook-conversation'],
          })
        }
        notebook={{ selection, draftKey: key, ensureConversation, onCitation }}
      />
      {id && (
        <Link to="/spaces/$spaceId/chat/$conversationId" params={{ spaceId, conversationId: id }}>
          查看此问答的历史 →
        </Link>
      )}
    </>
  )
}
