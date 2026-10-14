import type { ReactNode } from 'react'
import { FilePenLine } from 'lucide-react'
import { exerciseHelpLabels, type ExerciseHelp } from '@/api/exercises'
import { Button } from '../ui/button'

// Controlled presentation shared with Studio. The caller owns identity, draft
// isolation, receipts and assessment; this component never creates a session.
export type PracticeCardProps = {
  prompt: string
  answer: string
  help: ExerciseHelp | ''
  helpOptions: ExerciseHelp[]
  submitted: boolean
  deferred: boolean
  disabled: boolean
  hint?: string
  onAnswer: (answer: string) => void
  onHelpChange: (help: ExerciseHelp) => void
  onSubmit: () => void
  onHint?: () => void
  onDefer?: () => void
  onResume?: () => void
  children?: ReactNode
}
export function PracticeCard(p: PracticeCardProps) {
  return (
    <article className="chat-practice-card" aria-label="对话练习" data-pending={!p.submitted && !p.deferred}>
      <div className="chat-practice-top">
        <span>
          <FilePenLine size={15} /> 试着想一想
        </span>
        <small>开放题</small>
      </div>
      <h3>{p.prompt}</h3>
      {p.deferred && !p.submitted ? (
        <div className="chat-practice-footer">
          <span>稍后再做</span>
          <Button disabled={p.disabled || !p.onResume} onClick={p.onResume}>
            继续这道题
          </Button>
        </div>
      ) : (
        <>
          <textarea
            aria-label="练习答案"
            placeholder="写下你的思路，不用急着答对…"
            value={p.answer}
            readOnly={p.submitted}
            disabled={p.disabled}
            maxLength={16000}
            onChange={(e) => p.onAnswer(e.target.value)}
          />
          <div className="chat-practice-footer">
            {p.submitted ? (
              <span>已提交 · {p.help ? exerciseHelpLabels[p.help] : ''}</span>
            ) : (
              <>
                <label>
                  我得到的帮助
                  <select
                    aria-label="我得到的帮助"
                    value={p.help}
                    disabled={p.disabled}
                    onChange={(e) => p.onHelpChange(e.target.value as ExerciseHelp)}
                  >
                    <option value="">请选择</option>
                    {p.helpOptions.map((h) => (
                      <option key={h} value={h}>
                        {exerciseHelpLabels[h]}
                      </option>
                    ))}
                  </select>
                </label>
                <Button disabled={p.disabled || !p.answer.trim() || !p.help} onClick={p.onSubmit}>
                  提交答案
                </Button>
              </>
            )}
          </div>
          {!p.submitted && (
            <div className="chat-practice-actions">
              <Button variant="ghost" disabled={p.disabled || !p.onHint} onClick={p.onHint}>
                给我一个提示
              </Button>
              {p.onDefer && (
                <Button variant="ghost" disabled={p.disabled} onClick={p.onDefer}>
                  稍后再做
                </Button>
              )}
            </div>
          )}
          {!!p.hint && <p className="chat-practice-hint">{p.hint}</p>}
        </>
      )}
      {p.children}
    </article>
  )
}
