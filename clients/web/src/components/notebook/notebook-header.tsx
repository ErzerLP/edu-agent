import { Link } from '@tanstack/react-router'
import type { Space } from '@/api/runtime'
import { SpaceDialog } from './space-dialog'

export function NotebookHeader({ space }: { space: Space }) {
  return (
    <header className="notebook-workspace-header">
      <div>
        <Link to="/">全部学习区</Link>
        <h1>{space.name}</h1>
      </div>
      {space.status === 'archived' && <span className="badge">已归档 · 只读</span>}
      <SpaceDialog space={space} />
    </header>
  )
}
