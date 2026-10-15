import { useEffect, useRef, useState } from 'react'
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query'
import { z } from 'zod'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { goalSchema, type Space } from '@/api/runtime'
import { SpaceDialog } from './space-dialog'
import { settingsSchema } from '@/api/settings'
import {
  isMentorTerminal,
  mentorReceiptSchema,
  mentorSnapshotSchema,
  mentorStatus,
} from '@/api/mentor'
import { studioAPI, type StudioGenerate, type StudioRevision } from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { useSourceSelection } from './source-selection'
import { SubmissionNotice } from './submission-notice'

export const studioNames = { guide: '学习指南', brief: '简报', note: '笔记', quiz: '测验' }
const reasons: Record<string, string> = {
  output_budget_exceeded: '模型输出预算耗尽，结果未通过完整校验，未发布产物。',
  response_not_completed: '模型未完成输出，未发布产物。',
  schema_mismatch: '模型结果不符合结构要求，未发布产物。',
  invalid_source_citation: '引用核验失败，未发布产物。',
  source_coverage_partial: '内容已保存；所选资料覆盖不全，解析缺口或截断范围未补写。打开产物查看覆盖说明。',
  lease_expired: '运行租约失效；已发出的请求不会自动重试。',
  budget_exhausted: '预算不足，尚未发出下一次模型请求。',
  source_unavailable: '来源权限或版本不可用，已隐藏受保护内容。',
  studio_version_conflict: '产物已有新版本，本次结果未发布。请重开最新版本。',
}

export function StudioRuns({ spaceId, onOpen }: { spaceId: string; onOpen: (id: string) => void }) {
  const { session, prefix } = useIdentity()
  const runs = useInfiniteQuery({
    queryKey: [...prefix, spaceId, 'studio-runs'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/runs', {
          params: {
            header: { 'X-Learning-Space-ID': spaceId },
            query: { task_kind: 'studio', cursor: pageParam },
          },
          signal,
        }),
        z.object({ items: z.array(mentorSnapshotSchema), next_cursor: z.string().optional() }),
      ),
    getNextPageParam: (page) => page.next_cursor,
    gcTime: 0,
    retry: false,
    refetchInterval: 2500,
  })
  return (
    <section aria-label="Studio 生成记录">
      <h3>生成记录</h3>
      {runs.error ? (
        <ErrorState error={runs.error} />
      ) : (
        runs.data?.pages
          .flatMap((page) => page.items)
          .map((run) => (
            <Button key={run.run_id} variant="outline" onClick={() => onOpen(run.run_id)}>
              查看生成 · {mentorStatus[run.status]} · {new Date(run.updated_at).toLocaleString()}
            </Button>
          ))
      )}
      {runs.hasNextPage && <Button onClick={() => void runs.fetchNextPage()}>更多生成记录</Button>}
    </section>
  )
}

