import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Moon, Sun, X } from 'lucide-react'
import { useIdentity } from '@/lib/session'
import { learningClient, unwrap } from '@/api/client'
import { pageOf, spaceSchema } from '@/api/runtime'
import { progressSearch } from '@/api/progress'
import { notesyncCollection } from '@/api/notesync'
import { Button } from '../ui/button'
import { ErrorState } from '../common'

type Destination = 'feedback' | 'chat' | 'knowledge' | 'notesync' | 'runs' | 'studio' | 'manage'
const destinations: { id: Destination; label: string }[] = [
  { id: 'feedback', label: '评估与证据' },
  { id: 'chat', label: '导师历史' },
  { id: 'runs', label: '任务中心' },
  { id: 'knowledge', label: '知识与参考' },
  { id: 'notesync', label: 'NoteSync' },
  { id: 'studio', label: 'Studio 内容库' },
  { id: 'manage', label: '管理学习区与目标' },
]

function spaceLink(destination: Destination, spaceId: string, label: string): ReactNode {
  const params = { spaceId }
  switch (destination) {
    case 'feedback':
      return (
        <Link to="/spaces/$spaceId/feedback" params={params}>
          {label}
        </Link>
      )
    case 'chat':
      return (
        <Link to="/spaces/$spaceId/chat" params={params}>
          {label}
        </Link>
      )
    case 'knowledge':
      return (
        <Link
          to="/spaces/$spaceId/knowledge"
          params={params}
          search={{ goal: undefined, session: undefined }}
        >
          {label}
        </Link>
      )
    case 'notesync':
      return (
        <Link
          to="/spaces/$spaceId/notesync"
          params={params}
          search={{ collection: notesyncCollection }}
        >
          {label}
        </Link>
      )
    case 'runs':
      return (
        <Link to="/runs" search={{ space: spaceId }}>
          {label}
        </Link>
      )
    case 'studio':
      return (
        <Link to="/spaces/$spaceId/studio" params={params}>
          {label}
        </Link>
      )
    case 'manage':
      return (
        <Link to="/spaces/$spaceId/manage" params={params}>
          {label}
        </Link>
      )
  }
}

export function SpaceSwitcher({
  spaceId,
  destination,
}: {
  spaceId?: string
  destination?: Destination
}) {
  const { session, prefix } = useIdentity()
  const spaces = useQuery({
    queryKey: [...prefix, 'spaces', 'switcher'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces', {
          params: { query: { status: 'active', limit: 100 } },
          signal,
        }),
        pageOf(spaceSchema),
      ),
    enabled: session.capabilities.spaces,
  })
  return (
    <details className="space-switcher" open={!!destination}>
      <summary>{destination ? '请选择学习区' : '切换学习区'}</summary>
      <div className="switcher-menu">
        {spaces.isPending && session.capabilities.spaces && <p role="status">正在读取学习区…</p>}
        {spaces.error && <ErrorState error={spaces.error} retry={() => void spaces.refetch()} />}
        {!session.capabilities.spaces && <p>当前服务尚不支持学习区。</p>}
        {spaces.data?.items.map((space) => (
          <div key={space.id}>
            {destination ? (
              spaceLink(destination, space.id, space.name)
            ) : (
              <Link
                to="/spaces/$spaceId"
                params={{ spaceId: space.id }}
                aria-current={spaceId === space.id ? 'page' : undefined}
              >
                {space.name}
              </Link>
            )}
          </div>
        ))}
        <Link to="/">查看与管理全部学习区</Link>
      </div>
    </details>
  )
}

