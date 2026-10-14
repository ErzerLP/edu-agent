import { expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { renderToStaticMarkup } from 'react-dom/server'
import { MentorAnswer } from './mentor-answer'
import type { MentorCitation } from '@/api/source-selection'

it('GFM 表格与删除线保留公式和引用，自动链接与代码内的标记保持原文', () => {
  const result = renderToStaticMarkup(
    <MentorAnswer
      spaceId="space"
      runId="run"
      citations={[{ id: '1' } as MentorCitation]}
      onCitation={() => {}}
      text={[
        '| 概念 | 公式与依据 |',
        '| --- | --- |',
        String.raw`| ~~旧说法 [1]~~ | $x_1^2$ [1] 和 \(a[1]+b\) |`,
        '| 代码 | `$x$ [1]` |',
        '',
        '- [x] 已阅读 $y^2$ [1]',
        '',
        'https://example.org/$x$',
        '',
        String.raw`\[x^2+y^2=1\]`,
        '',
        '```text',
        '$x$ [1]',
        '```',
      ].join('\n')}
    />,
  )
  expect(result).toContain('<table>')
  expect(result).toContain('<del>旧说法 ')
  expect(result.match(/<math /g)).toHaveLength(4)
  expect(result.match(/display="block"/g)).toHaveLength(1)
  expect(result.match(/aria-label="核验引用 1"/g)).toHaveLength(3)
  expect(result).toContain('<code>$x$ [1]</code>')
  expect(result).toContain('href="https://example.org/$x$"')
  expect(result).not.toMatch(/<a\b[^>]*>[\s\S]*?<button[\s\S]*?<\/a>/)
  expect(result).not.toMatch(/<p>\s*<div/)
})

it('对话的四种公式分隔符保留行内、块级布局与公式外引用按钮', () => {
  const result = renderToStaticMarkup(
    <MentorAnswer
      spaceId="space"
      runId="run"
      renderMath
      text={String.raw`行内 \(a_1v_1+\dots\) [1] 和 $ x^2 $。
块级 \[a[1]+b=0\]
$$\sum_{i=1}^n a_i=0$$
保留换行与未知引用 [2]。`}
      citations={[{ id: '1' } as MentorCitation]}
      onCitation={() => {}}
    />,
  )
  expect(result.match(/<math /g)).toHaveLength(4)
  expect(result.match(/display="block"/g)).toHaveLength(2)
  expect(result.match(/aria-label="核验引用 1"/g)).toHaveLength(1)
  expect(result.replace(/<[^>]+>/g, '')).toContain('保留换行与未知引用 [2]。')
  expect(result).not.toContain('\\(')
})

it('危险和无效公式保留可读文本，HTML、代码、转义美元不执行或误渲染', () => {
  const text =
    String.raw`<img src=x onerror=alert(1)> \(\href{https://evil.example}{x}\) $\invalid$ \$5，价格 $5 和 $10。` +
    '\n代码：`$x$`，未闭合：\\(x'
  const result = renderToStaticMarkup(
    <MentorAnswer spaceId="space" runId="run" text={text} renderMath />,
  )
  expect(result).not.toContain('<math')
  expect(result).not.toContain('<img')
  expect(result).not.toContain('href="https://evil.example')
  expect(result).toContain('\\href')
  expect(result).toContain('$x$')
  expect(result.replace(/<[^>]+>/g, '')).toContain('$5')
  expect(result).toContain('\\(x')
})

it('显式关闭公式时仍保留可读文本', () => {
  const result = renderToStaticMarkup(
    <MentorAnswer spaceId="space" runId="run" text={String.raw`\[x^2\]`} renderMath={false} />,
  )
  expect(result).not.toContain('<math')
  expect(result).toContain('[x^2]')
})

it('所有回答入口渲染 Markdown，强调和列表中的引用与公式共存，代码保持原文', () => {
  const result = renderToStaticMarkup(
    <MentorAnswer
      spaceId="space"
      runId="run"
      citations={[{ id: '1' } as MentorCitation]}
      onCitation={() => {}}
      text={
        String.raw`# 学习回答

段落 **加粗 [1]** 与 *斜体*，行内 \(x_1 + a[1]\)。

- 第一项 $x^2$ [1]
- 第二项

1. 有序项

> 引用块

[资料](https://example.org/doc)

\[x^2 + y^2 = 1\]

` +
        '行内代码 ``$x$ [1] `文字` ``\n\n```ts\nconst x = "$x$ [1]"\n```\n\n~~~text\n$x$ [1]\n~~~\n\n    $x$ [1]'
      }
    />,
  )
  expect(result).toContain('<h1>学习回答</h1>')
  expect(result).toContain('<strong>加粗 ')
  expect(result).toContain('<em>斜体</em>')
  expect(result).toContain('<ul>')
  expect(result).toContain('<ol>')
  expect(result).toContain('<blockquote>')
  expect(result).toContain(
    '<pre tabindex="0" aria-label="代码，可横向滚动"><code class="language-ts">',
  )
  expect(result).toContain('<code>$x$ [1] `文字` </code>')
  expect(result.match(/<math /g)).toHaveLength(3)
  expect(result.match(/display="block"/g)).toHaveLength(1)
  expect(result.match(/aria-label="核验引用 1"/g)).toHaveLength(2)
  expect(result).toContain(
    'href="https://example.org/doc" target="_blank" rel="noopener noreferrer"',
  )
  expect(result).not.toMatch(/<p>\s*<div/)
})

it('回答不渲染原始 HTML、追踪图片或危险链接，链接中的引用不变成嵌套按钮', () => {
  const result = renderToStaticMarkup(
    <MentorAnswer
      spaceId="space"
      runId="run"
      citations={[{ id: '1' } as MentorCitation]}
      onCitation={() => {}}
      text={
        '<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n![追踪](https://tracker.example/pixel)\n\n[危险](javascript:alert%281%29)\n\n[凭据](https://user:secret@example.org)\n\n[资料 [1]](https://example.org/doc)'
      }
    />,
  )
  expect(result).not.toMatch(/<script|<img|onerror=|javascript:|user:secret/)
  expect(result).toContain('资料 [1]')
  expect(result).not.toContain('aria-label="核验引用 1"')
})
