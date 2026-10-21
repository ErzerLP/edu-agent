import { expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { WebSearchRecord } from './web-search-record'
import { webProgressSchema } from '@/api/web-search'

it('只有用户网址读取失败、没有搜索词时仍显示检索记录与失败类别', () => {
  const progress = webProgressSchema.parse({ stage: 'completed', read_attempted: 1, read_succeeded: 0, read_failed: 1, fetch_http_requests: 1,
    failures: [{ resource_id: 'a167f5cd-0c4d-4db7-afef-4218a571eff5', title: '<script>RFC</script>', category: 'http_404', reason: 'http_404' }],
  })
  const html = renderToStaticMarkup(<WebSearchRecord progress={progress} />)
  expect(html).toContain('本轮检索记录')
  expect(html).toContain('HTTP 404')
  expect(html).toContain('http_404')
  expect(html).toContain('&lt;script&gt;RFC&lt;/script&gt;')
  expect(html).not.toContain('<script>')
})

it('同时显示检索词和大小失败的详细判定', () => {
  const progress = webProgressSchema.parse({ stage: 'completed', queries: ['Go release notes'], read_attempted: 1, read_succeeded: 0, read_failed: 1, fetch_http_requests: 1,
    failures: [{ resource_id: 'a167f5cd-0c4d-4db7-afef-4218a571eff5', title: '大页面', category: 'too_large', reason: 'decoded_limit' }],
  })
  const html = renderToStaticMarkup(<WebSearchRecord progress={progress} />)
  expect(html).toContain('Go release notes')
  expect(html).toContain('too_large')
  expect(html).toContain('decoded_limit')
  expect(html).toContain('大小上限')
})