export function NavigationDrawer({
  spaceId,
  theme,
  onThemeChange,
  onClose,
}: {
  spaceId?: string
  theme: string
  onThemeChange: () => void
  onClose: () => void
}) {
  const { logout, drafts } = useIdentity()
  const dialog = useRef<HTMLDialogElement>(null)
  const cancel = useRef<HTMLButtonElement>(null)
  const [destination, setDestination] = useState<Destination>()
  const [confirmLogout, setConfirmLogout] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  useEffect(() => {
    const element = dialog.current!
    element.showModal()
    return () => element.close()
  }, [])
  useEffect(() => {
    if (confirmLogout) cancel.current?.focus()
  }, [confirmLogout])
  return (
    <dialog
      ref={dialog}
      className="notebook-drawer"
      aria-label="侧边菜单"
      onClose={(event) => {
        if (!event.currentTarget.open) onClose()
      }}
      onCancel={(event) => {
        if (confirmLogout) {
          event.preventDefault()
          setConfirmLogout(false)
        }
      }}
      onClick={(event) => {
        if (event.target === dialog.current) {
          const bounds = dialog.current.getBoundingClientRect()
          if (
            event.clientX < bounds.left ||
            event.clientX > bounds.right ||
            event.clientY < bounds.top ||
            event.clientY > bounds.bottom
          )
            onClose()
        }
        if ((event.target as HTMLElement).closest('a')) onClose()
      }}
    >
      <div className="notebook-drawer-heading">
        <h2>学习菜单</h2>
        <Button variant="ghost" aria-label="关闭侧边菜单" onClick={onClose}>
          <X size={20} aria-hidden="true" />
        </Button>
      </div>
      {confirmLogout ? (
        <section
          role="alertdialog"
          aria-labelledby="notebook-logout-title"
          aria-describedby="notebook-logout-description"
        >
          <h3 id="notebook-logout-title">退出浏览器学习会话？</h3>
          <p id="notebook-logout-description">
            {drafts.dirty
              ? '未保存草稿会丢失。退出不会撤销设备。'
              : '已保存的目标会保留。退出不会撤销设备。'}
          </p>
          <div className="actions">
            <Button
              ref={cancel}
              variant="outline"
              disabled={busy}
              onClick={() => setConfirmLogout(false)}
            >
              取消
            </Button>
            <Button
              variant="destructive"
              disabled={busy}
              onClick={async () => {
                setBusy(true)
                try {
                  await logout()
                } catch (e) {
                  setError(e)
                  setBusy(false)
                }
              }}
            >
              确认退出
            </Button>
          </div>
          {!!error && <ErrorState error={error} />}
        </section>
      ) : (
        <>
          <nav aria-label="全局导航" className="notebook-drawer-links">
            <Link to="/">全部学习区</Link>
            <Link to="/progress" search={progressSearch.parse(spaceId ? { space: spaceId } : {})}>
              进度与复习
            </Link>
            {destinations.map(({ id, label }) => (
              <div key={id}>
                {spaceId ? (
                  spaceLink(id, spaceId, label)
                ) : (
                  <Button variant="ghost" onClick={() => setDestination(id)}>
                    {label}
                  </Button>
                )}
              </div>
            ))}
            {destination && (
              <p className="hint">
                选择“{destinations.find((item) => item.id === destination)?.label}”所属的学习区。
              </p>
            )}
            <SpaceSwitcher spaceId={spaceId} destination={destination} />
            <Link to="/memory">长期记忆</Link>
            <a href="/app/offline">离线学习</a>
            <Link to="/settings">设置与能力</Link>
            <Link to="/settings/data">数据与隐私</Link>
            <Link to="/settings/devices">设备管理</Link>
          </nav>
          <p className="hint notebook-notesync-hint">
            NoteSync 沿用默认区／默认集合映射；其他区可查看当前支持状态。
          </p>
          <div className="notebook-drawer-actions">
            <Button
              variant="ghost"
              onClick={onThemeChange}
              aria-label={theme === 'dark' ? '切换浅色主题' : '切换深色主题'}
            >
              {theme === 'dark' ? (
                <Sun size={20} aria-hidden="true" />
              ) : (
                <Moon size={20} aria-hidden="true" />
              )}
              <span>{theme === 'dark' ? '浅色主题' : '深色主题'}</span>
            </Button>
            <Button variant="outline" onClick={() => setConfirmLogout(true)}>
              退出
            </Button>
          </div>
        </>
      )}
    </dialog>
  )
}
