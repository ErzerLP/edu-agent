import { citationParts, type MentorCitation } from '@/api/source-selection'
import { SourceDialog, MentorCitationContent } from './notebook/source-viewer'
import { Button } from './ui/button'

export function MentorAnswer({
  spaceId,
  runId,
  text,
  citations = [],
  onCitation,
}: {
  spaceId: string
  runId: string
  text: string
  citations?: MentorCitation[]
  onCitation?: (runId: string, id: string) => void
}) {
  return (
    <>
      {citationParts(
        text,
        citations.map((c) => c.id),
      ).map((part, index) =>
        part.id ? (
          onCitation ? (
            <Button
              className="citation-link"
              variant="outline"
              key={index}
              onClick={() => onCitation(runId, part.id!)}
              aria-label={`核验引用 ${part.id}`}
            >
              {part.text}
            </Button>
          ) : (
            <SourceDialog key={index} label={part.text} title={`核验引用 ${part.id}`}>
              <MentorCitationContent spaceId={spaceId} runId={runId} citationId={part.id} />
            </SourceDialog>
          )
        ) : (
          <span key={index}>{part.text}</span>
        ),
      )}
    </>
  )
}
