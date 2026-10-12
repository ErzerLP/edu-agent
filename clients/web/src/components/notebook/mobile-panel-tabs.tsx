import { useRef } from 'react'
import { BookOpen, Library, Sparkles } from 'lucide-react'
import type { NotebookPanel } from './notebook-layout'

const panels = [
  { id: 'sources', label: '来源', Icon: Library },
  { id: 'classroom', label: '课堂', Icon: BookOpen },
  { id: 'studio', label: 'Studio', Icon: Sparkles },
] as const

export function MobilePanelTabs({
  panel,
  onChange,
}: {
  panel: NotebookPanel
  onChange: (panel: NotebookPanel) => void
}) {
  const tabs = useRef<(HTMLButtonElement | null)[]>([])
  return (
    <nav className="notebook-mobile-tabs" role="tablist" aria-label="学习区面板">
      {panels.map(({ id, label, Icon }, index) => (
        <button
          key={id}
          id={`notebook-tab-${id}`}
          type="button"
          role="tab"
          ref={(element) => {
            tabs.current[index] = element
          }}
          aria-selected={panel === id}
          aria-controls={`notebook-body-${id}`}
          tabIndex={panel === id ? 0 : -1}
          onClick={() => onChange(id)}
          onKeyDown={(event) => {
            const next =
              event.key === 'ArrowRight'
                ? (index + 1) % 3
                : event.key === 'ArrowLeft'
                  ? (index + 2) % 3
                  : event.key === 'Home'
                    ? 0
                    : event.key === 'End'
                      ? 2
                      : undefined
            if (next !== undefined) {
              event.preventDefault()
              onChange(panels[next].id)
              tabs.current[next]?.focus()
            }
          }}
        >
          <Icon size={20} aria-hidden="true" />
          <span>{label}</span>
        </button>
      ))}
    </nav>
  )
}
