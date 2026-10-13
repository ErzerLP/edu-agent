import { createPortal } from 'react-dom'
import type { Space } from '@/api/runtime'
import { SpaceDialog } from './space-dialog'
import { useNotebookChrome } from './notebook-chrome'

export function NotebookHeader({ space }: { space: Space }) {
  const { header } = useNotebookChrome()
  return (
    header &&
    createPortal(
      <div className="notebook-workspace-header">
        <h1>{space.name}</h1>
        {space.status === 'archived' && <span className="badge">已归档 · 只读</span>}
        <SpaceDialog space={space} />
      </div>,
      header,
    )
  )
}
