import type { ReactNode } from 'react'
import type { MentorSnapshot } from '@/api/mentor'

export function RunResultNotice({ run, children, role = 'alert', className }: {
  run: Pick<MentorSnapshot, 'status' | 'reason' | 'result_unknown' | 'requests_used' | 'cost_unknown'>
  children: ReactNode
  role?: 'alert' | 'status'
  className?: string
}) {
  if (run.reason === 'context_configuration_invalid') {
    return <p className={className} role={role}>模型上下文窗口不足以容纳系统要求和输出预留，请增大上下文窗口或减小单次输出上限。</p>
  }
  if (run.reason === 'run_storage_unavailable') {
    return <p className={className} role={role}>存储暂不可用，已停止后续模型请求。请稍后重试。</p>
  }
  if (run.reason === 'user_stopped' && ['cancelling', 'cancelled'].includes(run.status)) {
    return run.result_unknown || run.requests_used > 0 || run.cost_unknown
      ? <p className="hint" role="status">已发出的请求可能计入用量。</p>
      : null
  }
  if (!run.result_unknown) return null
  return <p className={className} role={role}>{children}</p>
}
