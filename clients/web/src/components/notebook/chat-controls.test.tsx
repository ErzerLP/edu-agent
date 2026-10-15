import { expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { SubmissionNotice } from './submission-notice'
import { ChatInput, sendsOnEnter } from './chat-input'

it('pending 仅在请求结束仍未确认时提示，在途和已受理时隐藏', () => {
  const render = (pending: boolean, inFlight: boolean) => renderToStaticMarkup(
    <SubmissionNotice pending={pending} inFlight={inFlight}>提交结果待核对。重试</SubmissionNotice>,
  )
  expect(render(true, true)).toBe('')
  expect(render(false, false)).toBe('')
  expect(render(true, false)).toContain('提交结果待核对')
  expect(render(true, false)).not.toContain('role="alert"')
})

const input = { value: '', onChange: () => {}, onSend: () => {}, onStop: () => {}, disabled: false, ready: true, running: false, stopping: false }
it('空输入禁用发送，运行时同一按钮变为停止，停止中禁用，结束后恢复发送', () => {
  expect(renderToStaticMarkup(<ChatInput {...input} />)).toMatch(/aria-label="发送提问"[^>]*disabled/)
  const running = renderToStaticMarkup(<ChatInput {...input} running />)
  expect(running).toContain('aria-label="停止生成"')
  expect(running).toMatch(/<button [^>]*type="button"/)
  expect(running.match(/<button /g)).toHaveLength(1)
  expect(running).not.toContain('disabled=""')
  const stopping = renderToStaticMarkup(<ChatInput {...input} running stopping />)
  expect(stopping).toMatch(/aria-label="停止生成"[^>]*disabled/)
  expect(stopping).toContain('aria-busy="true"')
  const idle = renderToStaticMarkup(<ChatInput {...input} value="问题" />)
  expect(idle).toContain('aria-label="发送提问"')
  expect(idle).not.toContain('disabled=""')
})

it('仅 Enter 发送，Shift+Enter、IME 组字和兼容组字键保留编辑', () => {
  const enter = { key: 'Enter', shiftKey: false, keyCode: 13, nativeEvent: { isComposing: false } }
  expect(sendsOnEnter(enter)).toBe(true)
  expect(sendsOnEnter({ ...enter, shiftKey: true })).toBe(false)
  expect(sendsOnEnter({ ...enter, nativeEvent: { isComposing: true } })).toBe(false)
  expect(sendsOnEnter({ ...enter, keyCode: 229 })).toBe(false)
  expect(sendsOnEnter({ ...enter, key: 'a' })).toBe(false)
})
