import { z } from 'zod'
import { ApiError } from './client'
import { feedbackSchema } from './feedback'
import { learningSourceCitationSchema } from './source-selection'

const id = z.uuid(),
  version = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
export const helpLevel = z.enum(['none', 'hint', 'scaffold', 'answer_revealed'])
export type ExerciseHelp = z.infer<typeof helpLevel>
// Presentation only; the original quote, range and hash stay intact for lookup.
export const citationExcerpt = (text: string) =>
  text.replace(/<!--\s*edu-agent-node:v1\b[\s\S]*?-->\s*/g, '').trim()
export const practiceExerciseSchema = z
  .object({
    id,
    space_id: id,
    privacy_generation: version,
    session_id: id,
    activity_id: id,
    artifact_id: id,
    content_version: version,
    session_version: version,
    prompt: z.string(),
    allowed_help: z.array(helpLevel).min(1).max(4),
    help_version: z.number().int().nonnegative(),
    help_floor: helpLevel.optional(),
    deferred: z.boolean(),
    can_continue: z.boolean(),
    attempt_id: id.optional(),
    feedback: feedbackSchema.optional(),
    assessment: z
      .object({
        status: z.string(),
        version: z.number().int().nonnegative(),
        reason: z.string().optional(),
        result_unknown: z.boolean(),
        can_resume: z.boolean(),
        configuration: z.string(),
        destination: z.string(),
      })
      .optional(),
  })
  .superRefine((v, ctx) => {
    if (
      v.feedback &&
      (v.feedback.attempt.attempt_id !== v.attempt_id ||
        v.feedback.activity.activity_id !== v.activity_id ||
        v.feedback.activity.session_id !== v.session_id ||
        v.feedback.learning_space_id !== v.space_id)
    )
      ctx.addIssue({ code: 'custom', message: '练习反馈身份不一致' })
  })
export const exerciseSchema = practiceExerciseSchema.safeExtend({ conversation_id: id, run_id: id })
export type PracticeExercise = z.infer<typeof practiceExerciseSchema>
export type Exercise = z.infer<typeof exerciseSchema>
export const exerciseHelpSchema = z.object({ operation_id: id, level: helpLevel, text: z.string() })
export const exerciseCitationSchema = learningSourceCitationSchema.extend({ id })
export function checkExercise(
  v: Exercise,
  exercise: string,
  space: string,
  generation: number,
  conversation: string,
) {
  if (
    v.id !== exercise ||
    v.space_id !== space ||
    v.privacy_generation !== generation ||
    v.conversation_id !== conversation
  )
    throw new ApiError(502, 'invalid_response')
  return v
}
const levels: ExerciseHelp[] = ['none', 'hint', 'scaffold', 'answer_revealed']
export function availableHelp(allowed: ExerciseHelp[], floor?: ExerciseHelp) {
  return allowed.filter((h) => levels.indexOf(h) >= levels.indexOf(floor ?? 'none'))
}
export const exerciseHelpLabels: Record<ExerciseHelp, string> = {
  none: '独立完成',
  hint: '看过提示',
  scaffold: '参考了示例',
  answer_revealed: '看过答案',
}
export function feedbackHeading(disposition: string, conclusions: string[]) {
  if (disposition === 'provisional') return '待复核'
  if (disposition === 'voided') return '本次反馈已撤回'
  if (!['accepted', 'overridden'].includes(disposition)) return '正在评估'
  return conclusions.length > 0 && conclusions.every((c) => c === 'pass')
    ? '你找到了关键点'
    : '再补充一点理由'
}
