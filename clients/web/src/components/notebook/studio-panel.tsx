import {
  ArrowRight,
  BookOpen,
  ClipboardList,
  History,
  Presentation,
  StickyNote,
} from 'lucide-react'
import { useEffect, useState } from 'react'
import { ApiError } from '@/api/client'
import type { Space } from '@/api/runtime'
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { studioAPI, type StudioRevision } from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { StudioNoteEditor } from './studio-note-editor'
import { StudioDocument } from './studio-document'
import { StudioGenerator, StudioRun, StudioRuns, studioNames } from './studio-generation'

const tiles = [
  { name: '学习指南', kind: 'guide', Icon: BookOpen, color: 1, description: '梳理要点与思路' },
  { name: '测验', kind: 'quiz', Icon: ClipboardList, color: 0, description: '检验你的理解' },
  { name: '简报', kind: 'brief', Icon: Presentation, color: 2, description: '把资料读薄一点' },
]

export function StudioPanel({
  space,
  onCitation,
  archived = false,
}: {
  space: Space
  onCitation: (artifact: string, version: number, id: string) => void
  archived?: boolean
}) {
  const spaceId = space.id
  const { session, prefix, drafts } = useIdentity()
  const queryClient = useQueryClient()
  const [selected, setSelected] = useState<string>()
  const [document, setDocument] = useState<string>()
  const [generation, setGeneration] = useState<{
    kind: 'guide' | 'brief' | 'quiz'
    base?: StudioRevision
  }>()
  const [runId, setRunId] = useState<string>()
  const canRead = session.device.scopes.includes('learning:read')
  const canWrite = session.device.scopes.includes('learning:write') && !archived
  const canGenerate =
    canWrite && session.device.scopes.includes('knowledge:read') && session.capabilities.runs
  const back = () => {
    setDocument(undefined)
    setGeneration(undefined)
    setRunId(undefined)
  }
  const openDocument = (id: string) => {
    back()
    setDocument(id)
    void queryClient.invalidateQueries({ queryKey: [...prefix, spaceId, 'studio-list'] })
  }
  const notes = useInfiniteQuery({
    queryKey: [...prefix, spaceId, 'studio-list'],
    enabled: canRead,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) => studioAPI(session, spaceId).list(pageParam, signal),
    getNextPageParam: (page) => page.next_cursor,
    refetchInterval: 5000,
    retry: false,
    gcTime: 0,
  })
  const denied =
    notes.error instanceof ApiError &&
    ([401, 403, 404].includes(notes.error.status) ||
      ['content_redacted', 'privacy_clear_in_progress'].includes(notes.error.code))
  useEffect(() => {
    if (!canRead || denied) {
      drafts.deletePrefix(JSON.stringify([...prefix, spaceId, 'studio-draft']).slice(0, -1) + ',')
      drafts.deletePrefix(
        JSON.stringify([...prefix, spaceId, 'studio-generation']).slice(0, -1) + ',',
      )
      drafts.deletePrefix(
        JSON.stringify([...prefix, spaceId, 'studio-quiz-start']).slice(0, -1) + ',',
      )
      drafts.deletePrefix(
        JSON.stringify([...prefix, spaceId, 'studio-exercise']).slice(0, -1) + ',',
      )
      setSelected(undefined)
      back()
    }
  }, [canRead, denied, drafts, prefix, spaceId])
  if (!canRead || denied) return <p>当前设备无法读取 Studio 笔记，已隐藏正文并清除本区笔记草稿。</p>
  if (document)
    return (
      <StudioDocument
        key={document}
        spaceId={spaceId}
        artifactId={document}
        canWrite={canGenerate}
        onBack={back}
        onCitation={onCitation}
        onRegenerate={(base) => {
          setDocument(undefined)
          setGeneration({ kind: base.kind as 'guide' | 'brief' | 'quiz', base })
        }}
      />
    )
  if (generation)
    return canGenerate ? (
      <StudioGenerator
        key={generation.base?.artifact_id ?? generation.kind}
        space={space}
        {...generation}
        onBack={back}
        onOpen={openDocument}
      />
    ) : (
      <>
        <Button onClick={back}>返回 Studio</Button>
        <p>生成需要学习写入、来源读取、模型能力及活动学习区。</p>
      </>
    )
  if (runId)
    return (
      <>
        <Button onClick={back}>返回 Studio</Button>
        <StudioRun spaceId={spaceId} runId={runId} onOpen={openDocument} />
      </>
    )
  if (selected)
    return (
      <StudioNoteEditor
        key={JSON.stringify([...prefix, spaceId, selected])}
        spaceId={spaceId}
        artifactId={selected === 'new' ? undefined : selected}
        archived={archived}
        onBack={() => setSelected(undefined)}
        onSaved={(note) => {
          setSelected(note.artifact_id)
          void queryClient.invalidateQueries({ queryKey: [...prefix, spaceId, 'studio-list'] })
        }}
      />
    )
  return (
    <div className="notebook-studio-home">
      <div className="notebook-studio-tiles">
        {tiles.map(({ name, kind, Icon, color, description }) => (
          <button
            type="button"
            className={`notebook-studio-tile notebook-color-${color}`}
            key={name}
            disabled={!canGenerate}
            onClick={() => setGeneration({ kind: kind as 'guide' | 'brief' | 'quiz' })}
          >
            <Icon size={22} aria-hidden="true" />
            <h3>{name}</h3>
            <span>{description}</span>
          </button>
        ))}
        <button
          type="button"
          className="notebook-studio-tile notebook-color-3"
          disabled={!canWrite}
          onClick={() => setSelected('new')}
        >
          <StickyNote size={22} aria-hidden="true" />
          <h3>笔记</h3>
          <span>留下自己的想法</span>
        </button>
      </div>
      {!canGenerate && (
        <p className="hint">{archived ? '已归档，只读。' : '生成暂不可用，请查看设置。'}</p>
      )}
      <div className="studio-section-head">
        <h3>已生成</h3>
        {session.capabilities.runs && (
          <details className="studio-history">
            <summary aria-label="生成记录">
              <History size={13} aria-hidden="true" />
            </summary>
            <StudioRuns spaceId={spaceId} onOpen={setRunId} />
          </details>
        )}
      </div>
      <div className="studio-notes-list">
        {notes.isPending && <p role="status">正在读取笔记列表…</p>}
        {notes.error ? (
          <ErrorState error={notes.error} retry={() => void notes.refetch()} />
        ) : (
          <>
            {notes.data?.pages
              .flatMap((page) => page.items)
              .map((item) => {
                const Icon = {
                  note: StickyNote,
                  guide: BookOpen,
                  brief: Presentation,
                  quiz: ClipboardList,
                }[item.kind]
                return (
                  <Button
                    key={item.artifact_id}
                    className="studio-note-row"
                    variant="outline"
                    onClick={() =>
                      item.kind === 'note'
                        ? setSelected(item.artifact_id)
                        : openDocument(item.artifact_id)
                    }
                  >
                    <span className={`studio-output-icon ${item.kind}`}>
                      <Icon size={16} aria-hidden="true" />
                    </span>
                    <span className="studio-output-info">
                      <span className="studio-output-title">{item.title}</span>
                      <small>
                        {studioNames[item.kind]} · 第 {item.version} 版
                        {drafts.get(
                          JSON.stringify([...prefix, spaceId, 'studio-draft', item.artifact_id]),
                        )
                          ? ' · 本地草稿'
                          : ''}
                      </small>
                    </span>
                    <ArrowRight size={13} aria-hidden="true" />
                  </Button>
                )
              })}
            {notes.data?.pages[0]?.items.length === 0 && <p className="hint">还没有内容。</p>}
            {notes.hasNextPage && (
              <Button
                variant="outline"
                disabled={notes.isFetchingNextPage}
                onClick={() => void notes.fetchNextPage()}
              >
                加载更多笔记
              </Button>
            )}
          </>
        )}
      </div>
      <Button
        className="notebook-add-note"
        variant="outline"
        disabled={!canWrite}
        onClick={() => setSelected('new')}
      >
        ＋ 添加笔记
      </Button>
    </div>
  )
}
