import { useEffect } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { studioJobsAPI, type StudioJob } from '@/api/studio-jobs'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'

export function StudioJobProgress({
  spaceId,
  job,
  busy,
  canWrite,
  onCommand,
  onOpen,
}: {
  spaceId: string
  job: StudioJob
  busy: boolean
  canWrite: boolean
  onCommand: (kind: 'stop' | 'resume' | 'retry' | 'save_as_new') => void
  onOpen: (id: string) => void
}) {
  const { session, prefix } = useIdentity()
  const units = useInfiniteQuery({
    queryKey: [...prefix, spaceId, 'studio-units', job.id, job.confirmed_plan_revision],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) =>
      studioJobsAPI(session, spaceId).units(job.id, pageParam, signal),
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
    gcTime: 0,
  })
  useEffect(() => {
    void units.refetch()
  }, [job.completed_units, job.status])
  const active = ['queued', 'generating', 'summarizing'].includes(job.status)
  const quiz = job.kind === 'quiz'
  const unitLabel = quiz ? '题目' : '段落'
  const allDone = job.total_units > 0 && job.completed_units === job.total_units
  return (
    <section aria-label="生成进度">
      <p role="status" aria-live="polite">
        已完成 {job.completed_units}/{job.total_units}
        {quiz ? ' 题' : ''}
      </p>
      <progress
        aria-label={`已完成${unitLabel}`}
        value={job.completed_units}
        max={Math.max(1, job.total_units)}
      />
      {job.status !== 'completed' && (
        <p className="hint">
          {quiz
            ? '全部题目及汇总完成后才发布测验并开放作答。'
            : '可查看已完成部分；全部段落及汇总完成后才发布产物。'}
        </p>
      )}
      {job.status === 'summarizing' && (
        <p>{quiz ? '正在整理练习覆盖范围和建议作答顺序…' : '正在整理总览、衔接和未覆盖范围…'}</p>
      )}
      {job.result_unknown && (
        <p className="hint">
          停止前的调用结果未知，迟到正文不会保存。
          {job.cost_unknown ? '已发生用量仍待服务方结算。' : '已发生用量已记录。'}
        </p>
      )}
      {job.status === 'failed' && (
        <p role="alert">
          {job.reason === 'studio_version_conflict'
            ? `目标产物已有新版本。已保留全部${unitLabel}，可以另存为新产物。`
            : allDone
              ? `汇总或发布未完成，已保留全部${unitLabel}。`
              : `当前${unitLabel}未完成，已保留之前通过核验的部分。`}
        </p>
      )}
      {canWrite && (
        <div className="studio-plan-buttons">
          {active && (
            <Button variant="outline" disabled={busy} onClick={() => onCommand('stop')}>
              停止生成
            </Button>
          )}
          {job.status === 'stopped' && (
            <Button disabled={busy} onClick={() => onCommand('resume')}>
              继续生成
            </Button>
          )}
          {job.status === 'failed' && job.reason !== 'studio_version_conflict' && (
            <Button disabled={busy} onClick={() => onCommand('retry')}>
              {allDone ? '重试汇总' : '重试剩余部分'}
            </Button>
          )}
          {job.reason === 'studio_version_conflict' && (
            <Button disabled={busy} onClick={() => onCommand('save_as_new')}>
              另存为新产物
            </Button>
          )}
        </div>
      )}
      {job.status === 'completed' && job.result && (
        <Button onClick={() => onOpen(job.result!.artifact_id)}>打开已保存产物</Button>
      )}
      {units.error ? (
        <ErrorState error={units.error} retry={() => void units.refetch()} />
      ) : (
        <ol aria-label={`生成${unitLabel}`}>
          {units.data?.pages
            .flatMap((page) => page.items)
            .map((unit) => (
              <li key={unit.id}>
                {unit.body ? (
                  <details>
                    <summary>
                      {unit.ordinal}. {unit.topic} · 已完成
                    </summary>
                    {unit.body.claims?.map((claim, index) => (
                      <p key={index}>{claim.text}</p>
                    ))}
                    <details>
                      <summary>核验引用</summary>
                      {unit.body.citations.map((citation) => (
                        <blockquote key={citation.id}>{citation.fragment.text}</blockquote>
                      ))}
                    </details>
                  </details>
                ) : (
                  <p>
                    {unit.ordinal}. {unit.topic} ·{' '}
                    {unit.status === 'succeeded'
                      ? '已完成'
                      : unit.status === 'running' && active
                        ? '正在生成'
                        : unit.status === 'failed'
                          ? '未通过生成或核验'
                          : '待生成'}
                  </p>
                )}
              </li>
            ))}
        </ol>
      )}
      {units.hasNextPage && (
        <Button
          variant="outline"
          disabled={units.isFetchingNextPage}
          onClick={() => void units.fetchNextPage()}
        >
          更多{unitLabel}
        </Button>
      )}
    </section>
  )
}
