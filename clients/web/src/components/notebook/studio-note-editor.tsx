import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from '@/api/client'
import {
  studioAPI,
  studioRequest,
  type NoteDraft,
  type NoteSourceCandidate,
  type StudioRevision,
} from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { AddSourceContent } from './add-source-dialog'
import { SubmissionNotice } from './submission-notice'
import { StudioSections } from './studio-sections'

export function StudioNoteEditor({
  spaceId,
  artifactId,
  archived,
  onBack,
  onSaved,
}: {
  spaceId: string
  artifactId?: string
  archived: boolean
  onBack: () => void
  onSaved: (note: StudioRevision) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const queryClient = useQueryClient()
  const key = JSON.stringify([...prefix, spaceId, 'studio-draft', artifactId ?? 'new'])
  const queryKey = [...prefix, spaceId, 'studio-artifact', artifactId]
  const api = studioAPI(session, spaceId)
  const saved = useQuery({
    queryKey,
    enabled: !!artifactId,
    queryFn: ({ signal }) => api.get(artifactId!, signal),
    refetchInterval: 5000,
    retry: false,
    gcTime: 0,
  })
  const history = useQuery({
    queryKey: [...queryKey, 'revisions'],
    enabled: !!artifactId,
    queryFn: ({ signal }) => api.history(artifactId!, signal),
    retry: false,
    gcTime: 0,
  })
  const [draft, setDraft] = useState<NoteDraft | undefined>(() => drafts.get(key))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [notice, setNotice] = useState('')
  const [reviewed, setReviewed] = useState<StudioRevision>()
  const [importVersion, setImportVersion] = useState<number>()
  const [candidate, setCandidate] = useState<NoteSourceCandidate>()
  const dialog = useRef<HTMLDialogElement>(null)
  const importButton = useRef<HTMLButtonElement>(null)
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  const denied = [error, saved.error, history.error].find(
    (e) =>
      e instanceof ApiError &&
      ([401, 403, 404].includes(e.status) ||
        ['content_redacted', 'privacy_clear_in_progress'].includes(e.code)),
  )
  useEffect(() => {
    if (denied) {
      drafts.delete(key)
      setDraft(undefined)
      setReviewed(undefined)
      setCandidate(undefined)
      dialog.current?.close()
    }
  }, [denied, drafts, key])
  const value: NoteDraft = draft ?? {
    title: saved.data?.body.title ?? '',
    markdown: saved.data?.body.markdown ?? '',
    baseVersion: saved.data?.version ?? 0,
    goalId: saved.data?.goal_id ?? null,
  }
  const persist = (next: NoteDraft) => {
    if (live.current) {
      drafts.set(key, next)
      setDraft(next)
    }
  }
  const canWrite = !archived && session.device.scopes.includes('learning:write')
  const canImport =
    !archived &&
    ['knowledge:read', 'knowledge:write', 'knowledge:approve'].every((scope) =>
      session.device.scopes.includes(scope),
    )
  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError(undefined)
    setNotice('')
    try {
      await fn()
    } catch (e) {
      if (live.current) setError(e)
    } finally {
      if (live.current) setBusy(false)
    }
  }
  const accept = (note: StudioRevision) => {
    if (!live.current) return
    const request = drafts.get<NoteDraft>(key)?.pending
    if (
      !request ||
      note.learning_space_id !== spaceId ||
      note.privacy_generation !== session.generation ||
      note.version !== request.expected_version + 1 ||
      note.body.title !== request.title ||
      note.body.markdown !== request.markdown ||
      (artifactId && note.artifact_id !== artifactId)
    )
      throw new ApiError(502, 'invalid_response')
    drafts.delete(key)
    setDraft(undefined)
    setReviewed(undefined)
    queryClient.setQueryData(queryKey, note)
    if (artifactId) void history.refetch()
    setNotice(`已保存第 ${note.version} 版`)
    onSaved(note)
  }
  const save = async (baseVersion = value.baseVersion) => {
    const next = { ...value, baseVersion, conflict: false }
    const request = next.pending ?? studioRequest(drafts, key, next, session.generation)
    persist({ ...next, pending: request, unknown: true })
    try {
      accept(await api.save(artifactId, request))
    } catch (e) {
      if (e instanceof ApiError && e.status >= 400 && e.status < 500)
        persist({ ...next, pending: undefined, unknown: false, conflict: e.status === 409 })
      throw e
    }
  }
  const reconcile = async () => {
    if (!value.pending) return
    try {
      accept(await api.operation(value.pending.operation_id))
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        persist({ ...value, unknown: false })
        setNotice('原操作尚未提交。可用原请求重试保存。')
        return
      }
      throw e
    }
  }
  if (denied)
    return (
      <>
        <Button variant="outline" onClick={onBack}>
          返回 Studio
        </Button>
        <ErrorState error={denied} />
      </>
    )
  if (artifactId && saved.isPending) return <p role="status">正在读取笔记…</p>
  if (saved.error) return <ErrorState error={saved.error} retry={() => void saved.refetch()} />
  if (saved.data?.body.schema === 2)
    return (
      <section aria-label="笔记编辑器">
        <Button variant="outline" onClick={onBack}>
          返回 Studio
        </Button>
        <h3>{saved.data.body.title}</h3>
        <p>已保存第 {saved.data.version} 版</p>
        <StudioSections
          key={`${saved.data.artifact_id}:${saved.data.version}`}
          revision={saved.data}
          canWrite={canWrite}
          onSaved={(next) => {
            queryClient.setQueryData(queryKey, next)
            onSaved(next)
          }}
        />
      </section>
    )
  return (
    <section className="studio-note-editor" aria-label="笔记编辑器">
      <Button variant="outline" onClick={onBack}>
        ← 返回 Studio
      </Button>
      <h3>{artifactId ? '编辑笔记' : '新笔记'}</h3>
      <p className="hint">
        {saved.data ? `已保存第 ${saved.data.version} 版` : '尚未保存'} ·
        草稿只在本标签页，刷新会丢失。
      </p>
      {!!error && <ErrorState error={error} />}
      {notice && <p role="status">{notice}</p>}
      {archived && <p>学习区已归档，可查看已保存版本。</p>}
      <fieldset disabled={!canWrite || busy || !!value.unknown}>
        <label>
          笔记标题
          <input
            maxLength={200}
            value={value.title}
            onChange={(e) => {
              persist({ ...value, title: e.target.value, pending: undefined })
              setReviewed(undefined)
            }}
          />
        </label>
        <label>
          笔记正文
          <textarea
            className="studio-note-body"
            rows={12}
            value={value.markdown}
            onChange={(e) => {
              persist({ ...value, markdown: e.target.value, pending: undefined })
              setReviewed(undefined)
            }}
          />
        </label>
      </fieldset>
      {value.conflict && (
        <div role="alert">
          <p>保存版本冲突，本地草稿已保留。请读取最新版本并比较后再保存。</p>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() =>
              void run(async () => {
                const latest = await api.get(artifactId!)
                if (live.current) setReviewed(latest)
              })
            }
          >
            读取最新版本并保留草稿
          </Button>
          {reviewed && (
            <>
              <h4>
                服务端第 {reviewed.version} 版：{reviewed.body.title}
              </h4>
              <pre className="reference-text">{reviewed.body.markdown}</pre>
              <Button
                disabled={busy || !canWrite}
                onClick={() => void run(() => save(reviewed.version))}
              >
                以已核对版本保存草稿
              </Button>
            </>
          )}
        </div>
      )}
      {value.unknown ? (
        <>
          <SubmissionNotice pending={!!value.unknown} inFlight={busy}>
            保存结果未知，草稿已保留。
            <Button variant="ghost" disabled={busy} onClick={() => void run(reconcile)}>
              核对笔记保存结果
            </Button>
          </SubmissionNotice>
        </>
      ) : (
        <Button
          disabled={
            busy || !canWrite || !!value.conflict || !value.title.trim() || !value.markdown.trim()
          }
          onClick={() => void run(() => save())}
        >
          保存笔记
        </Button>
      )}
      {saved.data && (
        <div className="studio-note-import">
          <label>
            转为来源的已保存版本
            <select
              value={importVersion ?? saved.data.version}
              onChange={(e) => setImportVersion(Number(e.target.value))}
            >
              {(history.data?.items ?? [{ version: saved.data.version }]).map((item) => (
                <option key={item.version} value={item.version}>
                  第 {item.version} 版
                </option>
              ))}
            </select>
          </label>
          <Button
            ref={importButton}
            variant="outline"
            disabled={!canImport || busy || !!value.unknown}
            onClick={() =>
              void run(async () => {
                const result = await api.candidate(
                  artifactId!,
                  importVersion ?? saved.data!.version,
                )
                if (live.current) {
                  setCandidate(result)
                  dialog.current?.showModal()
                }
              })
            }
          >
            转为来源
          </Button>
          <p className="hint">复制所选已保存版本；未保存内容不会导入，不自动修改课堂。</p>
        </div>
      )}
      <dialog
        ref={dialog}
        className="source-dialog expanded"
        aria-label="笔记转为来源"
        onClose={() => {
          setCandidate(undefined)
          importButton.current?.focus()
        }}
      >
        <h2>笔记转为来源</h2>
        <Button autoFocus variant="outline" onClick={() => dialog.current?.close()}>
          关闭笔记导入
        </Button>
        {candidate && (
          <AddSourceContent
            key={`${candidate.derived_from.artifact_id}:${candidate.derived_from.version}`}
            spaceId={spaceId}
            noteCandidate={candidate}
          />
        )}
      </dialog>
    </section>
  )
}
