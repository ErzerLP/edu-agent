import { expect, it } from 'vitest'
import { webProgressSchema } from './web-search'

it('读取进度保留失败类别、详细原因及资源身份', () => {
  const failure = { resource_id: 'a167f5cd-0c4d-4db7-afef-4218a571eff5', title: 'RFC PDF', category: 'http_404', reason: 'http_404' }
  const result = webProgressSchema.parse({ stage: 'completed', read_attempted: 1, read_succeeded: 0, read_failed: 1, fetch_http_requests: 1, failures: [failure] })
  expect(result).toHaveProperty('failures', [failure])
})
