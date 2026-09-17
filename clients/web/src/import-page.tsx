import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, errorText, learningClient, unwrap } from './api/client'
import { spaceSchema } from './api/runtime'
import {
  importAPI,
  importErrors,
  importStatus,
  type ImportJob,
  type ImportCommand,
} from './api/import-jobs'
import {
  scanImportFiles,
  selectedSources,
  uploadRemaining,
  continueImport,
  verifySources,
  type ScanItem,
} from './lib/import-files'
import { useIdentity } from './lib/session'
import { Button } from './components/ui/button'
import { Confirm, Pagination } from './components/common'
import { ImportDiff, ImportIdentityReview, ImportManifest } from './components/import-review'

export function ImportError({ error }: { error: unknown }) {
  if (!error) return null
  return (
    <div role="alert" className="notice error">
      {error instanceof ApiError ? (
        <>
          {importErrors[error.code] ?? errorText(error)}（{error.code}）
        </>
      ) : error instanceof Error ? (
        error.message
      ) : (
        errorText(error)
      )}
    </div>
  )
}

export function ImportCounts({ job }: { job: ImportJob }) {
  const counts = (statuses: string[]) =>
    job.batches.filter((batch) => statuses.includes(batch.status)).length
  const published = counts(['completed'])
  // 原服务只在上传且解析成功后离开 pending；后续暂存丢失或发布失败不抹去该事实。
  const parsed = job.batches.filter((batch) => batch.status !== 'pending').length
  return (
    <p role="status">
      总计 {job.batch_count} · 已上传 {parsed} · 解析完成 {parsed} · 剩余计划已确认{' '}
      {job.approved ? '是' : '否'} · 已发布 {published} · 未处理 {job.batch_count - published} ·
      结果未知 {counts(['unknown'])} · 失败 {counts(['failed'])} · 暂存缺失 {counts(['missing'])} ·
      预览失效 {counts(['stale'])}
    </p>
  )
}

