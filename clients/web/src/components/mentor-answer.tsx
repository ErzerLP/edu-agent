import { citationParts, type MentorCitation } from '@/api/source-selection'
import { MathBlock, SafeMarkdown } from './content-blocks'
import { SourceDialog, MentorCitationContent } from './notebook/source-viewer'
import { Button } from './ui/button'

type MarkdownNode = {
  type: string
  value?: string
  url?: string
  alt?: string
  children?: MarkdownNode[]
  data?: unknown
}

function answerMarkdown(text: string, renderMath: boolean, citationIds: string[]) {
  const formulas: { text: string; displayMode: boolean; raw: string }[] = []
  // Protect formulas before Markdown consumes backslashes or interprets TeX as emphasis.
  // The marker is absent from the input, and code/HTML/link nodes are left untouched below.
  let marker = '\uE000math'
  while (text.includes(marker)) marker += 'x'
  const prepared = renderMath
    ? text.replace(
        /\\\\|\\\$|^ {0,3}(?<fence>`{3,}|~{3,})[^\n]*(?:\n[\s\S]*?(?:^ {0,3}\k<fence>[`~]*[ \t]*(?:\n|$)|$(?![\s\S])))?|^(?: {4}|\t)[^\n]*(?:\n|$)|(?<ticks>`+)[\s\S]*?\k<ticks>(?!`)|\\\([\s\S]*?\\\)|\\\[[\s\S]*?\\\]|\$\$[\s\S]*?\$\$|\$(?:\\.|[^$\\\n])+?\$(?!\d)|\\[([]/gm,
        (segment) => {
          if (!/^(?:\\\(|\\\[|\$)/.test(segment)) return segment
          if (segment.length === 2 && segment.startsWith('\\')) return `\\${segment}`
          const displayMode = segment.startsWith('\\[') || segment.startsWith('$$')
          const delimiterLength = segment.startsWith('$') && !displayMode ? 1 : 2
          formulas.push({
            text: segment.slice(delimiterLength, -delimiterLength),
            displayMode,
            raw: segment,
          })
          return `${marker}${formulas.length - 1}\uE001`
        },
      )
    : text
  const tokens = new RegExp(`(${marker}[0-9]+\uE001)`, 'g')
  const plugin = () => (tree: MarkdownNode) => {
    function visit(node: MarkdownNode) {
      if (
        ['code', 'inlineCode', 'html', 'link', 'linkReference', 'image', 'imageReference'].includes(
          node.type,
        )
      ) {
        // Restore literal delimiters in links and HTML without inserting interactive children.
        restoreNode(node)
        return
      }
      if (!node.children) return
      node.children = node.children.flatMap((child): MarkdownNode[] => {
        if (child.type !== 'text' || !child.value) {
          visit(child)
          return [child]
        }
        return child.value
          .split(tokens)
          .filter(Boolean)
          .flatMap((segment): MarkdownNode[] => {
            if (segment.startsWith(marker)) {
              const index = Number(segment.slice(marker.length, -1))
              return [
                {
                  type: formulas[index].displayMode ? 'answerDisplayMath' : 'answerMath',
                  data: {
                    hName: formulas[index].displayMode ? 'div' : 'span',
                    hProperties: { 'data-answer-math': index },
                  },
                },
              ]
            }
            return citationParts(segment, citationIds).map((part) =>
              part.id
                ? {
                    type: 'answerCitation',
                    data: { hName: 'span', hProperties: { 'data-answer-citation': part.id } },
                  }
                : { type: 'text', value: part.text },
            )
          })
      })
      // Display formulas become paragraph siblings, keeping block elements out of <p>.
      node.children = node.children.flatMap((child) => {
        if (
          child.type !== 'paragraph' ||
          !child.children?.some((part) => part.type === 'answerDisplayMath')
        )
          return [child]
        const blocks: MarkdownNode[] = []
        let paragraph: MarkdownNode = { type: 'paragraph', children: [] }
        for (const part of child.children) {
          if (part.type === 'answerDisplayMath') {
            if (paragraph.children!.length) blocks.push(paragraph)
            blocks.push(part)
            paragraph = { type: 'paragraph', children: [] }
          } else paragraph.children!.push(part)
        }
        if (paragraph.children!.length) blocks.push(paragraph)
        return blocks
      })
    }
    function restore(value: string) {
      return value.replace(tokens, (token) => {
        const formula = formulas[Number(token.slice(marker.length, -1))]
        return formula.raw
      })
    }
    function restoreNode(node: MarkdownNode) {
      if (node.value) node.value = restore(node.value)
      if (node.url) node.url = restore(node.url)
      if (node.alt) node.alt = restore(node.alt)
      node.children?.forEach(restoreNode)
    }
    visit(tree)
  }
  return { prepared, formulas, plugin }
}

export function MentorAnswer({
  spaceId,
  runId,
  text,
  citations = [],
  onCitation,
  renderMath = true,
}: {
  spaceId: string
  runId: string
  text: string
  citations?: MentorCitation[]
  onCitation?: (runId: string, id: string) => void
  renderMath?: boolean
}) {
  const { prepared, formulas, plugin } = answerMarkdown(
    text,
    renderMath,
    citations.map((c) => c.id),
  )
  return (
    <div className="mentor-markdown">
      <SafeMarkdown
        text={prepared}
        remarkPlugins={[plugin]}
        components={{
          div: ({ node, children }) => {
            const index = node?.properties['data-answer-math']
            return index !== undefined ? (
              <MathBlock {...formulas[Number(index)]} />
            ) : (
              <div>{children}</div>
            )
          },
          span: ({ node, children }) => {
            const index = node?.properties['data-answer-math']
            if (index !== undefined) return <MathBlock {...formulas[Number(index)]} />
            const id = node?.properties['data-answer-citation']
            if (typeof id !== 'string') return <span>{children}</span>
            return onCitation ? (
              <Button
                className="citation-link"
                variant="outline"
                onClick={() => onCitation(runId, id)}
                aria-label={`核验引用 ${id}`}
              >
                [{id}]
              </Button>
            ) : (
              <SourceDialog label={`[${id}]`} title={`核验引用 ${id}`}>
                <MentorCitationContent spaceId={spaceId} runId={runId} citationId={id} />
              </SourceDialog>
            )
          },
        }}
      />
    </div>
  )
}
