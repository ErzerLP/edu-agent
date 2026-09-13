import type { ReactNode } from 'react'
import * as AlertDialog from '@radix-ui/react-alert-dialog'
import { Button } from './ui/button'
import { errorText } from '@/api/client'

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  return (
    <div className="notice error" role="alert">
      <p>{errorText(error)}</p>
      {retry && (
        <Button variant="outline" onClick={retry}>
          重试
        </Button>
      )}
    </div>
  )
}
export function EmptyState({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}
export function CapabilityGate({
  available,
  reason,
  children,
}: {
  available: boolean
  reason: string
  children: ReactNode
}) {
  return (
    <div className="capability">
      {children}
      {!available && <p className="hint">{reason}</p>}
    </div>
  )
}
export function Pagination({
  next,
  previous,
  page,
}: {
  next?: () => void
  previous?: () => void
  page: number
}) {
  return (
    <nav aria-label="分页" className="pagination">
      <Button variant="outline" disabled={!previous} onClick={previous}>
        上一页
      </Button>
      <span>第 {page} 页</span>
      <Button variant="outline" disabled={!next} onClick={next}>
        下一页
      </Button>
    </nav>
  )
}
export function Confirm({
  label,
  title,
  children,
  onConfirm,
  disabled = false,
}: {
  label: string
  title: string
  children: ReactNode
  onConfirm: () => void
  disabled?: boolean
}) {
  return (
    <AlertDialog.Root>
      <AlertDialog.Trigger asChild>
        <Button variant="outline" disabled={disabled}>
          {label}
        </Button>
      </AlertDialog.Trigger>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="overlay" />
        <AlertDialog.Content className="dialog">
          <AlertDialog.Title>{title}</AlertDialog.Title>
          <AlertDialog.Description asChild>
            <div>{children}</div>
          </AlertDialog.Description>
          <div className="actions">
            <AlertDialog.Cancel asChild>
              <Button variant="outline">取消</Button>
            </AlertDialog.Cancel>
            <AlertDialog.Action asChild>
              <Button variant="destructive" onClick={onConfirm}>
                确认{label}
              </Button>
            </AlertDialog.Action>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  )
}
