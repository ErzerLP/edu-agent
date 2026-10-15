import { useLayoutEffect, useRef } from 'react'
import { ArrowUp, LoaderCircle, Square } from 'lucide-react'
import { Button } from '../ui/button'

export function sendsOnEnter(e: {
  key: string
  shiftKey: boolean
  keyCode: number
  nativeEvent: { isComposing: boolean }
}) {
  return e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing && e.keyCode !== 229
}

export function ChatInput({ value, onChange, onSend, onStop, disabled, ready, running, stopping, stopDisabled = false }: {
  value: string
  onChange: (value: string) => void
  onSend: () => void
  onStop: () => void
  disabled: boolean
  ready: boolean
  running: boolean
  stopping: boolean
  stopDisabled?: boolean
}) {
  const textarea = useRef<HTMLTextAreaElement>(null)
  useLayoutEffect(() => {
    const element = textarea.current
    if (!element) return
    const resize = () => {
      element.style.height = 'auto'
      element.style.height = `${Math.min(element.scrollHeight, 144)}px`
    }
    resize()
    // Width changes (mobile panel or window resize) also change line wrapping.
    const observer = new ResizeObserver(resize)
    observer.observe(element.parentElement!)
    return () => observer.disconnect()
  }, [value])
  return <div className="chat-input">
    <textarea
      ref={textarea}
      aria-label="向导师提问"
      placeholder="对这些资料，你有什么好奇的？"
      rows={1}
      maxLength={16000}
      value={value}
      disabled={disabled}
      onChange={e => onChange(e.target.value)}
      onKeyDown={e => {
        if (sendsOnEnter(e)) {
          e.preventDefault()
          if (!running && ready && value.trim()) onSend()
        }
      }}
    />
    <Button
      className="chat-submit"
      aria-label={running ? '停止生成' : '发送提问'}
      type={running ? 'button' : 'submit'}
      disabled={running ? stopping || stopDisabled : !ready || !value.trim()}
      aria-busy={stopping || undefined}
      onClick={running ? onStop : undefined}
    >
      {stopping ? <LoaderCircle className="chat-spinner" size={17} aria-hidden="true" /> : running ? <Square size={14} fill="currentColor" aria-hidden="true" /> : <ArrowUp size={17} aria-hidden="true" />}
    </Button>
  </div>
}
