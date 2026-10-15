import type { ReactNode } from 'react'

// A persisted request is also present during an ordinary submission. Only an
// unresolved request after the in-flight check needs recovery actions.
export function SubmissionNotice({ pending, inFlight, children }: {
  pending: boolean
  inFlight: boolean
  children: ReactNode
}) {
  if (!pending || inFlight) return null
  return <div className="submission-notice" role="status">{children}</div>
}
