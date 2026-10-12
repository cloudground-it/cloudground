// The theme (app/app.config.ts) against regressions a reading of a page
// does not catch: one empty state at every size, and a tab list whose
// divider stays inside its own box. The config is loaded as it is (Node
// strips its types) with defineAppConfig provided. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'

globalThis.defineAppConfig = c => c
const { default: config } = await import('../app/app.config.ts')
const { ui } = config

test('every empty state size reads the same, with a larger title', () => {
  const sizes = Object.values(ui.empty.variants.size)
  assert.equal(sizes.length, 5)
  for (const s of sizes) assert.deepEqual(s, sizes[0])
  assert.match(sizes[0].title, /\btext-xl\b/)
  assert.match(sizes[0].description, /\btext-sm\b/)
})

test('an empty state sits close to the top of its card', () => {
  // Nuxt UI's own p-8 from lg up is replaced at every breakpoint.
  for (const bp of ['', 'sm:', 'lg:']) assert.match(ui.empty.slots.root, new RegExp(`(^| )${bp}py-[0-7]( |$)`))
})

test('an empty state icon is not drawn in the neutral avatar colour', () => {
  assert.match(ui.empty.slots.avatar, /\[&>\[data-slot=icon\]\]:text-cg-blue/)
})

test('a link tab list keeps its divider inside its box', () => {
  const link = ui.tabs.compoundVariants.find(v => v.variant === 'link' && v.orientation === 'horizontal')
  assert.match(link.class.list, /\bborder-b\b/)
  assert.match(link.class.list, /(^| )mb-0( |$)/, 'Nuxt UI\'s -mb-px puts the divider where a scroller clips it')
})

test('the tab bar draws its divider under the tabs, not below the scroller', () => {
  const src = readFileSync(new URL('../app/components/TabBar.vue', import.meta.url), 'utf8')
  const nav = src.match(/<nav [^>]*>/)[0]
  assert.doesNotMatch(nav, /border-b/)
  assert.match(nav, /shadow-\[inset_0_-1px_0_var\(--ui-border\)\]/)
})

// The design system's colours (DESIGN.md): every value is a token in
// main.css, and every text pair reaches WCAG AA in both themes.
const css = readFileSync(new URL('../app/assets/css/main.css', import.meta.url), 'utf8')
const block = sel => {
  const i = css.indexOf(`\n${sel} {`)
  assert.notEqual(i, -1, `${sel} block in main.css`)
  return css.slice(i, css.indexOf('\n}', i))
}
const tokens = (src) => Object.fromEntries([...src.matchAll(/--cg-([\w-]+):\s*(#[0-9A-Fa-f]{6})\b/g)].map(m => [m[1], m[2]]))
const light = tokens(block(':root'))
const dark = { ...light, ...tokens(block('.dark')) }
const lum = (hex) => {
  const c = [1, 3, 5].map(i => parseInt(hex.slice(i, i + 2), 16) / 255).map(v => v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4)
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]
}
const ratio = (a, b) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05) }

test('every text pair of the palette reaches AA in both themes', () => {
  const text = ['ink', 'text', 'muted', 'blue', 'stamp', 'ok', 'warn', 'err']
  const grounds = ['paper', 'paper-2', 'paper-3']
  const pairs = [
    ...text.flatMap(f => grounds.map(g => [f, g, 4.5])),
    ['ok', 'ok-tint', 4.5], ['warn', 'warn-tint', 4.5], ['err', 'err-tint', 4.5], ['blue', 'blue-tint', 4.5],
    ['ink', 'blue-tint', 4.5], ['text', 'err-tint', 4.5], ['text', 'warn-tint', 4.5],
    ['on-orange', 'orange', 4.5], ['paper', 'ink', 4.5],
    ['code-fg', 'code-bg', 4.5], ['code-muted', 'code-bg', 4.5],
    // Control frames and the bars of a meter (WCAG 1.4.11).
    ['rule-strong', 'paper', 3], ['orange', 'paper', 3], ['blue', 'paper-3', 3]
  ]
  for (const [name, t] of [['light', light], ['dark', dark]]) {
    for (const [f, g, min] of pairs) {
      assert.ok(t[f] && t[g], `${name}: --cg-${f} and --cg-${g} are defined`)
      const r = ratio(t[f], t[g])
      assert.ok(r >= min, `${name}: --cg-${f} on --cg-${g} is ${r.toFixed(2)}:1, under ${min}:1`)
    }
  }
})

test('no colour value is written outside the tokens', () => {
  const root = new URL('../app/', import.meta.url)
  const files = readdirSync(root, { recursive: true }).filter(f => /\.(vue|ts)$/.test(f))
  // app.vue mirrors --cg-paper of each theme for the browser's theme-color.
  const allowed = { 'app.vue': ['#131418', '#FBFAF6'] }
  for (const f of files) {
    const src = readFileSync(new URL(f, root), 'utf8')
    const found = (src.match(/#[0-9A-Fa-f]{6}\b|#[0-9A-Fa-f]{3}\b(?![\w-])|\brgba?\(|\bhsla?\(|\b(?:bg|text|border|ring|fill|stroke)-(?:white|black|(?:red|orange|amber|yellow|green|emerald|blue|sky|indigo|violet|gray|zinc|stone|neutral|slate)-\d{2,3})\b/g) ?? [])
      .filter(v => !(allowed[f.split('/').pop()] ?? []).includes(v))
    assert.deepEqual(found, [], `${f} writes a colour value`)
  }
})

test('orange is never a default: the plain button is the outline one', () => {
  assert.deepEqual(ui.button.defaultVariants, { color: 'neutral', variant: 'outline' })
  for (const c of ['input', 'textarea', 'select', 'selectMenu', 'checkbox', 'radioGroup', 'switch', 'progress']) {
    assert.equal(ui[c].defaultVariants?.color, 'secondary', `${c} draws its checked and focused states in blue`)
  }
})
