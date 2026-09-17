import { describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { renderToStaticMarkup } from 'react-dom/server'
import { ContentBlocks, SafeMarkdown, safeLink, safeMath } from './content-blocks'

describe('安全学习正文', () => {
  it('不执行原始 HTML，不自动请求图片，不允许危险链接', () => {
    const result = renderToStaticMarkup(
      <SafeMarkdown
        text={
          '<script>alert(1)</script>\n\n![私人图片](https://tracker.example/pixel)\n\n[危险](javascript:alert%281%29)\n\n[资料](https://example.org/doc)'
        }
      />,
    )
    expect(result).not.toContain('<script')
    expect(result).not.toContain('<img')
    expect(result).not.toContain('javascript:')
    expect(result).toContain('rel="noopener noreferrer"')
    for (const value of [
      'data:text/html,x',
      '//example.org',
      'http://example.org',
      'https://user:secret@example.org',
      '/v1/actions',
    ])
      expect(safeLink(value)).toBe('')
  })
  it('公式限制展开、宏、资源及尺寸，失败可读', () => {
    expect(safeMath('a^2+b^2=c^2')).toContain('<math')
    for (const value of [
      '\\includegraphics{https://example.org/a}',
      '\\href{https://example.org}{x}',
      '\\def\\a{\\a}\\a',
      'x'.repeat(4097),
    ])
      expect(safeMath(value)).toBeNull()
    const bounded = safeMath('\\rule{100000em}{100000em}')
    expect(bounded).toContain('width="10em" height="10em"')
    expect(bounded).not.toMatch(/(?:width|height)="100000em"/)
  })
  it('未知块保留语义文本；输入块本身不产生提交控件', () => {
    const result = renderToStaticMarkup(
      <ContentBlocks
        references={[]}
        blocks={[
          {
            block_id: '11111111-1111-4111-8111-111111111111',
            kind: 'unknown_simulation',
            fallback: '完整题意：比较两个数的大小。',
          },
          {
            block_id: '22222222-2222-4222-8222-222222222222',
            kind: 'answer_input',
            fallback: '正式答案请单独提交。',
          },
        ]}
      />,
    )
    expect(result).toContain('完整题意：比较两个数的大小。')
    expect(result).not.toContain('<form')
    expect(result).not.toContain('<input')
  })
})
