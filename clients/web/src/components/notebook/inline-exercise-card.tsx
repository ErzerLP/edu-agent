import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { checkExercise, exerciseHelpSchema, exerciseSchema } from '@/api/exercises'
import { mentorReceiptSchema } from '@/api/mentor'
import type { SourceSelection } from '@/api/source-selection'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { PracticeController } from './practice-controller'

// Study owner adapter. Studio supplies its own transport to PracticeController.
export function InlineExerciseCard({
  spaceId,
  conversationId,
  exerciseId,
  selection,
  disabled = false,
  onSource,
}: {
  spaceId: string
  conversationId: string
  exerciseId: string
  selection?: SourceSelection
  disabled?: boolean
  onSource?: () => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQuery({
    queryKey: [...prefix, spaceId, 'exercise', exerciseId],
    gcTime: 0,
    retry: false,
    queryFn: async ({ signal }) =>
      checkExercise(
        await unwrap(
          learningClient(session, spaceId).GET('/v1/learning/exercises/{exerciseID}', {
            params: {
              header: { 'X-Learning-Space-ID': spaceId },
              path: { exerciseID: exerciseId },
            },
            signal,
          }),
          exerciseSchema,
        ),
        exerciseId,
        spaceId,
        session.generation,
        conversationId,
      ),
    refetchInterval: 1500,
  })
  useEffect(() => {
    if (
      query.error instanceof ApiError &&
      ([401, 403, 404].includes(query.error.status) || query.error.code === 'content_redacted')
    )
      drafts.deletePrefix(
        JSON.stringify([...prefix, spaceId, 'study-exercise', conversationId, exerciseId]).slice(
          0,
          -1,
        ),
      )
  }, [query.error, drafts, prefix, spaceId, conversationId, exerciseId])
  if (query.error) return <ErrorState error={query.error} retry={() => void query.refetch()} />
  if (!query.data) return <p role="status">正在读取练习…</p>
  return (
    <PracticeController
      key={`${query.data.id}:${query.data.activity_id}:${query.data.content_version}`}
      exercise={query.data}
      draftScope={['study-exercise', conversationId]}
      transport={{
        help: selection
          ? async (operation, reconcile, frozenSelection) => {
              if (!frozenSelection) throw new ApiError(400, 'source_selection_required')
              return unwrap(
                reconcile
                  ? learningClient(session, spaceId).GET(
                      '/v1/learning/exercises/{exerciseID}/help/{operationID}',
                      {
                        params: {
                          header: { 'X-Learning-Space-ID': spaceId },
                          path: { exerciseID: exerciseId, operationID: operation },
                        },
                      },
                    )
                  : learningClient(session, spaceId).POST(
                      '/v1/learning/exercises/{exerciseID}/help',
                      {
                        params: {
                          header: { 'X-Learning-Space-ID': spaceId },
                          path: { exerciseID: exerciseId },
                        },
                        body: { operation_id: operation, source_selection: frozenSelection },
                      },
                    ),
                exerciseHelpSchema,
              )
            }
          : undefined,
        resumeAssessment: (body) =>
          unwrap(
            learningClient(session, spaceId).POST(
              '/v1/learning/exercises/{exerciseID}/assessment/resume',
              {
                params: {
                  header: { 'X-Learning-Space-ID': spaceId },
                  path: { exerciseID: exerciseId },
                },
                body,
              },
            ),
            mentorReceiptSchema,
          ),
        change: (kind, operation) =>
          unwrap(
            learningClient(session, spaceId).POST('/v1/learning/exercises/{exerciseID}/commands', {
              params: {
                header: { 'X-Learning-Space-ID': spaceId },
                path: { exerciseID: exerciseId },
              },
              body: { operation_id: operation, kind },
            }),
            mentorReceiptSchema,
          ),
      }}
      selection={selection}
      disabled={disabled}
      onSource={onSource}
      refresh={async () => {
        const result = await query.refetch()
        if (result.error) throw result.error
      }}
    />
  )
}
