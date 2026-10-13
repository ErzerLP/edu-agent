import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError } from '@/api/client'
import {
  checkStudioExercise,
  studioAPI,
  type StudioRevision,
  type studioQuizStateSchema,
} from '@/api/studio'
import type { z } from 'zod'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { PracticeController } from './practice-controller'

export function StudioPracticeCard({
  revision,
  record,
  disabled,
  onCitation,
}: {
  revision: StudioRevision
  record: z.infer<typeof studioQuizStateSchema>['questions'][number] & { exercise_id: string }
  disabled: boolean
  onCitation: (artifact: string, version: number, id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const spaceId = revision.learning_space_id,
    exerciseId = record.exercise_id
  const api = studioAPI(session, spaceId)
  const draftScope = ['studio-exercise', revision.artifact_id, revision.version]
  const query = useQuery({
    queryKey: [...prefix, spaceId, ...draftScope, exerciseId],
    gcTime: 0,
    retry: false,
    refetchInterval: 1500,
    queryFn: async ({ signal }) =>
      checkStudioExercise(await api.exercise(exerciseId, signal), {
        id: exerciseId,
        space: spaceId,
        generation: session.generation,
        artifact: revision.artifact_id,
        version: revision.version,
        question: record.question,
        session: record.session_id,
        activity: record.activity_id,
        content: record.content_artifact_id,
      }),
  })
  useEffect(() => {
    if (
      query.error instanceof ApiError &&
      ([401, 403, 404].includes(query.error.status) ||
        ['content_redacted', 'privacy_clear_in_progress'].includes(query.error.code))
    )
      drafts.deletePrefix(
        JSON.stringify([...prefix, spaceId, ...draftScope, exerciseId]).slice(0, -1),
      )
  }, [query.error, drafts, prefix, spaceId, revision.artifact_id, revision.version, exerciseId])
  if (query.error) return <ErrorState error={query.error} retry={() => void query.refetch()} />
  if (!query.data) return <p role="status">正在读取练习…</p>
  return (
    <PracticeController
      key={`${exerciseId}:${query.data.activity_id}:${query.data.content_version}`}
      exercise={query.data}
      disabled={disabled}
      draftScope={draftScope}
      transport={{
        help: (operation, reconcile) => api.help(exerciseId, operation, reconcile),
        resumeAssessment: (body) => api.resumeAssessment(exerciseId, body),
      }}
      refresh={async () => {
        const result = await query.refetch()
        if (result.error) throw result.error
      }}
      renderCitation={(reference, index) => {
        const citation = revision.body.citations.find(
          (c) => c.fragment.node_revision_id === reference,
        )
        return citation ? (
          <button
            type="button"
            className="citation-link"
            aria-label={`查看依据 ${index + 1}`}
            onClick={() => onCitation(revision.artifact_id, revision.version, citation.id)}
          >
            [{citation.id}]
          </button>
        ) : null
      }}
    />
  )
}
