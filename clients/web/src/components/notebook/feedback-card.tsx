import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { CheckCircle2, Clock3 } from 'lucide-react'
import { citationExcerpt, feedbackHeading, type Exercise } from '@/api/exercises'
import { SourceDialog, MentorCitationContent } from './source-viewer'
import { Button } from '../ui/button'
import { SafeMarkdown } from '../content-blocks'

export function FeedbackCard({
  exercise,
  onContinue,
  disabled,
  renderCitation,
}: {
  exercise: Pick<Exercise, 'feedback' | 'space_id' | 'can_continue'> & { id?: string }
  onContinue?: () => void
  disabled?: boolean
  renderCitation?: (referenceId: string, index: number) => ReactNode
}) {
  const feedback = exercise.feedback,
    decision = feedback?.decisions.at(-1)
  if (!feedback?.assessment || !decision) return null
  const pending = decision.disposition === 'provisional',
    items = decision.items
  const title = feedbackHeading(
    decision.disposition,
    items.map((i) => i.conclusion),
  )
  const passed =
    !pending &&
    ['accepted', 'overridden'].includes(decision.disposition) &&
    items.every((i) => i.conclusion === 'pass')
  return (
    <article className={`chat-feedback-card${pending ? ' pending' : ''}`} aria-label="练习反馈">
      <h3>
        {pending ? <Clock3 size={17} /> : <CheckCircle2 size={17} />} {title}
      </h3>
      {pending && <p>这次结果需要复核，暂不算通过。</p>}
      {items.map((item, index) => (
        <div key={item.rubric_item_id}>
          <p>
            {
              feedback.activity.rubric.items.find((r) => r.rubric_item_id === item.rubric_item_id)
                ?.criterion
            }
            ：
            {
              (
                {
                  pass: '已说明',
                  partial: '还需补充',
                  fail: '需要再想一想',
                  unassessed: '待核对',
                } as const
              )[item.conclusion]
            }
          </p>
          {item.knowledge_quote && (
            <div className="chat-feedback-basis">
              <span>依据：</span>
              <SafeMarkdown text={citationExcerpt(item.knowledge_quote)} />
              {renderCitation
                ? renderCitation(item.knowledge_reference_id, index)
                : exercise.id && (
                    <SourceDialog label={`查看依据 ${index + 1}`} title="反馈依据">
                      <MentorCitationContent
                        spaceId={exercise.space_id}
                        exerciseId={exercise.id}
                        citationId={item.knowledge_reference_id}
                      />
                    </SourceDialog>
                  )}
            </div>
          )}
        </div>
      ))}
      <p className="chat-feedback-next">
        <strong>下一步</strong> ·{' '}
        {pending
          ? '核对答案和依据，再查看复核记录。'
          : passed
            ? '换一个例子，试着用自己的话说明理由。'
            : '对照资料中的定义，把缺少的理由补完整。'}
      </p>
      <Link
        to="/spaces/$spaceId/feedback/$attemptId"
        params={{ spaceId: exercise.space_id, attemptId: feedback.attempt.attempt_id }}
      >
        {pending ? '查看并复核' : '查看记录'}
      </Link>
      {exercise.can_continue && onContinue && (
        <Button variant="ghost" disabled={disabled} onClick={onContinue}>
          继续学习
        </Button>
      )}
    </article>
  )
}
