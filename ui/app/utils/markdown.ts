/**
 * The small part of Markdown the changelog uses, read into a tree the
 * interface draws with components. There is no HTML anywhere: text is text,
 * a link keeps only an http or https address, and nothing is ever handed to
 * v-html (CHANGELOG.md is not trusted more than any other text).
 *
 * Blocks: `#`-`######` headings, `-` or `*` lists (a line indented under an
 * item continues it), paragraphs split by blank lines. Inline: `code`,
 * **strong**, *emphasis* and _emphasis_, [text](https://address).
 */

export type Inline
  = | { t: 'text', v: string }
    | { t: 'code', v: string }
    | { t: 'strong', v: string }
    | { t: 'em', v: string }
    | { t: 'link', v: string, href: string }

export type Block
  = | { t: 'h', level: number, inline: Inline[] }
    | { t: 'p', inline: Inline[] }
    | { t: 'ul', items: Inline[][] }

const inlineRe = /`([^`]+)`|\*\*([^*]+)\*\*|\*([^*\s][^*]*)\*|(?<![A-Za-z0-9])_([^_\s][^_]*)_(?![A-Za-z0-9])|\[([^\]]+)\]\(([^)\s]+)\)/g

/** Reads one line (or paragraph) of text into runs. */
export function inlineOf(text: string): Inline[] {
  const out: Inline[] = []
  let last = 0
  for (const m of text.matchAll(inlineRe)) {
    if (m.index > last) out.push({ t: 'text', v: text.slice(last, m.index) })
    if (m[1] !== undefined) out.push({ t: 'code', v: m[1] })
    else if (m[2] !== undefined) out.push({ t: 'strong', v: m[2] })
    else if (m[3] !== undefined) out.push({ t: 'em', v: m[3] })
    else if (m[4] !== undefined) out.push({ t: 'em', v: m[4] })
    else if (/^https?:\/\//i.test(m[6]!)) out.push({ t: 'link', v: m[5]!, href: m[6]! })
    else out.push({ t: 'text', v: m[5]! })
    last = m.index + m[0].length
  }
  if (last < text.length) out.push({ t: 'text', v: text.slice(last) })
  return out
}

/** Reads a Markdown body into blocks. */
export function markdownOf(md: string): Block[] {
  const blocks: Block[] = []
  let para: string[] = []
  let list: string[] | null = null
  const endPara = () => {
    if (para.length) blocks.push({ t: 'p', inline: inlineOf(para.join(' ')) })
    para = []
  }
  const endList = () => {
    if (list) blocks.push({ t: 'ul', items: list.map(inlineOf) })
    list = null
  }
  for (const raw of md.replace(/\r\n/g, '\n').split('\n')) {
    const line = raw.trimEnd()
    const heading = /^(#{1,6})\s+(.*)$/.exec(line)
    const item = /^\s{0,3}[-*]\s+(.*)$/.exec(line)
    if (!line.trim()) {
      endPara()
      endList()
    } else if (heading) {
      endPara()
      endList()
      blocks.push({ t: 'h', level: heading[1]!.length, inline: inlineOf(heading[2]!) })
    } else if (item) {
      endPara()
      list ??= []
      list.push(item[1]!)
    } else if (list && /^\s+\S/.test(line)) {
      list[list.length - 1] += ' ' + line.trim()
    } else {
      endList()
      para.push(line.trim())
    }
  }
  endPara()
  endList()
  return blocks
}
