import { z } from 'zod'
import { learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'

export type DiscoveryRequest = components['schemas']['SourceDiscoveryRequest']
export type DiscoveryPolicy = components['schemas']['SourceDiscoveryPolicy']
export const discoveryCandidateSchema = z.strictObject({
  title: z.string().min(1).max(4096),
  url: z
    .url()
    .max(8192)
    .refine((value) => ['http:', 'https:'].includes(new URL(value).protocol)),
  summary: z.string().max(16384),
  status: z.literal('summary_only'),
})
export type DiscoveryCandidate = z.infer<typeof discoveryCandidateSchema>
export const discoveryResultSchema = z.strictObject({
  items: z.array(discoveryCandidateSchema).max(4),
  request_budget: z.literal(1),
  requests_used: z.literal(1),
  cost_estimate: z.literal('unknown'),
})
export const discoveryCapabilitiesSchema = z.object({
  source_discovery: z
    .object({
      available: z.boolean(),
      reason: z.enum(['', 'missing_scope', 'search_not_configured']),
      max_candidates: z.literal(4),
      request_budget: z.literal(1),
    })
    .optional(),
})

export function discoveryRequest(
  keywords: string,
  domainText: string,
  consent: boolean,
): DiscoveryRequest {
  const domains = domainText.trim()
    ? [
        ...new Set(
          domainText
            .trim()
            .split(/[\s,，]+/)
            .map((value) => value.toLowerCase()),
        ),
      ]
    : []
  if (!consent) throw new Error('请先明确确认外发的公开关键词与域名限制')
  if (
    !keywords.trim() ||
    new TextEncoder().encode(keywords).length > 300 ||
    /[\x00\r\n]/.test(keywords)
  )
    throw new Error('公开关键词须为单行，最多 300 UTF-8 字节')
  if (
    domains.length > 5 ||
    domains.some((domain) => domain.length > 253 || !/^[a-z0-9-]+(?:\.[a-z0-9-]+)+$/.test(domain))
  )
    throw new Error('请填写最多 5 个公开域名，不包含协议、路径或通配符')
  const policy: DiscoveryPolicy = { mode: domains.length ? 'restrict' : 'supplement', domains }
  const query = domains.length
    ? `(${domains.map((domain) => `site:${domain}`).join(' OR ')}) (${keywords})`
    : keywords
  if (new TextEncoder().encode(query).length > 1024)
    throw new Error('关键词与域名限制合计超过搜索请求预算')
  return { keywords, external_consent: true, policy }
}

export const discoverSources = (
  session: Session,
  spaceId: string,
  body: DiscoveryRequest,
  signal: AbortSignal,
) =>
  unwrap(
    learningClient(session, spaceId).POST('/v1/knowledge/source-discovery', {
      params: { header: { 'X-Learning-Space-ID': spaceId } },
      body,
      signal,
    }),
    discoveryResultSchema,
  )
