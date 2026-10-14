import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { collectionsSchema, knowledgeAPI, type Collection } from '@/api/references'
import { learningClient, unwrap } from '@/api/client'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { ReferenceImport } from '../reference-import'
import { noteSourceItem, type NoteSourceCandidate } from '@/api/studio'

export function AddSourceDialog({ spaceId, initiallyOpen = false, onClose }: { spaceId: string; initiallyOpen?: boolean; onClose?: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null),
    trigger = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(false)
  useEffect(() => { if (initiallyOpen) { setOpen(true); dialog.current?.showModal() } }, [initiallyOpen])
  return (
    <>
      <Button
        ref={trigger}
        onClick={() => {
          setOpen(true)
          dialog.current?.showModal()
        }}
      >
        ＋ 添加来源
      </Button>
      <dialog
        ref={dialog}
        className="source-dialog expanded add-source-dialog"
        aria-label="添加来源"
        onClose={() => {
          setOpen(false)
          trigger.current?.focus()
          onClose?.()
        }}
      >
        <h2>添加来源</h2>
        <Button autoFocus variant="outline" onClick={() => dialog.current?.close()}>
          关闭添加来源
        </Button>
        {open && <AddSourceContent spaceId={spaceId} />}
      </dialog>
    </>
  )
}

export function AddSourceContent({ spaceId, noteCandidate }: { spaceId: string; noteCandidate?: NoteSourceCandidate }) {
  const { session, prefix, drafts } = useIdentity()
  const key = JSON.stringify([...prefix, spaceId, 'add-source', noteCandidate?.derived_from ?? null])
  const queryClient = useQueryClient()
  const [method, setMethod] = useState<'pdf' | 'text' | 'url' | 'search'>(
    () => drafts.get(`${key}:method`) ?? 'pdf',
  )
  const [collectionId, setCollectionId] = useState(
    () => drafts.get<string>(`${key}:collection`) ?? '',
  )
  const [name, setName] = useState(() => drafts.get<string>(`${key}:name`) ?? '学习资料')
  const [busy, setBusy] = useState(false),
    [error, setError] = useState<unknown>()
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  const collections = useQuery({
    queryKey: [...prefix, spaceId, 'notebook-collections'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/collections', {
          params: { header: { 'X-Learning-Space-ID': spaceId } },
          signal,
        }),
        collectionsSchema,
      ),
  })
  const select = (c: Collection) => {
    drafts.set(`${key}:collection`, c.id)
    setCollectionId(c.id)
  }
  const collection = collections.data?.items.find((c) => c.id === collectionId)
  const refresh = () => queryClient.invalidateQueries({ queryKey: [...prefix, spaceId] })
  const create = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const payload = { name: name.trim(), source: 'Notebook 用户添加来源' }
      const c = await knowledgeAPI(session, spaceId).collection({
        ...payload,
        id: drafts.operation(`${key}:create`, payload),
        action: 'create',
      })
      if (live.current) select(c)
      await refresh()
    } catch (error) {
      if (live.current) setError(error)
    } finally {
      if (live.current) setBusy(false)
    }
  }
  if (collections.error)
    return <ErrorState error={collections.error} retry={() => void collections.refetch()} />
  return (
    <>
      <p>{noteCandidate ? `复制笔记「${noteCandidate.title}」第 ${noteCandidate.derived_from.version} 版。它是个人理解，不是外部权威；以后修改笔记不会同步到此来源。` : '选择 PDF、文本、网址或搜索，预览后保存。'}已有课堂使用的资料保持不变。</p>
      {!noteCandidate && <div className="actions" aria-label="添加方式">
        {(
          [
            ['pdf', 'PDF 上传'],
            ['text', '粘贴文本'],
            ['url', '网址'],
            ['search', '搜索发现'],
          ] as const
        ).map(([id, label]) => (
          <Button
            key={id}
            variant="outline"
            aria-pressed={method === id}
            onClick={() => {
              drafts.set(`${key}:method`, id)
              setMethod(id)
            }}
          >
            {label}
          </Button>
        ))}
      </div>}
      <p role="status" className="add-source-status">
        {collections.isPending ? '正在读取目标集合…' : '\u00a0'}
      </p>
      <label>
        保存到已关联集合
        <select
          value={collectionId}
          onChange={(e) => {
            drafts.set(`${key}:collection`, e.target.value)
            setCollectionId(e.target.value)
          }}
        >
          <option value="">请选择或创建集合</option>
          {collections.data?.items.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
              {c.shared ? '（共享集合）' : ''} · {c.id.slice(0, 8)}
            </option>
          ))}
        </select>
      </label>
      <details open={!collectionId}>
        <summary>创建区内私有集合</summary>
        <label>
          新集合名称
          <input
            value={name}
            maxLength={120}
            disabled={busy}
            onChange={(e) => {
              drafts.set(`${key}:name`, e.target.value)
              setName(e.target.value)
            }}
          />
        </label>
        <Button disabled={busy || !name.trim()} onClick={() => void create()}>
          创建并选用集合
        </Button>
        {!!error && <ErrorState error={error} />}
      </details>
      {collection && (
        <ReferenceImport
          key={collection.id}
          spaceId={spaceId}
          collection={collection}
          target={noteCandidate ? `studio-${noteCandidate.derived_from.artifact_id}-${noteCandidate.derived_from.version}` : 'notebook'}
          method={noteCandidate ? 'note' : method}
          initialItems={noteCandidate ? [noteSourceItem(noteCandidate)] : undefined}
          onImported={() => {
            void refresh()
          }}
        />
      )}
    </>
  )
}
