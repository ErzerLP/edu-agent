import { Link } from '@tanstack/react-router'
import { BookOpen, ClipboardList, Presentation, StickyNote } from 'lucide-react'
import { useEffect, useState } from 'react'
import { ApiError } from '@/api/client'
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { studioAPI, type StudioRevision } from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { StudioNoteEditor } from './studio-note-editor'
import { StudioDocument } from './studio-document'
import { StudioGenerator, StudioRun, StudioRuns, studioNames } from './studio-generation'

const tiles = [
  { name: '学习指南', kind: 'guide', Icon: BookOpen, color: 1 },
  { name: '测验', kind: 'quiz', Icon: ClipboardList, color: 0 },
  { name: '简报', kind: 'brief', Icon: Presentation, color: 2 },
]

export function StudioPanel({
  spaceId,
  goalId,
  onCitation,
  archived = false,
}: {
  spaceId: string
  goalId?: string
  onCitation: (artifact: string, version: number, id: string) => void
  archived?: boolean
}) {
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
        spaceId={spaceId}
        goalId={goalId}
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
    <>
      <p className="hint">整理学习成果，留住自己的理解。</p>
      <div className="notebook-studio-tiles">
        {tiles.map(({ name, kind, Icon, color }) => (
          <button
            type="button"
            className={`notebook-studio-tile notebook-color-${color}`}
            key={name}
            disabled={!canGenerate}
            onClick={() => setGeneration({ kind: kind as 'guide' | 'brief' | 'quiz' })}
          >
            <Icon size={22} aria-hidden="true" />
            <h3>{name}</h3>
            <span>{kind === 'quiz' && !goalId ? '请先选择或建立目标' : '按所选来源生成'}</span>
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
          <span>写下自己的理解</span>
        </button>
      </div>
      {!canGenerate && (
        <p className="hint">指南／简报需要模型运行能力、来源读取与学习写入权限；归档区只读。</p>
      )}
      {session.capabilities.runs && <StudioRuns spaceId={spaceId} onOpen={setRunId} />}
      <div className="studio-notes-list">
        <h3>已保存产物</h3>
        {notes.isPending && <p role="status">正在读取笔记列表…</p>}
        {notes.error ? (
          <ErrorState error={notes.error} retry={() => void notes.refetch()} />
        ) : (
          <>
            {notes.data?.pages
              .flatMap((page) => page.items)
              .map((item) => (
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
                  <StickyNote size={18} aria-hidden="true" />
                  <span>
                    {item.title}
                    <small>
                      {studioNames[item.kind]} · 已保存第 {item.version} 版
                      {drafts.get(
                        JSON.stringify([...prefix, spaceId, 'studio-draft', item.artifact_id]),
                      )
                        ? ' · 本地草稿'
                        : ''}
                    </small>
                  </span>
                </Button>
              ))}
            {notes.data?.pages[0]?.items.length === 0 && (
              <p className="hint">还没有已保存的笔记。</p>
            )}
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
        <Button variant="outline" disabled={!canWrite} onClick={() => setSelected('new')}>
          ＋ 添加笔记
        </Button>
      </div>
      <div className="notebook-studio-library">
        <h3>课堂内容库</h3>
        <p className="hint">查看已保存的阅读与练习，继续使用收藏、筛选和版本历史。</p>
        <Link className="button button-outline" to="/spaces/$spaceId/studio" params={{ spaceId }}>
          进入原 Studio 内容库 →
        </Link>
      </div>
    </>
  )
}
