import { createContext, useContext } from 'react'

// Workspace controls keep their state in the owning panel while appearing in the shared shell.
export const NotebookChrome = createContext<{
  header: HTMLElement | null
  menu: HTMLElement | null
}>({ header: null, menu: null })

export const useNotebookChrome = () => useContext(NotebookChrome)
