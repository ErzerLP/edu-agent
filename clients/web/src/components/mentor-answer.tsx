import { Fragment } from 'react'
import { citationParts, type MentorCitation } from '@/api/source-selection'
import { MathBlock } from './content-blocks'
import { SourceDialog, MentorCitationContent } from './notebook/source-viewer'
import { Button } from './ui/button'

export function MentorAnswer({
  spaceId,
  runId,
  text,
  citations = [],
  onCitation,
  renderMath = false,
}: {
  spaceId: string
  runId: string
  text: string
  citations?: MentorCitation[]
  onCitation?: (runId: string, id: string) => void
  renderMath?: boolean
}) {
  return (
    <>
      {(renderMath
        ? text.split(
            /(\\\\|\\\$|`{3}[\s\S]*?`{3}|`[^`\n]*`|\\\([\s\S]*?\\\)|\\\[[\s\S]*?\\\]|\$\$[\s\S]*?\$\$|\$(?:\\.|[^$\\\n])+?\$(?!\d))/g,
          )
        : [text]
      ).map((segment, segmentIndex) => {
        // Parse complete formulas before citation IDs so matrix indexes remain math.
        if (
          segmentIndex % 2 === 1 &&
          (segment.startsWith('\\(') || segment.startsWith('\\[') || segment.startsWith('$'))
        ) {
          const displayMode = segment.startsWith('\\[') || segment.startsWith('$$')
          const delimiterLength = segment.startsWith('$') && !displayMode ? 1 : 2
          return (
            <MathBlock
              key={segmentIndex}
              text={segment.slice(delimiterLength, -delimiterLength)}
              displayMode={displayMode}
            />
          )
        }
        if (segmentIndex % 2 === 1) return <span key={segmentIndex}>{segment}</span>
        return (
          <Fragment key={segmentIndex}>
            {citationParts(
              segment,
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
          </Fragment>
        )
      })}
    </>
  )
}
