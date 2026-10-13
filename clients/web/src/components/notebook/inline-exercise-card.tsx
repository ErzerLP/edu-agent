import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import {
  availableHelp,
  checkExercise,
  exerciseHelpSchema,
  exerciseSchema,
  type Exercise,
  type ExerciseHelp,
} from '@/api/exercises'
import { mentorReceiptSchema } from '@/api/mentor'
import { contentHeader, operationReceipt, operationResult } from '@/api/teaching'
import type { SourceSelection } from '@/api/source-selection'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { FeedbackCard } from './feedback-card'
import { PracticeCard } from './practice-card'

type AnswerBody = {
  operation_id: string
  payload_schema_version: 1
  aggregate_type: 'session'
  aggregate_id: string
  expected_version: number
  action: 'submit_attempt'
  answer: string
  help: ExerciseHelp
  content_version: number
}
type Pending =
  | { kind: 'answer'; body: AnswerBody }
  | { kind: 'continue'; operation: string; version: number }
  | { kind: 'help'; operation: string; selection: SourceSelection }
  | { kind: 'defer' | 'resume'; operation: string }
  | {
      kind: 'assessment'
      operation: string
      version: number
      configuration: string
      requests: number
      tokens: number
    }

// Shared card controller: C4 can supply a published exercise ID and exact
// conversation binding; all learning actions keep their original receipts.
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
    <ExerciseCard
      key={`${query.data.id}:${query.data.activity_id}:${query.data.content_version}`}
      exercise={query.data}
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

