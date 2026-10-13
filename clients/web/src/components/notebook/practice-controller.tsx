import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import {
  availableHelp,
  type PracticeExercise,
  type ExerciseHelp,
  type exerciseHelpSchema,
} from '@/api/exercises'
import { mentorReceiptSchema } from '@/api/mentor'
import { contentHeader, operationReceipt, operationResult } from '@/api/teaching'
import type { SourceSelection } from '@/api/source-selection'
import type { components } from '@/api/schema'
import type { z } from 'zod'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { FeedbackCard } from './feedback-card'
import { PracticeCard } from './practice-card'

export type PracticeTransport = {
  help?: (
    operation: string,
    reconcile: boolean,
    selection?: SourceSelection,
  ) => Promise<z.infer<typeof exerciseHelpSchema>>
  resumeAssessment: (
    body: components['schemas']['StudioAssessmentResume'],
  ) => Promise<z.infer<typeof mentorReceiptSchema>>
  change?: (
    kind: 'defer' | 'resume',
    operation: string,
  ) => Promise<z.infer<typeof mentorReceiptSchema>>
}

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
  | { kind: 'help'; operation: string; selection?: SourceSelection }
  | { kind: 'defer' | 'resume'; operation: string }
  | {
      kind: 'assessment'
      operation: string
      version: number
      configuration: string
      requests: number
      tokens: number
    }

export function PracticeController({
  exercise,
  selection,
  disabled,
  refresh,
  onSource,
  draftScope,
  transport,
  renderCitation,
}: {
  exercise: PracticeExercise
  selection?: SourceSelection
  disabled: boolean
  refresh: () => Promise<void>
  onSource?: () => void
  draftScope: (string | number)[]
  transport: PracticeTransport
  renderCitation?: (referenceId: string, index: number) => ReactNode
}) {
  const { session, prefix, drafts } = useIdentity(),
    query = useQueryClient()
  const key = JSON.stringify([
    ...prefix,
    exercise.space_id,
    ...draftScope,
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
    options = availableHelp(exercise.allowed_help, exercise.help_floor)
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
        if (!transport.help) throw new ApiError(400, 'hint_unavailable')
        const result = await transport.help(p.operation, reconcile, p.selection)
        if (result.operation_id !== p.operation) throw new ApiError(502, 'invalid_response')
        drafts.set(key + ':hint', result.text)
        if (live.current) {
          setHint(result.text)
          setHelp(result.level)
          drafts.set(key + ':help', result.level)
        }
      } else {
        const result = reconcile
          ? await unwrap(
              client().GET('/v1/learning/operations/{operationID}', {
                params: { header, path: { operationID: p.operation } },
              }),
              mentorReceiptSchema,
            )
          : p.kind === 'assessment'
            ? await transport.resumeAssessment({
                operation_id: p.operation,
                expected_version: p.version,
                configuration: p.configuration,
                request_budget: p.requests,
                token_budget: p.tokens,
              })
            : await transport.change!(p.kind, p.operation)
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
          transport.help && exercise.allowed_help.includes('hint')
            ? () => void perform({ kind: 'help', operation: crypto.randomUUID(), selection })
            : undefined
        }
        onDefer={
          transport.change
            ? () => void perform({ kind: 'defer', operation: crypto.randomUUID() })
            : undefined
        }
        onResume={
          transport.change
            ? () => void perform({ kind: 'resume', operation: crypto.randomUUID() })
            : undefined
        }
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
        renderCitation={renderCitation}
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
