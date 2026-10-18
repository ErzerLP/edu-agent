import { useEffect, useId, useRef, useState } from 'react'
import { Check, ChevronDown, MessageCircle, Plus } from 'lucide-react'

type Conversation = { id: string; title: string }

export function ConversationSwitcher({
  options,
  conversation,
  conversationId,
  busy,
  onConversation,
}: {
  options: Conversation[]
  conversation?: Conversation
  conversationId?: string
  busy: boolean
  onConversation: (id?: string) => void
}) {
  const [open, setOpen] = useState(false)
  const [focusedId, setFocusedId] = useState(conversationId)
  const root = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const listId = useId()
  const history = conversation && !options.some((item) => item.id === conversation.id)
    ? [conversation, ...options]
    : options
  const items = [{ id: undefined, title: '新对话' }, ...history]
  const title = items.find((item) => item.id === conversationId)?.title ?? '新对话'
  const close = () => {
    setOpen(false)
    trigger.current?.focus()
  }

  useEffect(() => {
    if (!open) return
    const element = list.current!
    const selected = element.querySelector<HTMLElement>('[aria-selected="true"]')
    const target = selected ?? element.querySelector<HTMLElement>('[role="option"]')
    target?.focus()
    const outside = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('pointerdown', outside)
    return () => document.removeEventListener('pointerdown', outside)
  }, [open])

  return (
    <div
      className="chat-conversation-switcher"
      ref={root}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false)
      }}
      onKeyDown={(event) => {
        if (open && event.key === 'Escape') {
          event.preventDefault()
          event.stopPropagation()
          close()
        }
      }}
    >
      <button
        type="button"
        className="chat-conversation-trigger"
        ref={trigger}
        aria-label="切换对话"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        disabled={busy}
        onClick={() => setOpen(!open)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault()
            setOpen(true)
          }
        }}
      >
        <MessageCircle size={16} aria-hidden="true" />
        <span>{title}</span>
        <ChevronDown size={15} aria-hidden="true" />
      </button>
      {open && (
        <div
          className="chat-conversation-menu"
          id={listId}
          ref={list}
          role="listbox"
          aria-label="切换对话"
          onKeyDown={(event) => {
            if (!busy && ['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
              event.preventDefault()
              const entries = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="option"]'))
              const index = entries.indexOf(document.activeElement as HTMLButtonElement)
              const next = event.key === 'Home' ? 0 : event.key === 'End' ? entries.length - 1
                : (index + (event.key === 'ArrowDown' ? 1 : -1) + entries.length) % entries.length
              entries[next]?.focus()
            }
          }}
        >
          {items.map((item, index) => (
            <button
              type="button"
              key={item.id ?? 'new'}
              className="chat-conversation-option"
              role="option"
              aria-selected={item.id === conversationId}
              tabIndex={item.id === focusedId ? 0 : -1}
              disabled={busy}
              onFocus={() => setFocusedId(item.id)}
              onClick={() => {
                close()
                onConversation(item.id)
              }}
            >
              {index === 0 ? <Plus size={17} aria-hidden="true" /> : <MessageCircle size={17} aria-hidden="true" />}
              <span>{item.title}</span>
              {item.id === conversationId && <Check size={16} aria-hidden="true" />}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
