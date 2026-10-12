import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { selectedSources, sourceKey, type SourceIdentity } from '@/api/source-selection'
import { useNotebookSources } from '@/lib/use-notebook-sources'

export type CitationFocus = { runId?: string; artifactId?: string; version?: number; id: string }
function useSelection(spaceId: string) {
  const catalog = useNotebookSources(spaceId)
  const [selected, setSelected] = useState<SourceIdentity[]>()
  const [citation, setCitation] = useState<CitationFocus>()
  const items = catalog.sources.data?.pages.flatMap((p) => p.items) ?? []
  const ready =
    catalog.available &&
    catalog.capabilities.data?.source_catalog === true &&
    !catalog.capabilities.error &&
    !catalog.sources.isPending &&
    !catalog.sources.error
  useEffect(() => {
    if (ready && selected === undefined) setSelected(items)
  }, [ready, selected, catalog.sources.data])
  const selection = ready ? selectedSources(selected, items) : undefined
  const toggle = (item: SourceIdentity, checked: boolean) =>
    setSelected((old) =>
      checked
        ? [...(old ?? []).filter((s) => sourceKey(s) !== sourceKey(item)), item]
        : (old ?? []).filter((s) => sourceKey(s) !== sourceKey(item)),
    )
  return {
    ...catalog,
    items,
    selection,
    toggle,
    selectAll: (checked: boolean) => setSelected(checked ? items : []),
    citation,
    setCitation,
  }
}
const SelectionContext = createContext<ReturnType<typeof useSelection> | null>(null)
export function SourceSelectionProvider({
  spaceId,
  children,
}: {
  spaceId: string
  children: ReactNode
}) {
  const value = useSelection(spaceId)
  return <SelectionContext.Provider value={value}>{children}</SelectionContext.Provider>
}
export function useSourceSelection() {
  const value = useContext(SelectionContext)
  if (!value) throw new Error('SourceSelectionProvider missing')
  return value
}
