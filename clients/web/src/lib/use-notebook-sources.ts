import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { z } from 'zod'
import { learningClient, unwrap } from '@/api/client'
import { sourcePageSchema } from '@/api/notebook-sources'
import { useIdentity } from './session'

export function useNotebookSources(spaceId: string) {
  const { session, prefix } = useIdentity()
  const available = session.device.scopes.includes('knowledge:read')
  const capabilities = useQuery({
    queryKey: [...prefix, spaceId, 'source-capabilities'],
    enabled: available,
    gcTime: 0,
    retry: false,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/source-capabilities', { signal }),
        z.object({ source_catalog: z.boolean().optional() }),
      ),
  })
  const sources = useInfiniteQuery({
    queryKey: [...prefix, spaceId, 'notebook-sources'],
    enabled: available && capabilities.data?.source_catalog === true,
    gcTime: 0,
    retry: false,
    initialPageParam: '',
    getNextPageParam: (page: z.infer<typeof sourcePageSchema>) => page.next_cursor || undefined,
    queryFn: ({ signal, pageParam }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/sources', {
          params: {
            header: { 'X-Learning-Space-ID': spaceId },
            query: { limit: 30, ...(pageParam ? { cursor: pageParam } : {}) },
          },
          signal,
        }),
        sourcePageSchema,
      ),
  })
  return { available, capabilities, sources }
}