export function StudioRun({
  spaceId,
  runId,
  onOpen,
}: {
  spaceId: string
  runId: string
  onOpen: (id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const header = { 'X-Learning-Space-ID': spaceId }
  const client = learningClient(session, spaceId)
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const run = useQuery({
    queryKey: [...prefix, spaceId, 'studio-run', runId],
    queryFn: ({ signal }) =>
      unwrap(
        client.GET('/v1/learning/runs/{runID}', {
          params: { header, path: { runID: runId } },
          signal,
        }),
        mentorSnapshotSchema,
      ),
    gcTime: 0,
    retry: false,
    refetchInterval: 1500,
  })
  useEffect(() => {
    if (run.data && isMentorTerminal(run.data.status))
      void query.invalidateQueries({ queryKey: [...prefix, spaceId, 'studio-list'] })
  }, [run.data?.status, query, prefix, spaceId])
  const state = run.data
  const command = async (kind: 'stop' | 'continue_budget') => {
    if (!state) return
    setBusy(true)
    setError(undefined)
    try {
      for (let attempt = 0; attempt < 2; attempt++) {
        const current = await unwrap(
          client.GET('/v1/learning/runs/{runID}', { params: { header, path: { runID: runId } } }),
          mentorSnapshotSchema,
        )
        if (isMentorTerminal(current.status) || current.status === 'cancelling') break
        const body = {
          kind,
          expected_version: current.version,
          ...(kind === 'continue_budget'
            ? {
                request_budget: current.studio?.request.request_budget ?? 1,
                token_budget: current.studio?.request.token_budget ?? 50000,
              }
            : {}),
        }
        try {
          await unwrap(
            client.POST('/v1/learning/runs/{runID}/commands', {
              params: { header, path: { runID: runId } },
              body: {
                ...body,
                operation_id: drafts.operation(
                  JSON.stringify([...prefix, spaceId, runId, kind]),
                  body,
                ),
              },
            }),
            mentorReceiptSchema,
          )
          break
        } catch (e) {
          // Only a definitive version rejection permits rebasing the same stop intent.
          // An unknown response never gets a fresh operation automatically.
          if (
            kind !== 'stop' ||
            attempt > 0 ||
            !(e instanceof ApiError) ||
            e.code !== 'version_conflict'
          )
            throw e
        }
      }
      await run.refetch()
    } catch (e) {
      setError(e)
      void run.refetch()
    } finally {
      setBusy(false)
    }
  }
  if (run.error) return <ErrorState error={run.error} retry={() => void run.refetch()} />
  if (!state) return <p role="status">正在核对生成状态…</p>
  return (
    <section aria-label="Studio 运行状态">
      <p role="status">{state.status === 'running' ? '正在生成' : mentorStatus[state.status]}</p>
      {state.reason && <p>{reasons[state.reason] ?? `运行说明：${state.reason}`}</p>}
      {state.result_unknown && (
        <p role="alert">结果未知：请求可能已发送，但未确认发布；不会自动重复生成。</p>
      )}
      {state.cost_unknown && <p className="hint">提供商费用尚未核实。</p>}
      <p className="hint">
        剩余请求 {state.requests_left} · 剩余 token 预算 {state.tokens_left}
      </p>
      {state.body_available && state.source_manifest && (
        <p>本次使用 {state.source_manifest.entries.length} 个来源；后续勾选不会改变本次生成。</p>
      )}
      {state.body_available && state.studio?.result && (
        <Button onClick={() => onOpen(state.studio!.result!.artifact_id)}>打开已保存产物</Button>
      )}
      {!isMentorTerminal(state.status) && state.status !== 'cancelling' && (
        <Button disabled={busy} variant="outline" onClick={() => void command('stop')}>
          取消生成
        </Button>
      )}
      {state.status === 'paused_budget' && (
        <Button disabled={busy} onClick={() => void command('continue_budget')}>
          按原请求预算继续
        </Button>
      )}
      {!!error && <ErrorState error={error} retry={() => void run.refetch()} />}
    </section>
  )
}

export function StudioGenerator({
  space,
  kind,
  base,
  onBack,
  onOpen,
}: {
  space: Space
  kind: 'guide' | 'brief' | 'quiz'
  base?: StudioRevision
  onBack: () => void
  onOpen: (id: string) => void
}) {
  const spaceId = space.id
  const { session, prefix, drafts } = useIdentity()
  const { selection } = useSourceSelection()
  const key = JSON.stringify([...prefix, spaceId, 'studio-generation', base?.artifact_id ?? kind])
  const [pending, setPending] = useState<StudioGenerate | undefined>(() => drafts.get(key))
  const [runId, setRunId] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [tokens, setTokens] = useState(50000)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const directionMode = kind === 'quiz' && (!base || !!base.body.quiz?.direction)
  const targetGoal = !directionMode ? (base?.goal_id ?? undefined) : undefined
  const configuration = useQuery({
    queryKey: [...prefix, 'settings'],
    queryFn: ({ signal }) =>
      unwrap(learningClient(session).GET('/v1/settings', { signal }), settingsSchema),
    retry: false,
  })
  const modelReady =
    configuration.data?.effective_mentor.enabled && configuration.data.effective_mentor.configured
  const goal = useQuery({
    queryKey: [...prefix, spaceId, targetGoal, 'studio-goal'],
    enabled: !!targetGoal,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: targetGoal! } },
          signal,
        }),
        goalSchema,
      ),
    gcTime: 0,
    retry: false,
  })
  const accepted = (id: string) => {
    drafts.delete(key)
    if (alive.current) {
      setPending(undefined)
      setRunId(id)
    }
  }
  const submit = async () => {
    if (
      !modelReady ||
      !selection ||
      selection.mode !== 'items' ||
      (targetGoal && !goal.data) ||
      (directionMode && !space.direction_version)
    )
      return
    setBusy(true)
    setError(undefined)
    const common = {
      operation_id: crypto.randomUUID(),
      privacy_generation: session.generation,
      artifact_id: base?.artifact_id ?? null,
      expected_version: base?.version ?? 0,
      source_selection: selection,
      request_budget: 1,
      token_budget: tokens,
    }
    const body: StudioGenerate =
      pending ??
      (directionMode
        ? {
            ...common,
            kind: 'quiz',
            goal_id: null,
            goal_version: 0,
            mode: 'direction',
            direction_version: space.direction_version!,
          }
        : kind === 'quiz'
          ? { ...common, kind, goal_id: targetGoal!, goal_version: goal.data!.revision }
          : {
              ...common,
              kind,
              goal_id: targetGoal ?? null,
              goal_version: goal.data?.revision ?? 0,
            })
    drafts.set(key, body)
    setPending(body)
    try {
      const receipt = await studioAPI(session, spaceId).generate(body)
      accepted(receipt.run_id)
    } catch (e) {
      if (alive.current) {
        setError(e)
        if (
          e instanceof ApiError &&
          ((e.status >= 400 && e.status < 500) || e.code === 'mentor_not_configured')
        ) {
          drafts.delete(key)
          setPending(undefined)
        }
      }
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  const reconcile = async () => {
    if (!pending) return
    setBusy(true)
    setError(undefined)
    try {
      const receipt = await unwrap(
        learningClient(session, spaceId).GET('/v1/learning/operations/{operationID}', {
          params: {
            header: { 'X-Learning-Space-ID': spaceId },
            path: { operationID: pending.operation_id },
          },
        }),
        mentorReceiptSchema,
      )
      accepted(receipt.run_id)
    } catch (e) {
      if (alive.current) setError(e)
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  return (
    <section aria-label={`${studioNames[kind]}生成`}>
      <Button variant="outline" onClick={onBack}>
        返回 Studio
      </Button>
      <h3>
        {base ? '重新生成' : '生成'}
        {studioNames[kind]}
      </h3>
      {runId ? (
        <StudioRun spaceId={spaceId} runId={runId} onOpen={onOpen} />
      ) : (
        <>
          <p>
            使用已选的 {selection?.items.length ?? 0} 个来源。
            {directionMode && space.direction ? `学习方向：${space.direction}` : ''}
          </p>
          {!modelReady && <p>请先在设置中配置并启用 Web 导师模型。</p>}
          {directionMode && !space.direction_version && (
            <SpaceDialog space={space} confirmDirection />
          )}
          {modelReady && (
            <p className="hint">
              所选片段{directionMode ? '和学习方向' : ''}将发送到{' '}
              {configuration.data?.effective_mentor.endpoint}。
              {directionMode
                ? '每题作答使用独立评估预算，最多自动重试一次，受设置中的请求次数和 Token 上限约束。'
                : ''}
            </p>
          )}
          {(!selection || selection.mode === 'none') && <p>请先勾选至少一个可读取的来源。</p>}
          <label>
            本次生成 Token 预算
            <input
              type="number"
              min={1}
              max={10000000}
              value={tokens}
              disabled={!!pending}
              onChange={(e) => setTokens(Number(e.target.value))}
            />
          </label>
          <SubmissionNotice pending={!!pending} inFlight={busy}>
            {pending && <>
              请求结果待核对，已选定 {pending.source_selection.items.length}{' '}
              个来源。请先核对原操作；重新发送也会使用同一操作和原来源选择。
            <Button variant="ghost" disabled={busy} onClick={() => void reconcile()}>
              核对原生成操作
            </Button>
            </>}
          </SubmissionNotice>
          <Button
            disabled={
              busy ||
              !modelReady ||
              (directionMode && !space.direction_version) ||
              !selection?.items.length ||
              !!goal.error ||
              (!!targetGoal && !goal.data) ||
              !Number.isInteger(tokens) ||
              tokens < 1
            }
            onClick={() => void submit()}
          >
            {busy ? '正在提交…' : pending ? '重新发送原请求' : `开始生成${studioNames[kind]}`}
          </Button>
          {goal.error && <ErrorState error={goal.error} />}
          {!!error && <ErrorState error={error} />}
        </>
      )}
    </section>
  )
}
