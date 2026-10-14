import { describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { renderToStaticMarkup } from 'react-dom/server'
import { ContentBlocks, MathBlock, SafeMarkdown, safeLink, safeMath } from './content-blocks'

describe('安全学习正文', () => {
  it('GFM 表格、删除线、自动链接与只读任务列表使用安全渲染', () => {
    const result = renderToStaticMarkup(
      <SafeMarkdown
        text={[
          '| 概念 | 说明 |',
          '| :--- | ---: |',
          '| 偶数 | ~~旧说法~~ **可被 2 整除** |',
          '',
          'https://example.org/doc',
          '',
          '- [x] 已阅读',
          '- [ ] 待复习',
        ].join('\n')}
      />,
    )
    expect(result).toContain('tabindex="0" aria-label="表格，可横向滚动"')
    expect(result).toContain('<table>')
    expect(result).toContain('<thead>')
    expect(result).toContain('text-align:right')
    expect(result).toContain('<del>旧说法</del>')
    expect(result).toContain('href="https://example.org/doc"')
    const tasks = result.match(/<input[^>]*>/g) ?? []
    expect(tasks).toHaveLength(2)
    expect(
      tasks.every((task) => task.includes('disabled=""') && task.includes('type="checkbox"')),
    ).toBe(true)
    expect(tasks[0]).toContain('checked=""')
    expect(tasks[0]).toContain('aria-label="已完成任务"')
    expect(tasks[1]).not.toContain('checked=""')
    expect(tasks[1]).toContain('aria-label="未完成任务"')
  })
  it('GFM 单元格不渲染 HTML 或危险链接，自动链接沿用 HTTPS 限制', () => {
    const result = renderToStaticMarkup(
      <SafeMarkdown
        text={
          '| 内容 |\n| --- |\n| <img src=x onerror=alert(1)> [危险](javascript:alert%281%29) |\n\nhttp://example.org/unsafe https://user:secret@example.org/ contact@example.org'
        }
      />,
    )
    expect(result).toContain('<table>')
    expect(result).not.toMatch(
      /<img|onerror=|href="(?:javascript:|http:|mailto:|https:\/\/user:secret@)/,
    )
    expect(result).toContain('链接已禁用')
  })
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
  it('长公式可键盘聚焦并保留 MathML，拒绝解析的公式仍可滚动阅读', () => {
    for (const displayMode of [false, true]) {
      const result = renderToStaticMarkup(
        <MathBlock text={Array(24).fill('x_{123}').join('+')} displayMode={displayMode} />,
      )
      expect(result).toContain('tabindex="0"')
      expect(result).toContain('<math')
    }
    const fallback = renderToStaticMarkup(<MathBlock text={'x'.repeat(4097)} />)
    expect(fallback).toContain('公式文本（不支持或超过安全限制）')
    expect(fallback).toContain('tabindex="0"')
    expect(fallback).not.toContain('<math')
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
