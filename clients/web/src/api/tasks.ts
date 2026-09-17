import { learningClient, unwrap } from './client'
import { pageOf, type Session } from './runtime'
import { mentorSnapshotSchema, mentorStatus, type MentorSnapshot } from './mentor'
import { importAPI } from './import-jobs'

export const taskKinds = {
  research: '研究',
  start_learning: '开学准备',
  content_edit: '内容生成与改写',
  mentor: '目标内导师',
  import_job: '资料导入',
} as const
export type TaskKind = keyof typeof taskKinds
export type RunKind = Exclude<TaskKind, 'import_job'>
export const taskRunStatus = { ...mentorStatus, running: '执行中' }
// 适配器只读取原 owner；同步尚未接入，因此不注册占位任务。
export const taskAdapters = {
  runs: (
    session: Session,
    space: string,
    kind?: RunKind,
    status?: MentorSnapshot['status'],
    cursor?: string,
    signal?: AbortSignal,
  ) =>
    unwrap(
      learningClient(session, space).GET('/v1/learning/runs', {
        params: {
          header: { 'X-Learning-Space-ID': space },
          query: { task_kind: kind, status, cursor, limit: 20 },
        },
        signal,
      }),
      pageOf(mentorSnapshotSchema),
    ),
  imports: (
    session: Session,
    space: string,
    collection: string,
    cursor?: string,
    signal?: AbortSignal,
  ) => importAPI(session, space, collection, signal).list(cursor),
}
