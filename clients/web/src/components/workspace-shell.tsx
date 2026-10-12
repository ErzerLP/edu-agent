import { Link, Outlet, useParams, useSearch } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { BookOpen, Menu, Monitor } from 'lucide-react'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { NavigationDrawer } from './notebook/navigation-drawer'
import { z } from 'zod'

export function WorkspaceShell() {
  const { spaceId } = useParams({ strict: false })
  const search = useSearch({ strict: false })
  const { session } = useIdentity()
  const [menuOpen, setMenuOpen] = useState(false)
  const selectedSpace = z.uuid().safeParse(spaceId ?? search.space)
  const currentSpace = selectedSpace.success ? selectedSpace.data : undefined
  const [theme, setTheme] = useState(() => {
    try {
      return localStorage.getItem('knowledge-mesh-theme') ?? 'light'
    } catch {
      return 'light'
    }
  })
  useEffect(() => {
    document.documentElement.dataset.theme = theme
    try {
      localStorage.setItem('knowledge-mesh-theme', theme)
    } catch {
      /* 无法保存偏好时仍可使用当前主题。 */
    }
  }, [theme])
  return (
    <>
      <a className="skip-link" href="#main">
        跳到主内容
      </a>
      <header className="site-header notebook-site-header">
        <Button
          variant="ghost"
          aria-label="打开侧边菜单"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen(true)}
        >
          <Menu aria-hidden="true" size={24} />
        </Button>
        <Link to="/" className="brand">
          <BookOpen aria-hidden="true" size={24} />
          <span>
            知络<small>Knowledge Mesh</small>
          </span>
        </Link>
        <Link className="notebook-device" to="/settings/devices" aria-label="当前配对设备">
          <Monitor aria-hidden="true" size={18} />
          <span>{session.device.display_name}</span>
        </Link>
      </header>
      {menuOpen && (
        <NavigationDrawer
          spaceId={currentSpace}
          theme={theme}
          onThemeChange={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
          onClose={() => setMenuOpen(false)}
        />
      )}
      <main id="main" className="main" tabIndex={-1}>
        <Outlet />
      </main>
      <footer>从目标出发 · 按你的节奏学习</footer>
    </>
  )
}
