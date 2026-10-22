// The changelog's Markdown is drawn from a tree, never as HTML: raw tags stay
// text, links keep only http(s) addresses, and the shapes CHANGELOG.md uses
// read as blocks. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'

const { inlineOf, markdownOf } = await import('../app/utils/markdown.ts')

test('paragraphs, lists with wrapped items, and headings', () => {
  const blocks = markdownOf('A line\nthat wraps.\n\nSecond.\n\n- one\n  continued\n- two\n\n### Fixed\n\n* star')
  assert.deepEqual(blocks.map(b => b.t), ['p', 'p', 'ul', 'h', 'ul'])
  assert.equal(blocks[0].inline[0].v, 'A line that wraps.')
  assert.equal(blocks[2].items.length, 2)
  assert.equal(blocks[2].items[0][0].v, 'one continued')
  assert.equal(blocks[3].level, 3)
})

test('inline code, strong, emphasis and links', () => {
  const runs = inlineOf('use `cgctl` **now**, *really* and [docs](https://cloudground.it/docs)')
  assert.deepEqual(runs.map(r => r.t), ['text', 'code', 'text', 'strong', 'text', 'em', 'text', 'link'])
  assert.equal(runs.at(-1).href, 'https://cloudground.it/docs')
})

test('raw HTML stays text and a link keeps only http(s)', () => {
  const runs = inlineOf('<script>alert(1)</script> <img src=x onerror=y> [x](javascript:alert(1)) [y](data:text/html,hi)')
  assert.ok(runs.every(r => r.t !== 'link'), JSON.stringify(runs))
  const text = runs.map(r => r.v).join('')
  assert.ok(text.includes('<script>alert(1)</script>'), text)
  assert.ok(!JSON.stringify(markdownOf('<b>x</b>')).includes('"t":"html"'))
})

test('snake_case words are not emphasis', () => {
  assert.deepEqual(inlineOf('the dev_mode_until field').map(r => r.t), ['text'])
})
