import { expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { renderToStaticMarkup } from 'react-dom/server'
import { PracticeCard } from './practice-card'

it('共享题卡要求答案与实际帮助，收起时不能提交，也不显示完成', () => {
  const props = {
    prompt: '解释偶数',
    answer: '',
    help: '' as const,
    helpOptions: ['hint' as const],
    submitted: false,
    deferred: false,
    disabled: false,
    onAnswer: () => {},
    onHelpChange: () => {},
    onSubmit: () => {},
    onResume: () => {},
  }
  const empty = renderToStaticMarkup(<PracticeCard {...props} />)
  expect(empty).toMatch(/disabled=""[^>]*>提交答案/)
  expect(empty).not.toContain('value="none"')
  const deferred = renderToStaticMarkup(<PracticeCard {...props} deferred />)
  expect(deferred).toContain('继续这道题')
  expect(deferred).not.toContain('提交答案')
  expect(deferred).not.toContain('已完成')
})
