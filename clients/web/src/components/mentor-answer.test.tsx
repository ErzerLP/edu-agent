import { expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { renderToStaticMarkup } from 'react-dom/server'
import { MentorAnswer } from './mentor-answer'
import type { MentorCitation } from '@/api/source-selection'

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
  expect(result.replace(/<[^>]+>/g, '')).toContain('\n保留换行与未知引用 [2]。')
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
  expect(result.replace(/<[^>]+>/g, '')).toContain('\\$5')
  expect(result).toContain('\\(x')
})

it('未启用公式的其他消息入口保持原有文本渲染', () => {
  const result = renderToStaticMarkup(
    <MentorAnswer spaceId="space" runId="run" text={String.raw`\[x^2\]`} />,
  )
  expect(result).not.toContain('<math')
  expect(result).toContain('\\[x^2\\]')
})
