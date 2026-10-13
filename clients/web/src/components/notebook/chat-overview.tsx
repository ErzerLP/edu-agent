import { useQueries } from '@tanstack/react-query'
import { Sparkles, ArrowRight, History, MessageSquare } from 'lucide-react'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { sourceDetailSchema } from '@/api/notebook-sources'
import { sourceKey } from '@/api/source-selection'
import type { Space } from '@/api/runtime'
import { useIdentity } from '@/lib/session'
import { useSourceSelection } from './source-selection'
import { Button } from '../ui/button'
import { SpaceDialog } from './space-dialog'
import { ErrorState } from '../common'

export function ChatOverview({
  space,
  disabled,
  onAsk,
  onContinue,
  onSource,
}: {
  space: Space
  disabled: boolean
  onAsk: (prompt: string) => void
  onContinue?: () => void
  onSource: () => void
}) {
  const { session, prefix } = useIdentity()
  const { selection, items, setCitation } = useSourceSelection()
  const selected = items.filter((s) => selection?.items.some((i) => sourceKey(s) === sourceKey(i)))
  const summaries = useQueries({
    queries: selected.slice(0, 3).map((source) => ({
      queryKey: [...prefix, space.id, 'chat-overview-source', sourceKey(source)],
      gcTime: 0,
      retry: false,
      refetchInterval: 5000,
      queryFn: async ({ signal }: { signal: AbortSignal }) => {
        const value = await unwrap(
          learningClient(session, space.id).GET(
            '/v1/knowledge/sources/{revisionID}/documents/{documentRevisionID}',
            {
              params: {
                path: {
                  revisionID: source.revision_id,
                  documentRevisionID: source.document_revision_id,
                },
                header: {
                  'X-Learning-Space-ID': space.id,
                  'X-Knowledge-Collection-ID': source.collection_id,
                },
              },
              signal,
            },
          ),
          sourceDetailSchema,
        )
        if (sourceKey(value) !== sourceKey(source)) throw new ApiError(502, 'invalid_response')
        return value
      },
    })),
  })
  const questions = [
    '先介绍这个方向的核心概念',
    '这些概念之间有什么联系？',
    '有哪些容易混淆的地方？',
  ]
  return (
    <div className="chat-overview">
      <div className="chat-overview-label">
        <Sparkles size={17} />
        学习区概要
      </div>
      <h3>{space.name}</h3>
      {space.direction ? (
        <p className="chat-direction">
          <span>学习方向</span>
          {space.direction}
        </p>
      ) : (
        <SpaceDialog space={space} confirmDirection />
      )}
      <div className="chat-source-summary">
        {summaries.map((summary, index) => (
          <div key={sourceKey(selected[index])}>
            {summary.error ? (
              <ErrorState error={summary.error} />
            ) : summary.data ? (
              <p>
                <strong>{summary.data.path}</strong>：
                {summary.data.fragments[0]?.text.replace(/\s+/g, ' ').trim().slice(0, 160) ??
                  '没有可定位的正文片段。'}
                {summary.data.fragments[0] && (
                  <button
                    className="chat-citation"
                    aria-label={`概要引用 ${index + 1}`}
                    onClick={() => {
                      setCitation({ id: String(index + 1), source: selected[index] })
                      onSource()
                    }}
                  >
                    {index + 1}
                  </button>
                )}
              </p>
            ) : (
              <p role="status">正在读取 {selected[index].path}…</p>
            )}
          </div>
        ))}
        {selected.length > 3 && (
          <p>
            以及{' '}
            {selected
              .slice(3)
              .map((s) => s.path)
              .join('、')}
          </p>
        )}
      </div>
      <p className="chat-suggestion-label">从一个问题开始</p>
      <div className="chat-suggestions">
        {questions.map((question) => (
          <button key={question} disabled={disabled} onClick={() => onAsk(question)}>
            <MessageSquare size={15} />
            <span>{question}</span>
            <ArrowRight size={14} />
          </button>
        ))}
      </div>
      <div className="actions">
        <Button
          disabled={disabled}
          onClick={() => onAsk('请围绕学习方向和所选资料，从核心概念开始讲解。')}
        >
          <Sparkles size={15} aria-hidden="true" />
          开始学习
        </Button>
        <Button variant="outline" disabled={!onContinue} onClick={onContinue}>
          <History size={15} aria-hidden="true" />
          继续上次
        </Button>
      </div>
    </div>
  )
}