export function ExerciseCard({
  exercise,
  selection,
  disabled,
  refresh,
  onSource,
}: {
  exercise: Exercise
  selection?: SourceSelection
  disabled: boolean
  refresh: () => Promise<void>
  onSource?: () => void
}) {
  const { session, prefix, drafts } = useIdentity(),
    query = useQueryClient()
  const key = JSON.stringify([
    ...prefix,
    exercise.space_id,
    'study-exercise',
    exercise.conversation_id,
    exercise.id,
    exercise.activity_id,
    exercise.content_version,
    'exercise-answer',
  ])
  const [answer, setAnswer] = useState(() => drafts.get<string>(key) ?? ''),
    [help, setHelp] = useState<ExerciseHelp | ''>(
      () => drafts.get<ExerciseHelp>(key + ':help') ?? '',
    )
  const [pending, setPending] = useState<Pending | undefined>(() =>
    drafts.get<Pending>(key + ':pending'),
  )
  const [hint, setHint] = useState(() => drafts.get<string>(key + ':hint') ?? ''),
    [error, setError] = useState<unknown>(),
    [busy, setBusy] = useState(false)
  const [requests, setRequests] = useState(2),
    [tokens, setTokens] = useState(30000)
  const live = useRef(true),
    busyRef = useRef(false)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  useEffect(() => {
    if (exercise.attempt_id) {
      drafts.delete(key)
      drafts.delete(key + ':help')
      if (
        pending?.kind === 'answer' &&
        exercise.feedback?.receipt.operation_id === pending.body.operation_id
      ) {
        drafts.delete(key + ':pending')
        setPending(undefined)
        setError(undefined)
      }
    }
  }, [exercise.attempt_id, exercise.feedback?.receipt.operation_id, drafts, key, pending])
  const writable = !disabled && session.device.scopes.includes('learning:write')
  const client = () => learningClient(session, exercise.space_id),
    header = { 'X-Learning-Space-ID': exercise.space_id },
    path = { exerciseID: exercise.id }
  const options = availableHelp(exercise.allowed_help, exercise.help_floor)
  const selectedHelp = help && options.includes(help) ? help : ''
  const remember = (p: Pending) => {
    drafts.set(key + ':pending', p)
    setPending(p)
  }
  const done = () => {
    drafts.delete(key + ':pending')
    if (live.current) setPending(undefined)
    void query.invalidateQueries({ queryKey: [...prefix, exercise.space_id] })
  }
  const perform = async (p: Pending, reconcile = false) => {
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setError(undefined)
    remember(p)
    try {
      if (p.kind === 'answer') {
        if (reconcile) {
          const result = await unwrap(
            client().GET('/v1/tutoring/sessions/{sessionID}/operations/{operationID}', {
              params: {
                path: { sessionID: exercise.session_id, operationID: p.body.operation_id },
                header: contentHeader(exercise.space_id),
              },
            }),
            operationReceipt,
          )
          if (
            result.session_id !== exercise.session_id ||
            result.operation_id !== p.body.operation_id
          )
            throw new ApiError(502, 'invalid_response')
          if (result.status === 'rejected') {
            done()
            throw new ApiError(409, result.code ?? 'answer_rejected')
          }
        } else {
          const result = await unwrap(
            client().POST('/v1/learning/content/{artifactID}/answers', {
              params: {
                header: contentHeader(exercise.space_id),
                path: { artifactID: exercise.artifact_id },
              },
              body: p.body,
            }),
            operationResult,
          )
          if (result.aggregate_id !== exercise.session_id)
            throw new ApiError(502, 'invalid_response')
        }
        drafts.delete(key)
        drafts.delete(key + ':help')
      } else if (p.kind === 'continue') {
        if (reconcile) {
          const result = await unwrap(
            client().GET('/v1/tutoring/sessions/{sessionID}/operations/{operationID}', {
              params: {
                path: { sessionID: exercise.session_id, operationID: p.operation },
                header: contentHeader(exercise.space_id),
              },
            }),
            operationReceipt,
          )
          if (result.session_id !== exercise.session_id || result.operation_id !== p.operation)
            throw new ApiError(502, 'invalid_response')
          if (result.status === 'rejected') {
            done()
            throw new ApiError(409, result.code ?? 'action_rejected')
          }
        } else {
          const result = await unwrap(
            client().POST('/v1/tutoring/sessions/{sessionID}/actions', {
              params: { path: { sessionID: exercise.session_id } },
              body: {
                operation_id: p.operation,
                payload_schema_version: 1,
                aggregate_type: 'session',
                aggregate_id: exercise.session_id,
                expected_version: p.version,
                action: 'acknowledge_feedback',
              },
            }),
            operationResult,
          )
          if (result.aggregate_id !== exercise.session_id)
            throw new ApiError(502, 'invalid_response')
        }
      } else if (p.kind === 'help') {
        const result = await unwrap(
          reconcile
            ? client().GET('/v1/learning/exercises/{exerciseID}/help/{operationID}', {
                params: { header, path: { ...path, operationID: p.operation } },
              })
            : client().POST('/v1/learning/exercises/{exerciseID}/help', {
                params: { header, path },
                body: { operation_id: p.operation, source_selection: p.selection },
              }),
          exerciseHelpSchema,
        )
        if (result.operation_id !== p.operation) throw new ApiError(502, 'invalid_response')
        drafts.set(key + ':hint', result.text)
        if (live.current) {
          setHint(result.text)
          setHelp(result.level)
          drafts.set(key + ':help', result.level)
        }
      } else {
        const result = await unwrap(
          reconcile
            ? client().GET('/v1/learning/operations/{operationID}', {
                params: { header, path: { operationID: p.operation } },
              })
            : p.kind === 'assessment'
              ? client().POST('/v1/learning/exercises/{exerciseID}/assessment/resume', {
                  params: { header, path },
                  body: {
                    operation_id: p.operation,
                    expected_version: p.version,
                    configuration: p.configuration,
                    request_budget: p.requests,
                    token_budget: p.tokens,
                  },
                })
              : client().POST('/v1/learning/exercises/{exerciseID}/commands', {
                  params: { header, path },
                  body: { operation_id: p.operation, kind: p.kind },
                }),
          mentorReceiptSchema,
        )
        if (result.operation_id !== p.operation) throw new ApiError(502, 'invalid_response')
      }
      done()
      await refresh()
    } catch (e) {
      if (e instanceof ApiError && e.status < 500 && !reconcile) done()
      if (live.current) setError(e)
    } finally {
      busyRef.current = false
      if (live.current) setBusy(false)
    }
  }
  const submit = () => {
    if (!answer.trim() || !selectedHelp || !writable || pending || busy) return
    void perform({
      kind: 'answer',
      body: {
        operation_id: crypto.randomUUID(),
        payload_schema_version: 1,
        aggregate_type: 'session',
        aggregate_id: exercise.session_id,
        expected_version: exercise.session_version,
        action: 'submit_attempt',
        answer,
        help: selectedHelp,
        content_version: exercise.content_version,
      },
    })
  }
  const state = exercise.assessment
  return (
    <>
      <PracticeCard
        prompt={exercise.prompt}
        answer={exercise.feedback?.attempt.answer ?? answer}
        help={exercise.feedback?.attempt.help ?? selectedHelp}
        helpOptions={options}
        submitted={!!exercise.attempt_id}
        deferred={exercise.deferred}
        disabled={!writable || busy || !!pending}
        hint={hint}
        onAnswer={(value) => {
          setAnswer(value)
          drafts.set(key, value)
        }}
        onHelpChange={(value) => {
          setHelp(value)
          drafts.set(key + ':help', value)
        }}
        onSubmit={submit}
        onHint={
          selection && exercise.allowed_help.includes('hint')
            ? () => void perform({ kind: 'help', operation: crypto.randomUUID(), selection })
            : undefined
        }
        onDefer={() => void perform({ kind: 'defer', operation: crypto.randomUUID() })}
        onResume={() => void perform({ kind: 'resume', operation: crypto.randomUUID() })}
      >
        {!!error && <ErrorState error={error} retry={() => void refresh()} />}
        {error instanceof ApiError &&
          error.code === 'exercise_source_selection_mismatch' &&
          onSource && (
            <Button variant="ghost" onClick={onSource}>
              选择来源
            </Button>
          )}
        {pending && (
          <div role="alert">
            操作结果待核对。
            <Button disabled={busy} onClick={() => void perform(pending, true)}>
              核对原操作
            </Button>
            <Button disabled={busy || !writable} onClick={() => void perform(pending)}>
              重试原操作
            </Button>
          </div>
        )}
        {exercise.attempt_id && !exercise.feedback?.assessment && (
          <p role="status">
            {state?.result_unknown
              ? '评估结果待核对，未重复发送。'
              : state?.can_resume
                ? '评估已暂停，答案已保存。'
                : state?.status === 'failed'
                  ? '评估未完成，答案已保存。'
                  : '正在评估…'}
          </p>
        )}
        {state?.can_resume && (
          <details>
            <summary>继续评估</summary>
            <p>使用当前模型连接 {state.destination}</p>
            <label>
              请求次数上限
              <input
                type="number"
                min={1}
                max={1000}
                value={requests}
                onChange={(e) => setRequests(Number(e.target.value))}
              />
            </label>
            <label>
              Token 预留上限
              <input
                type="number"
                min={1}
                max={10000000}
                value={tokens}
                onChange={(e) => setTokens(Number(e.target.value))}
              />
            </label>
            <Button
              disabled={!writable || busy || !!pending || requests < 1 || tokens < 1}
              onClick={() =>
                void perform({
                  kind: 'assessment',
                  operation: crypto.randomUUID(),
                  version: state.version,
                  configuration: state.configuration,
                  requests,
                  tokens,
                })
              }
            >
              确认并继续评估
            </Button>
          </details>
        )}
      </PracticeCard>
      <FeedbackCard
        exercise={exercise}
        disabled={!writable || busy || !!pending}
        onContinue={() =>
          void perform({
            kind: 'continue',
            operation: crypto.randomUUID(),
            version: exercise.session_version,
          })
        }
      />
    </>
  )
}
