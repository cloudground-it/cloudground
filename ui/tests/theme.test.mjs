// The theme (app/app.config.ts) against regressions a reading of a page
// does not catch: one empty state at every size, and a tab list whose
// divider stays inside its own box. The config is loaded as it is (Node
// strips its types) with defineAppConfig provided. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

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
