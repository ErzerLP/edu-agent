import { afterAll, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { discoveryRequest, discoveryResultSchema } from './source-discovery'
afterAll(() => vi.unstubAllGlobals())

it('仅构造用户确认的公开关键词和域策略，拒绝未确认和超预算输入', () => {
  expect(discoveryRequest('public algebra', 'example.org, docs.example.org', true)).toEqual({
    keywords: 'public algebra',
    external_consent: true,
    policy: { mode: 'restrict', domains: ['example.org', 'docs.example.org'] },
  })
  expect(() => discoveryRequest('public algebra', '', false)).toThrow()
  expect(() => discoveryRequest('界'.repeat(101), '', true)).toThrow()
  expect(() => discoveryRequest('public', 'https://example.org/path', true)).toThrow()
})
it('候选摘要不能充当正文、回执或已解析来源，响应最多四项', () => {
  const item = {
    title: 'Public',
    url: 'https://example.org/',
    summary: '摘要',
    status: 'summary_only',
  }
  const response = { items: [item], request_budget: 1, requests_used: 1, cost_estimate: 'unknown' }
  expect(discoveryResultSchema.parse(response).items[0]).toEqual(item)
  expect(() =>
    discoveryResultSchema.parse({
      ...response,
      items: [{ ...item, status: 'parsed', text: '摘要' }],
    }),
  ).toThrow()
  expect(() => discoveryResultSchema.parse({ ...response, items: Array(5).fill(item) })).toThrow()
  expect(() =>
    discoveryResultSchema.parse({ ...response, items: [{ ...item, source_receipt: 'forged' }] }),
  ).toThrow()
})
