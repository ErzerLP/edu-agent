import { useEffect, useState, type ReactNode } from 'react'
import { FileText, PanelLeftClose, PanelRightClose, Sparkles } from 'lucide-react'
import { Button } from '../ui/button'
import { MobilePanelTabs } from './mobile-panel-tabs'
import { useSourceSelection } from './source-selection'

export type NotebookPanel = 'sources' | 'classroom' | 'studio'

export function NotebookLayout({
  panel,
  onPanelChange,
  sources,
  classroom,
  studio,
  sourceFocus,
  legacyClassroom = false,
}: {
  panel: NotebookPanel
  onPanelChange: (panel: NotebookPanel) => void
  sources: ReactNode
  classroom: ReactNode
  studio: ReactNode
  sourceFocus?: { id: string }
  legacyClassroom?: boolean
}) {
  const { items } = useSourceSelection()
  const [narrow, setNarrow] = useState(() => matchMedia('(max-width: 900px)').matches)
  const [collapsed, setCollapsed] = useState({ sources: false, studio: false })
  useEffect(() => {
    if (sourceFocus) setCollapsed((value) => ({ ...value, sources: false }))
  }, [sourceFocus])
  useEffect(() => {
    const media = matchMedia('(max-width: 900px)')
    const resize = () => setNarrow(media.matches)
    media.addEventListener('change', resize)
    resize()
    return () => media.removeEventListener('change', resize)
  }, [])
  const columns = [
    { id: 'sources', title: '来源', content: sources },
    { id: 'classroom', title: legacyClassroom ? '课堂' : '对话', content: classroom },
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
              {id === 'sources' ? (
                <FileText size={18} aria-hidden="true" />
              ) : id === 'studio' ? (
                <Sparkles size={18} aria-hidden="true" />
              ) : null}
              <h2>{title}</h2>
              {id === 'sources' && <span className="notebook-count">{items.length}</span>}
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
      <MobilePanelTabs panel={panel} onChange={onPanelChange} legacyClassroom={legacyClassroom} />
    </>
  )
}