export function ImportPage({
  spaceId,
  collectionId,
  jobId,
  draft,
}: {
  spaceId: string
  collectionId: string
  jobId: string
  draft: boolean
}) {
  const { session, prefix } = useIdentity()
  const cache = useQueryClient()
  const queryKey = [...prefix, spaceId, collectionId, 'import-job', jobId]
  const operation = useRef<AbortController | null>(null)
  const mounted = useRef(true)
  const [rows, setRows] = useState<ScanItem[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [uncertain, setUncertain] = useState(false)
  const [sourceChanged, setSourceChanged] = useState(false)
  const [approvedVersion, setApprovedVersion] = useState<number>()
  const [page, setPage] = useState(0)
  const [batch, setBatch] = useState<number>()
  const query = useQuery({
    queryKey,
    queryFn: ({ signal }) => importAPI(session, spaceId, collectionId, signal).get(jobId),
    gcTime: 0,
  })
  const space = useQuery({
    queryKey: [...prefix, spaceId, 'space'],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces/{spaceID}', {
          params: { path: { spaceID: spaceId } },
          signal,
        }),
        spaceSchema,
      ),
  })
  const job = query.error ? undefined : query.data
  const detail = useQuery({
    queryKey: [...queryKey, job?.version, 'batch', batch],
    enabled: !!job && batch !== undefined,
    queryFn: ({ signal }) => importAPI(session, spaceId, collectionId, signal).batch(jobId, batch!),
    gcTime: 0,
  })
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      operation.current?.abort()
    }
  }, [])
  useEffect(() => {
    setApprovedVersion(undefined)
  }, [job?.plan_version, job?.status])
  const write = session.device.scopes.includes('knowledge:write')
  const approve = write && session.device.scopes.includes('knowledge:approve')
  const closed = !!job && ['completed', 'cancelled', 'expired'].includes(job.status)
  const inactive = space.data?.status !== 'active'
  const update = (next: ImportJob) => {
    if (mounted.current) cache.setQueryData(queryKey, next)
  }
  const act = async (
    fn: (api: ReturnType<typeof importAPI>, signal: AbortSignal) => Promise<void>,
    mutation = true,
  ) => {
    if (operation.current) return
    const controller = new AbortController()
    operation.current = controller
    setBusy(true)
    setError(undefined)
    try {
      await fn(importAPI(session, spaceId, collectionId, controller.signal), controller.signal)
    } catch (e) {
      if (mounted.current) {
        setError(e)
        if (mutation) setUncertain(true)
      }
    } finally {
      if (mounted.current) {
        setBusy(false)
        operation.current = null
      }
    }
  }
  const reconcile = () =>
    act(async (api) => {
      const next = await api.get(jobId)
      update(next)
      setUncertain(false)
      await query.refetch()
    }, false)
  const command = (body: Omit<ImportCommand, 'id'>) =>
    act(async (api) => {
      update(await api.get(jobId))
      update(await api.command({ id: jobId, ...body }))
    })
  const choose = (files: File[]) =>
    act(async (_api, signal) => {
      const next = await scanImportFiles(files, signal)
      if (!mounted.current) return
      setRows(next)
      setApprovedVersion(undefined)
      if (job) {
        try {
          verifySources(job, selectedSources(next))
          setSourceChanged(false)
        } catch (e) {
          setSourceChanged(true)
          throw e
        }
      }
    }, false)
  const upload = () =>
    act(async (api, signal) => {
      const sources = selectedSources(rows)
      if (!job) {
        const created = await api.command({
          id: jobId,
          action: 'create',
          items: sources.map((source) => source.item),
        })
        signal.throwIfAborted()
        update(created)
      }
      await uploadRemaining(api, jobId, sources, signal, update)
      setSourceChanged(false)
      setUncertain(false)
      await query.refetch()
    })
  const canCreate = draft && query.error instanceof ApiError && query.error.status === 404
  const disabled = busy || inactive || !write || closed || uncertain || sourceChanged
  return (
    <div className="import-page">
      <Link to="/runs" search={{ space: spaceId, collection: collectionId }}>
        ← 返回任务中心
      </Link>
      <h1>可恢复导入任务</h1>
      <p>
        学习区：{space.data?.name ?? spaceId} · 集合：{collectionId}
      </p>
      <p>原任务：{jobId}</p>
      <p>
        单文件 4 MiB · 单请求 16 MiB · 单任务 1000 文件 / 128
        MiB。每批一个文件，跨批不保证全部成功或全部回滚。
      </p>
      <ImportError error={error || (!canCreate && query.error) || space.error} />
      {!session.capabilities.import_jobs && <p>服务器尚未启用导入任务。</p>}
      {!write && (
        <p>当前设备只能查看资料。新建导入需使用操作者签发的“导入”配对码；原任务仍归原创建设备。</p>
      )}
      {write && !approve && (
        <p>当前设备不能批准发布；需显式授予导入审批权限，已有设备不会自动增权。</p>
      )}
      {inactive && space.data && <p>学习区已归档，仅允许查询和取消。</p>}
      {job && (
        <>
          <h2>{importStatus[job.status]}</h2>
          <ImportCounts job={job} />
          <p>
            有效期至 {new Date(job.expires_at).toLocaleString('zh-CN')} · 计划版本{' '}
            {job.plan_version}
          </p>
          {job.cleanup_pending && <p>暂存清理尚未完成，可核对原任务再次清理。</p>}
        </>
      )}
      <div className="actions task-actions">
        <Button variant="outline" disabled={busy} onClick={() => void reconcile()}>
          核对原操作
        </Button>
        {job && (
          <Confirm
            label="取消后续批次"
            title="停止此任务后续发布？"
            disabled={busy || !write || job.status === 'completed' || job.status === 'cancelled'}
            onConfirm={() =>
              void act(async (api) => {
                update(await api.cancel(jobId))
                setUncertain(false)
              })
            }
          >
            已发布结果会保留，取消不会回滚它们。当前批次若已提交，以原操作回执为准。
          </Confirm>
        )}
        {busy && (
          <Button variant="outline" onClick={() => operation.current?.abort()}>
            停止本页后续请求
          </Button>
        )}
      </div>
      {uncertain && (
        <p role="alert">请求结果尚未核实。先核对原操作，再决定继续；不会自动重传全部文件。</p>
      )}
      {!closed && (job || canCreate) && (
        <section className="panel">
          <h2>{job ? '重新选择原来源并补传' : '选择来源并创建任务'}</h2>
          <p>
            未上传文件仍在你的设备上；刷新或退出后必须重新选择完整原清单并核对摘要。正文仅在本页内存中保存。
          </p>
          <label>
            选择文件
            <input
              type="file"
              multiple
              accept=".md,.txt"
              disabled={busy || inactive || !write}
              onChange={(e) => {
                const files = Array.from(e.target.files ?? [])
                e.target.value = ''
                void choose(files)
              }}
            />
          </label>
          <label>
            选择目录
            <input
              type="file"
              multiple
              {...{ webkitdirectory: '' }}
              disabled={busy || inactive || !write}
              onChange={(e) => {
                const files = Array.from(e.target.files ?? [])
                e.target.value = ''
                void choose(files)
              }}
            />
          </label>
          {rows.length > 0 && (
            <ImportManifest rows={rows} change={job ? undefined : setRows} disabled={busy} />
          )}
          <Button
            disabled={
              busy ||
              inactive ||
              !write ||
              !rows.some((row) => row.selected && !row.error) ||
              sourceChanged ||
              uncertain
            }
            onClick={() => void upload()}
          >
            {job ? '核对来源并补传缺失部分' : '创建任务并分段上传'}
          </Button>
        </section>
      )}
      {job && (
        <section className="panel" aria-label="完整批次计划">
          <h2>完整清单与批次计划</h2>
          <p>
            计划绑定以下全部 {job.batch_count}{' '}
            项及其摘要；分页仅改变显示范围。每次继续最多正式发布一个批次。
          </p>
          {job.batches.slice(page * 20, page * 20 + 20).map((item, offset) => (
            <article className="import-row" key={item.operation_id}>
              <h3>
                批次 {page * 20 + offset + 1}：{item.item.path}
              </h3>
              <p>
                {importStatus[item.status]} · {item.item.bytes.toLocaleString()} 字节
              </p>
              {item.error && (
                <p role="alert">
                  {importErrors[item.error] ?? item.error}（{item.error}）
                </p>
              )}
              <details>
                <summary>来源与原操作标识</summary>
                <p>SHA-256：{item.item.sha256}</p>
                <p>operation：{item.operation_id}</p>
                <p>请求摘要：{item.request_hash ?? '尚未提交'}</p>
                <p>计划 manifest：{item.planned_manifest ?? '尚未规划'}</p>
                {item.result && (
                  <p>
                    正式版本：{item.result.revision.revision_id} · 新增{' '}
                    {item.result.summary?.added ?? 0} / 更新 {item.result.summary?.updated ?? 0} /
                    未变化 {item.result.summary?.unchanged ?? 0}
                  </p>
                )}
              </details>
              {item.preview && (
                <>
                  <p>
                    预览：新增 {item.preview.summary.added} / 更新 {item.preview.summary.updated} /
                    未变化 {item.preview.summary.unchanged}
                  </p>
                  <Button
                    variant="outline"
                    disabled={busy}
                    onClick={() => setBatch(page * 20 + offset)}
                  >
                    查看差异与身份
                  </Button>
                </>
              )}
            </article>
          ))}
          <Pagination
            page={page + 1}
            previous={page ? () => setPage(page - 1) : undefined}
            next={(page + 1) * 20 < job.batches.length ? () => setPage(page + 1) : undefined}
          />
          <div className="actions">
            <Button
              disabled={
                disabled ||
                job.batches.some((item) => ['pending', 'missing', 'unknown'].includes(item.status))
              }
              onClick={() => void command({ action: 'preview' })}
            >
              预览完整剩余计划
            </Button>
          </div>
          {job.status === 'ready' && (
            <>
              <label>
                <input
                  type="checkbox"
                  disabled={disabled || !approve}
                  checked={approvedVersion === job.plan_version}
                  onChange={(e) =>
                    setApprovedVersion(e.target.checked ? job.plan_version : undefined)
                  }
                />
                确认完整清单和计划版本 {job.plan_version}，同意逐批发布；已发布批次不会自动回滚。
              </label>
              <Button
                disabled={disabled || !approve || approvedVersion !== job.plan_version}
                onClick={() => void command({ action: 'confirm', plan_version: job.plan_version })}
              >
                确认完整计划
              </Button>
            </>
          )}
          {job.approved && (
            <div className="actions">
              <Button
                disabled={disabled || !approve}
                onClick={() =>
                  void act(async (api, signal) => {
                    await continueImport(api, jobId, signal, update)
                  })
                }
              >
                核对并继续一批
              </Button>
              <Button
                disabled={disabled || !approve}
                onClick={() =>
                  void act(async (api, signal) => {
                    await continueImport(api, jobId, signal, update, true)
                  })
                }
              >
                核对并逐批继续
              </Button>
            </div>
          )}
        </section>
      )}
      {batch !== undefined && job && (
        <section className="panel" aria-label="批次审阅">
          <h2>批次 {batch + 1} 审阅</h2>
          <Button variant="outline" onClick={() => setBatch(undefined)}>
            关闭批次审阅
          </Button>
          <ImportError error={detail.error} />
          {detail.data?.preview && (
            <>
              <ImportDiff preview={detail.data.preview} />
              <ImportIdentityReview
                key={`${job.plan_version}:${batch}`}
                preview={detail.data.preview}
                disabled={disabled}
                submit={(decisions) => void command({ action: 'resolve', batch, ...decisions })}
              />
            </>
          )}
        </section>
      )}
    </div>
  )
}
