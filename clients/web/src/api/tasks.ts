import { learningClient, unwrap } from './client'
import { pageOf, type Session } from './runtime'
import { mentorSnapshotSchema, mentorStatus, type MentorSnapshot } from './mentor'
import { importAPI } from './import-jobs'
import { notesyncAPI } from './notesync'

export const taskKinds = {
  research: '研究',
  start_learning: '开学准备',
  content_edit: '内容生成与改写',
  mentor: '目标内导师',
  import_job: '资料导入',
  notesync: 'NoteSync 同步审阅',
} as const
export type TaskKind = keyof typeof taskKinds
export type RunKind = Exclude<TaskKind, 'import_job' | 'notesync'>
export const taskRunStatus = { ...mentorStatus, running: '执行中' }
// 各适配器读取原业务服务，保留各自状态和分页，不创建替代运行。
export const taskAdapters = {
  notesync: (session: Session, space: string, collection: string, status: 'all' | 'open' | 'resolved' | 'closed', cursor?: string, signal?: AbortSignal) => notesyncAPI(session, space, collection, signal).reviews(status, cursor),
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
