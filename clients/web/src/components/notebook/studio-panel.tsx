import { Link } from '@tanstack/react-router'
import { BookOpen, ClipboardList, Presentation, StickyNote } from 'lucide-react'

const tiles = [
  { name: '学习指南', Icon: BookOpen, color: 1 },
  { name: '测验', Icon: ClipboardList, color: 0 },
  { name: '简报', Icon: Presentation, color: 2 },
  { name: '笔记', Icon: StickyNote, color: 3 },
]

export function StudioPanel({ spaceId }: { spaceId: string }) {
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
