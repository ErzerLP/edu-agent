import { useQuery } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { collectionsSchema, treeSchema, type Collection } from '@/api/references'
import { useIdentity } from './session'

export function useNotebookSources(spaceId: string) {
  const { session, prefix } = useIdentity()
  const available = session.device.scopes.includes('knowledge:read')
  const collections = useQuery({
    queryKey: [...prefix, spaceId, 'notebook-collections'],
    enabled: available,
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/collections', {
          params: { header: { 'X-Learning-Space-ID': spaceId }, query: { shared: false } },
          signal,
        }),
        collectionsSchema,
      ),
  })
  return { available, collections }
}

export function useNotebookCollectionSources(
  spaceId: string,
  collection: Collection,
  expanded: boolean,
) {
  const { session, prefix } = useIdentity()
  const revisionId = collection.head_revision_id
  return useQuery({
    queryKey: [...prefix, spaceId, collection.id, revisionId, 'notebook-source-tree'],
    enabled: expanded && !!revisionId && session.device.scopes.includes('knowledge:read'),
    gcTime: 0,
    queryFn: async ({ signal }) => {
      const tree = await unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/revisions/{revisionID}/tree', {
          params: {
            path: { revisionID: revisionId! },
            header: { 'X-Learning-Space-ID': spaceId, 'X-Knowledge-Collection-ID': collection.id },
          },
          signal,
        }),
        treeSchema,
      )
      if (
        tree.revision.revision_id !== revisionId ||
        tree.revision.documents.some(
          (document) => document.collection_id && document.collection_id !== collection.id,
        )
      )
        throw new ApiError(502, 'invalid_response')
      return tree.revision
    },
  })
}
