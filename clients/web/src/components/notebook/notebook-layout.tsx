import { useEffect, useState, type ReactNode } from 'react'
import { PanelLeftClose, PanelRightClose } from 'lucide-react'
import { Button } from '../ui/button'
import { MobilePanelTabs } from './mobile-panel-tabs'

export type NotebookPanel = 'sources' | 'classroom' | 'studio'

export function NotebookLayout({
  panel,
  onPanelChange,
  sources,
  classroom,
  studio,
  sourceFocus,
}: {
  panel: NotebookPanel
  onPanelChange: (panel: NotebookPanel) => void
  sources: ReactNode
  classroom: ReactNode
  studio: ReactNode
  sourceFocus?: { id: string }
}) {
  const [narrow, setNarrow] = useState(() => matchMedia('(max-width: 1099px)').matches)
  const [collapsed, setCollapsed] = useState({ sources: false, studio: false })
  useEffect(() => {
    if (sourceFocus) setCollapsed((value) => ({ ...value, sources: false }))
  }, [sourceFocus])
  useEffect(() => {
    const media = matchMedia('(max-width: 1099px)')
    const resize = () => setNarrow(media.matches)
    media.addEventListener('change', resize)
    resize()
    return () => media.removeEventListener('change', resize)
  }, [])
  const columns = [
    { id: 'sources', title: '来源', content: sources },
    { id: 'classroom', title: '课堂', content: classroom },
    { id: 'studio', title: 'Studio', content: studio },
  ] as const
  return (
    <>
      <div
        className="notebook-columns"
        data-sources-collapsed={!narrow && collapsed.sources}
        data-studio-collapsed={!narrow && collapsed.studio}
      >
        {columns.map(({ id, title, content }) => (
          <section
            key={id}
            className={`notebook-panel notebook-${id}`}
            aria-label={title}
            role="region"
            id={`notebook-panel-${id}`}
            hidden={narrow && panel !== id}
          >
            <header className="notebook-panel-header">
              <h2>{title}</h2>
              {id !== 'classroom' && (
                <Button
                  variant="ghost"
                  className="notebook-collapse"
                  aria-label={`${collapsed[id] ? '展开' : '收起'}${title}`}
                  aria-expanded={!collapsed[id]}
                  aria-controls={`notebook-body-${id}`}
                  onClick={() => setCollapsed((value) => ({ ...value, [id]: !value[id] }))}
                >
                  {id === 'sources' ? (
                    <PanelLeftClose size={18} aria-hidden="true" />
                  ) : (
                    <PanelRightClose size={18} aria-hidden="true" />
                  )}
                </Button>
              )}
            </header>
            <div
              id={`notebook-body-${id}`}
              className="notebook-panel-body"
              role={narrow ? 'tabpanel' : undefined}
              aria-labelledby={narrow ? `notebook-tab-${id}` : undefined}
              hidden={!narrow && id !== 'classroom' && collapsed[id]}
            >
              {content}
            </div>
          </section>
        ))}
      </div>
      <MobilePanelTabs panel={panel} onChange={onPanelChange} />
    </>
  )
}
