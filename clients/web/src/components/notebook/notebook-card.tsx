import { Link } from '@tanstack/react-router'
import { BookOpen } from 'lucide-react'
import type { Space } from '@/api/runtime'

export function NotebookCard({ space }: { space: Space }) {
  const color = [...space.id].reduce((value, char) => value + char.charCodeAt(0), 0) % 4
  return (
    <article className={`notebook-card notebook-color-${color}`}>
      <BookOpen aria-hidden="true" size={32} />
      <span className="notebook-card-status">
        {space.status === 'archived' ? '已归档' : '学习区'}
      </span>
      <h2>
        <Link to="/spaces/$spaceId" params={{ spaceId: space.id }}>
          {space.name}
        </Link>
      </h2>
      <p>{space.description || '为一个想探索的方向，留一片空间。'}</p>
      <small>
        学习区更新：
        <time dateTime={space.updated_at}>
          {new Date(space.updated_at).toLocaleString('zh-CN')}
        </time>
      </small>
    </article>
  )
}
