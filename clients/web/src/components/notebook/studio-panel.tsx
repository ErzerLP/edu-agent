import { Link } from '@tanstack/react-router'
import { BookOpen, ClipboardList, Presentation, StickyNote } from 'lucide-react'
import { useEffect, useState } from 'react'
import { ApiError } from '@/api/client'
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { studioAPI } from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { StudioNoteEditor } from './studio-note-editor'

const tiles = [
  { name: '学习指南', Icon: BookOpen, color: 1 },
  { name: '测验', Icon: ClipboardList, color: 0 },
  { name: '简报', Icon: Presentation, color: 2 },
]

export function StudioPanel({
  spaceId,
  archived = false,
}: {
  spaceId: string
  archived?: boolean
}) {
  const { session, prefix, drafts } = useIdentity()
  const queryClient = useQueryClient()
  const [selected, setSelected] = useState<string>()
  const canRead = session.device.scopes.includes('learning:read')
  const canWrite = session.device.scopes.includes('learning:write') && !archived
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
  const denied = notes.error instanceof ApiError && ([401, 403, 404].includes(notes.error.status) || ['content_redacted', 'privacy_clear_in_progress'].includes(notes.error.code))
  useEffect(() => {
    if (!canRead || denied) {
      drafts.deletePrefix(JSON.stringify([...prefix, spaceId, 'studio-draft']).slice(0, -1) + ',')
      setSelected(undefined)
    }
  }, [canRead, denied, drafts, prefix, spaceId])
  if (!canRead || denied) return <p>当前设备无法读取 Studio 笔记，已隐藏正文并清除本区笔记草稿。</p>
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
        {tiles.map(({ name, Icon, color }) => (
          <div className={`notebook-studio-tile notebook-color-${color}`} key={name}>
            <Icon size={22} aria-hidden="true" />
            <h3>{name}</h3>
            <span>尚未交付</span>
          </div>
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
      <div className="studio-notes-list">
        <h3>已保存笔记</h3>
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
                  onClick={() => setSelected(item.artifact_id)}
                >
                  <StickyNote size={18} aria-hidden="true" />
                  <span>
                    {item.title}
                    <small>
                      笔记 · 已保存第 {item.version} 版
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
