import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from '@/api/client'
import { studioAPI, type StudioRevision, type StudioQuizStart } from '@/api/studio'
import { helpLabels, stateLabels } from '@/api/teaching'
import { useIdentity } from '@/lib/session'
import { TeachingPage } from '@/teaching-page'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { StudioPracticeCard } from './studio-practice-card'

export function StudioQuiz({
  revision,
  canWrite,
  onCitation,
}: {
  revision: StudioRevision
  canWrite: boolean
  onCitation: (artifact: string, version: number, id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const queryClient = useQueryClient()
  const { learning_space_id: spaceId, artifact_id: id, version } = revision
  const api = studioAPI(session, spaceId)
  const key = JSON.stringify([...prefix, spaceId, 'studio-quiz-start', id, version])
  const queryKey = [...prefix, spaceId, 'studio-quiz', id, version]
  const [pending, setPending] = useState<StudioQuizStart | undefined>(() => drafts.get(key))
  const [active, setActive] = useState<number>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const state = useQuery({
    queryKey,
    queryFn: ({ signal }) => api.quiz(id, version, signal),
    gcTime: 0,
    retry: false,
    refetchInterval: 2000,
  })
  const questions = revision.body.quiz!.questions
  const records = state.data?.questions ?? []
  const selected = records.find((q) => q.question === (active ?? records.at(-1)?.question))
  const accepted = async (result: Awaited<ReturnType<typeof api.startQuiz>>, question: number) => {
    if (!alive.current) return
    drafts.delete(key)
    setPending(undefined)
    setActive(question)
    queryClient.setQueryData(queryKey, result)
    await queryClient.invalidateQueries({
      queryKey: [...prefix, spaceId, revision.goal_id, 'teaching-sessions'],
    })
  }
  const start = async (question: number) => {
    if (!state.data || busy || !canWrite) return
    const command = pending ?? {
      operation_id: crypto.randomUUID(),
      version,
      privacy_generation: session.generation,
      goal_version: state.data.goal_version,
      question,
    }
    drafts.set(key, command)
    setPending(command)
    setBusy(true)
    setError(undefined)
    try {
      await accepted(await api.startQuiz(id, command), command.question)
    } catch (e) {
      if (alive.current) {
        setError(e)
        if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
          drafts.delete(key)
          setPending(undefined)
        }
      }
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  const reconcile = async () => {
    if (!pending || busy) return
    setBusy(true)
    setError(undefined)
    try {
      await accepted(await api.quizOperation(pending.operation_id), pending.question)
    } catch (e) {
      if (alive.current) setError(e)
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  const denied =
    state.error instanceof ApiError &&
    ([401, 403, 404].includes(state.error.status) ||
      ['content_redacted', 'privacy_clear_in_progress'].includes(state.error.code))
  useEffect(() => {
    if (denied) {
      drafts.delete(key)
      setPending(undefined)
    }
  }, [denied, drafts, key])
  if (state.error) return <ErrorState error={state.error} retry={() => void state.refetch()} />
  return (
    <section aria-label="测验作答">
      <p>共 {questions.length} 题。开始作答后，答案会记入练习记录和学习进度。</p>
      <p role="status">
        已完成 {records.filter((q) => q.completed).length} / {questions.length} 题
      </p>
      {questions.map((question, i) => {
        const n = i + 1,
          record = records.find((q) => q.question === n)
        const ready = n === 1 || records.some((q) => q.question === n - 1 && q.completed)
        return (
          <section key={n} aria-label={`测验第 ${n} 题`}>
            <h4>第 {n} 题</h4>
            <p>{question.prompt}</p>
            <p className="hint">
              允许帮助：{question.allowed_help.map((h) => helpLabels[h]).join('、')}
            </p>
            {question.citations.map((c) => (
              <button
                type="button"
                className="citation-link"
                key={c}
                aria-label={`查看引用 ${c}`}
                onClick={() => onCitation(id, version, c)}
              >
                [{c}]
              </button>
            ))}
            {record ? (
              <>
                <p>{record.completed ? '已完成' : (stateLabels[record.state] ?? record.state)}</p>
                <Button variant="outline" onClick={() => setActive(n)}>
                  查看第 {n} 题作答
                </Button>
                {record.attempt_id && (
                  <Link
                    to="/spaces/$spaceId/feedback/$attemptId"
                    params={{ spaceId, attemptId: record.attempt_id }}
                  >
                    查看第 {n} 题反馈记录
                  </Link>
                )}
              </>
            ) : (
              <Button
                disabled={!canWrite || !ready || busy || !!pending || !state.data}
                onClick={() => void start(n)}
              >
                {n === 1 ? '开始作答' : `开始第 ${n} 题`}
              </Button>
            )}
          </section>
        )
      })}
      {pending && (
        <>
          <p role="alert">开始结果待核对，请先核对原操作。</p>
          <Button disabled={busy} onClick={() => void reconcile()}>
            核对原开始操作
          </Button>
          <Button
            disabled={busy || !canWrite}
            variant="outline"
            onClick={() => void start(pending.question)}
          >
            重新发送原开始请求
          </Button>
        </>
      )}
      {!!error && <ErrorState error={error} />}
      {selected && (
        <section aria-label={`第 ${selected.question} 题作答`}>
          {selected.exercise_id ? (
            <StudioPracticeCard
              key={selected.exercise_id}
              revision={revision}
              record={{ ...selected, exercise_id: selected.exercise_id }}
              disabled={!canWrite}
              onCitation={onCitation}
            />
          ) : (
            !selected.completed &&
            selected.state !== 'Completed' && (
              <TeachingPage
                key={selected.session_id}
                spaceId={spaceId}
                sessionId={selected.session_id}
                embedded
              />
            )
          )}
        </section>
      )}
      <Link
        to="/progress"
        search={{
          space: spaceId,
          goal: revision.goal_id ?? undefined,
          status: 'all',
          order: 'recent',
          view: 'goals',
        }}
      >
        查看学习进度
      </Link>
    </section>
  )
}
