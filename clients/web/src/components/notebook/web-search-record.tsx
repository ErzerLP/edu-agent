import type { WebProgress } from '@/api/web-search'

const failureLabels: Record<string, string> = {
  too_large: '页面或 PDF 超过读取大小上限',
  unsupported_type: '不支持的内容类型',
  timeout: '读取或解析超时',
  parse_empty: '未提取到可引用正文；页面可能需要动态加载，PDF 可能没有文本层',
  blocked_policy: '网络安全策略阻止读取',
  storage_restricted: '站点声明禁止保存、归档或摘要，未保留正文',
  unsupported_encoding: '不支持或损坏的字符集／压缩编码',
  invalid_compression: '响应压缩数据损坏',
  body_read_failed: '正文传输中断',
  dns_failed: '域名解析失败',
  network_failed: '网络连接失败',
  cancelled: '读取已取消',
  redirect_limit: '重定向异常或次数超限',
  pdf_parse_failed: 'PDF 损坏或解析失败',
  pdf_encrypted: 'PDF 已加密，无法读取',
  pdf_resource_limit: 'PDF 解析达到资源限制',
  body_unavailable: '未取得正文；旧记录未区分具体原因',
}

export function failureLabel(category: string) {
  if (/^http_\d{3}$/.test(category)) return `站点返回 HTTP ${category.slice(5)}`
  return failureLabels[category] ?? '网页读取失败'
}

export function WebSearchRecord({ progress }: { progress: WebProgress }) {
  if (!progress.queries?.length && !progress.failures?.length) return null
  return <details>
    <summary>本轮检索记录</summary>
    {!!progress.queries?.length && <ul>{progress.queries.map((text, index) => <li key={index}>{text}</li>)}</ul>}
    {!!progress.failures?.length && <>
      <p>读取失败</p>
      <ul>{progress.failures.map(failure => <li key={failure.resource_id}>
        {failure.title || '网页'}：{failureLabel(failure.category)} · <code>{failure.category}</code>
        {failure.reason !== failure.category && <>（{failure.reason}）</>}
      </li>)}</ul>
    </>}
    <p>已发起的搜索请求数和未知结果可在用量页查看。</p>
  </details>
}
