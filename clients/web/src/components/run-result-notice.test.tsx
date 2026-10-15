import { expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { RunResultNotice } from './run-result-notice'

const run = { status: 'cancelled', reason: 'user_stopped', result_unknown: true, requests_used: 1, cost_unknown: true } as const
it('an explicit user stop replaces the unknown-result warning with an ordinary usage note', () => {
  const html = renderToStaticMarkup(<RunResultNotice run={run}>模型请求结果未知，未自动重发。</RunResultNotice>)
  expect(html).not.toContain('结果未知')
  expect(html).not.toContain('role="alert"')
  expect(html).toContain('已发出的请求可能计入用量')
})
it.each(['lease_expired', 'context_changed', 'model_failed'])('non-user interruption %s preserves the warning', reason => {
  const html = renderToStaticMarkup(<RunResultNotice run={{ ...run, reason }}>模型请求结果未知，未自动重发。</RunResultNotice>)
  expect(html).toContain('模型请求结果未知，未自动重发。')
  expect(html).toContain('role="alert"')
})
it('a known model result needs no unknown-result notice', () => {
  expect(renderToStaticMarkup(<RunResultNotice run={{ ...run, reason: '', result_unknown: false }}>结果未知</RunResultNotice>)).toBe('')
})
it('a user stop still explains issued usage when the result is known', () => {
  const html = renderToStaticMarkup(<RunResultNotice run={{ ...run, result_unknown: false }}>结果未知</RunResultNotice>)
  expect(html).toContain('已发出的请求可能计入用量')
  expect(html).not.toContain('结果未知')
})
it('a user stop before any request needs no usage note', () => {
  expect(renderToStaticMarkup(<RunResultNotice run={{ ...run, result_unknown: false, requests_used: 0, cost_unknown: false }}>结果未知</RunResultNotice>)).toBe('')
})
