import { expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { PracticeExercise } from '@/api/exercises'
import { PracticeController } from './practice-controller'

vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
vi.mock('@/lib/session', async () => {
  const { DraftStore } = await import('@/lib/drafts')
  return {
    useIdentity: () => ({
      session: { device: { scopes: ['learning:write'] } },
      prefix: ['assessment-test'],
      drafts: new DraftStore(),
    }),
  }
})

const exercise: PracticeExercise = {
  id: 'exercise', space_id: 'space', privacy_generation: 1,
  session_id: 'session', activity_id: 'activity', artifact_id: 'artifact',
  content_version: 1, session_version: 3, prompt: '偶数有什么特点？',
  allowed_help: ['none'], help_version: 0, deferred: false, can_continue: false,
  attempt_id: 'attempt',
}

it.each([
  { reason: 'model_failed', result_unknown: true, can_resume: false },
  { reason: 'model_failed', result_unknown: false, can_resume: true },
  { reason: 'assessment_commit_failed', result_unknown: false, can_resume: true },
])('失败评估显示失败与可用恢复入口：$reason/$result_unknown', (failure) => {
  const client = new QueryClient()
  const html = renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <PracticeController
        exercise={{ ...exercise, assessment: {
          status: 'failed', version: 6, configuration: 'current',
          destination: 'https://model.example/v1', ...failure,
        } }}
        disabled={false}
        refresh={async () => {}}
        draftScope={['test']}
        transport={{ resumeAssessment: vi.fn() }}
      />
    </QueryClientProvider>,
  )
  expect(html).toContain('失败')
  expect(html).toContain('答案已保存')
  expect(html).not.toContain('评估结果待核对')
  expect(html).not.toContain('评估已暂停')
  if (failure.can_resume) {
    expect(html).toContain('确认并继续评估')
    expect(html).not.toMatch(/disabled=""[^>]*>确认并继续评估/)
  } else {
    expect(html).toContain('核对评估状态')
    expect(html).not.toContain('确认并继续评估')
  }
})
