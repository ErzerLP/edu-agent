import { useEffect, useRef, useState } from 'react'
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError } from '@/api/client'
import {
  studioAPI,
  type StudioRevision,
  type StudioSection,
  type NoteSourceCandidate,
} from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { AddSourceContent } from './add-source-dialog'

export function StudioSections({
  revision,
  canWrite,
  onSaved,
  onCitation,
}: {
  revision: StudioRevision
  canWrite: boolean
  onSaved: (next: StudioRevision) => void
  onCitation?: (artifact: string, version: number, id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const api = studioAPI(session, revision.learning_space_id)
  const pages = useInfiniteQuery({
    queryKey: [
      ...prefix,
      revision.learning_space_id,
      'studio-sections',
      revision.artifact_id,
      revision.version,
    ],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) =>
      api.sections(revision.artifact_id, revision.version, pageParam, signal),
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
    gcTime: 0,
  })
  const [candidate, setCandidate] = useState<NoteSourceCandidate>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const dialog = useRef<HTMLDialogElement>(null)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  useEffect(() => {
    if (
      pages.error instanceof ApiError &&
      ([401, 403, 404].includes(pages.error.status) ||
        ['content_redacted', 'privacy_clear_in_progress'].includes(pages.error.code))
    ) {
      drafts.deletePrefix(
        JSON.stringify([...prefix, revision.learning_space_id, 'studio-section-draft']).slice(
          0,
          -1,
        ) + ',',
      )
      setCandidate(undefined)
      dialog.current?.close()
    }
  }, [pages.error, drafts, prefix, revision.learning_space_id])
  const canImport =
    revision.kind === 'note' &&
    ['knowledge:read', 'knowledge:write', 'knowledge:approve'].every((s) =>
      session.device.scopes.includes(s),
    )
  if (pages.error) return <ErrorState error={pages.error} retry={() => void pages.refetch()} />
  return (
    <section aria-label="已保存段落">
      {!!revision.body.summary?.overview.length && (
        <section>
          <h4>总览</h4>
          {revision.body.summary.overview.map((claim) => (
            <p key={claim.id}>{claim.text}</p>
          ))}
        </section>
      )}
      {!!revision.body.summary?.transitions.length && (
        <section>
          <h4>衔接与回顾</h4>
          {revision.body.summary.transitions.map((claim) => (
            <p key={claim.id}>{claim.text}</p>
          ))}
        </section>
      )}
      {!!revision.body.summary?.uncovered.length && (
        <details>
          <summary>未覆盖范围</summary>
          <ul>
            {revision.body.summary.uncovered.map((text, i) => (
              <li key={i}>{text}</li>
            ))}
          </ul>
        </details>
      )}
      {pages.data?.pages
        .flatMap((p) => p.items)
        .map((section) => (
          <Section
            key={section.id}
            section={section}
            revision={revision}
            canEdit={canWrite && revision.kind === 'note'}
            onSaved={onSaved}
            onCitation={onCitation}
          />
        ))}
      {pages.hasNextPage && (
        <Button
          variant="outline"
          disabled={pages.isFetchingNextPage}
          onClick={() => void pages.fetchNextPage()}
        >
          更多已保存段落
        </Button>
      )}
      {canImport && (
        <>
          <Button
            variant="outline"
            disabled={busy}
            onClick={async () => {
              setBusy(true)
              setError(undefined)
              try {
                const result = await api.candidate(revision.artifact_id, revision.version)
                if (alive.current) {
                  setCandidate(result)
                  dialog.current?.showModal()
                }
              } catch (e) {
                if (alive.current) setError(e)
              } finally {
                if (alive.current) setBusy(false)
              }
            }}
          >
            转为来源
          </Button>
          <p className="hint">导入已保存第 {revision.version} 版的独立副本，先审阅再确认。</p>
        </>
      )}
      {!!error && <ErrorState error={error} />}
      <dialog
        ref={dialog}
        className="source-dialog expanded"
        aria-label="笔记转为来源"
        onClose={() => setCandidate(undefined)}
      >
        <h2>笔记转为来源</h2>
        <Button autoFocus variant="outline" onClick={() => dialog.current?.close()}>
          关闭笔记导入
        </Button>
        {candidate && (
          <AddSourceContent
            key={`${candidate.derived_from.artifact_id}:${candidate.derived_from.version}`}
            spaceId={revision.learning_space_id}
            noteCandidate={candidate}
          />
        )}
      </dialog>
    </section>
  )
}

type SectionDraft = { markdown: string; baseVersion: number }
function Section({
  section,
  revision,
  canEdit,
  onSaved,
  onCitation,
}: {
  section: StudioSection
  revision: StudioRevision
  canEdit: boolean
  onSaved: (next: StudioRevision) => void
  onCitation?: (artifact: string, version: number, id: string) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const key = JSON.stringify([
    ...prefix,
    revision.learning_space_id,
    'studio-section-draft',
    revision.artifact_id,
    section.id,
  ])
  const [draft, setDraft] = useState<SectionDraft | undefined>(() => drafts.get(key))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const denied =
    error instanceof ApiError &&
    ([401, 403, 404].includes(error.status) ||
      ['content_redacted', 'privacy_clear_in_progress'].includes(error.code))
  useEffect(() => {
    if (denied) {
      drafts.delete(key)
      setDraft(undefined)
    }
  }, [denied, drafts, key])
  const persist = (next: SectionDraft) => {
    drafts.set(key, next)
    setDraft(next)
  }
  const save = async () => {
    if (!draft) return
    setBusy(true)
    setError(undefined)
    const body = {
      privacy_generation: revision.privacy_generation,
      expected_version: draft.baseVersion,
      markdown: draft.markdown,
    }
    try {
      const next = await studioAPI(session, revision.learning_space_id).saveSection(
        revision.artifact_id,
        section.id,
        { ...body, operation_id: drafts.operation(key, body) },
      )
      if (!alive.current) return
      drafts.delete(key)
      setDraft(undefined)
      onSaved(next)
      void query.invalidateQueries({
        queryKey: [...prefix, revision.learning_space_id, 'studio-history', revision.artifact_id],
      })
    } catch (e) {
      if (alive.current) setError(e)
    } finally {
      if (alive.current) setBusy(false)
    }
  }
  if (denied) return <ErrorState error={error} />
  return (
    <section>
      <h4>
        {section.ordinal}. {section.body.title}
      </h4>
      {section.body.uncovered?.map((text, i) => (
        <p className="hint" key={i}>
          {text}
        </p>
      ))}
      {section.body.user_authored ? (
        <>
          <p className="hint">用户笔记 · 手工修改内容未经模型引用核验</p>
          <div style={{ whiteSpace: 'pre-wrap' }}>{section.body.markdown}</div>
        </>
      ) : (
        section.body.claims?.map((claim, i) => (
          <p key={i}>
            {claim.text}
            {onCitation &&
              claim.citations.map((id) => (
                <button
                  key={id}
                  type="button"
                  className="citation-link"
                  aria-label={`查看引用 ${section.ordinal}-${id}`}
                  onClick={() =>
                    onCitation(revision.artifact_id, revision.version, `${section.id}:${id}`)
                  }
                >
                  [{id}]
                </button>
              ))}
          </p>
        ))
      )}
      {!onCitation && !!section.body.citations.length && (
        <details>
          <summary>核验引用</summary>
          {section.body.citations.map((c) => (
            <blockquote key={c.id}>{c.fragment.text}</blockquote>
          ))}
        </details>
      )}
      {draft ? (
        <>
          <label>
            本段笔记
            <textarea
              rows={8}
              value={draft.markdown}
              disabled={!canEdit || busy}
              onChange={(e) => persist({ ...draft, markdown: e.target.value })}
            />
          </label>
          <p className="hint">保存后这一段标为用户笔记；原引用及模型内容保留在旧版本。</p>
          <Button disabled={!canEdit || busy || !draft.markdown.trim()} onClick={() => void save()}>
            保存本段为新版本
          </Button>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => {
              drafts.delete(key)
              setDraft(undefined)
            }}
          >
            取消编辑
          </Button>
          {error instanceof ApiError && error.status === 409 && (
            <>
              <p role="alert">产物版本已更新，你的输入已保留。先读取最新版本核对，再保存本段。</p>
              <Button
                variant="outline"
                disabled={busy}
                onClick={async () => {
                  try {
                    const latest = await studioAPI(session, revision.learning_space_id).get(
                      revision.artifact_id,
                    )
                    if (alive.current) {
                      persist({ ...draft, baseVersion: latest.version })
                      onSaved(latest)
                    }
                  } catch (e) {
                    if (alive.current) setError(e)
                  }
                }}
              >
                读取最新版本并保留输入
              </Button>
            </>
          )}
          {!!error && <ErrorState error={error} />}
        </>
      ) : (
        canEdit && (
          <Button
            variant="outline"
            onClick={() =>
              persist({ markdown: section.body.markdown, baseVersion: revision.version })
            }
          >
            编辑本段
          </Button>
        )
      )}
    </section>
  )
}
