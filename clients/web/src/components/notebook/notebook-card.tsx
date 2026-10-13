import { Link } from '@tanstack/react-router'
import { ArrowRight, BookOpen } from 'lucide-react'
import type { Space } from '@/api/runtime'

export function NotebookCard({ space }: { space: Space }) {
  const color = [...space.id].reduce((value, char) => value + char.charCodeAt(0), 0) % 4
  return (
    <article
      className={`notebook-card notebook-color-${color}`}
      title={space.description || space.direction || undefined}
    >
      <span className="notebook-card-icon">
        <BookOpen aria-hidden="true" size={27} />
      </span>
      <ArrowRight className="notebook-card-arrow" size={16} aria-hidden="true" />
      {space.status === 'archived' && <span className="notebook-card-status">已归档</span>}
      <h2>
        <Link to="/spaces/$spaceId" params={{ spaceId: space.id }}>
          {space.name}
        </Link>
      </h2>
      <small>
        更新于
        <time dateTime={space.updated_at}>
          {new Date(space.updated_at).toLocaleDateString('zh-CN')}
        </time>
      </small>
    </article>
  )
}
