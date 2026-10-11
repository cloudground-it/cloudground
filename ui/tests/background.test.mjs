// The requests the panel makes on its own carry X-CG-Background: 1, so the
// server does not count them as use of the session (internal/api/SPEC.md
// §2.2); what the person asks for does not. The composables are loaded as
// they are (Node strips their types) with Nuxt's auto-imports provided as
// globals and fetch recorded. Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { computed, effectScope, onScopeDispose, ref, watch } from 'vue'

const sent = []
globalThis.fetch = async (path, init) => {
  sent.push({ path, background: init.headers['X-CG-Background'] === '1' })
  return new Response('[]', { status: 200 })
}

const handlers = []
const resyncs = []
const events = { on: fn => handlers.push(fn), onResync: fn => resyncs.push(fn), down: ref(false) }

Object.assign(globalThis, {
  ref, computed, watch, onScopeDispose,
  useEvents: () => events,
  useDebounceFn: fn => fn,
  matchesEvent: () => true,
  formatLocale: ref('en'),
  useNuxtApp: () => ({}),
  useState: () => ref(null),
  navigateTo: async () => {}
})
Object.assign(globalThis, await import('../app/utils/api.ts'))
Object.assign(globalThis, await import('../app/utils/background.ts'))
const { useApi } = await import('../app/composables/useApi.ts')
const { useLive } = await import('../app/composables/useLive.ts')

const settle = (ms = 0) => new Promise(resolve => setTimeout(resolve, ms))
const last = path => sent.filter(r => r.path === path).at(-1)

test('the first load and an explicit refresh are the person\'s; events, resyncs and the timer are background', async () => {
  const api = useApi()
  const scope = effectScope()
  // The safety poll runs every 30 ms and may fire between any two steps on
  // a slow machine: each step looks at the request it made, by position.
  const start = sent.length
  const live = scope.run(() => useLive(() => api.get('/api/sites'), { on: { types: ['site.'] }, safetyMs: 30 }))
  try {
    await settle()
    assert.equal(sent[start]?.path, '/api/sites')
    assert.equal(sent[start].background, false, 'first load')

    let at = sent.length
    handlers.forEach(fn => fn({ type: 'site.changed' }))
    await settle()
    assert.ok(sent.length > at && sent.slice(at).every(r => r.background), 'refetch after an event')

    at = sent.length
    resyncs.forEach(fn => fn())
    await settle()
    assert.ok(sent.length > at && sent.slice(at).every(r => r.background), 'refetch after a resync')

    at = sent.length
    await settle(80)
    const polled = sent.slice(at).filter(r => r.path === '/api/sites')
    assert.ok(polled.length > 0, 'the safety poll ran')
    assert.ok(polled.every(r => r.background), 'the safety poll is background')

    at = sent.length
    const refreshed = live.refresh() // the request is built before the first await
    assert.equal(sent[at]?.background, false, 'an explicit refresh')
    await refreshed
  } finally {
    // A failed assertion must not leave the poll keeping the run alive.
    scope.stop()
  }
})

test('an async fetcher that awaits the API first (AlertsButton) is marked, and the mark ends with it', async () => {
  const api = useApi()
  await inBackground(async () => { await api.get('/api/system/alerts') })
  assert.equal(last('/api/system/alerts').background, true)
  await api.get('/api/system/alerts')
  assert.equal(last('/api/system/alerts').background, false)
})

test('a request can ask to be background itself (the task list)', async () => {
  const api = useApi()
  await api.get('/api/tasks', { background: true })
  assert.equal(last('/api/tasks').background, true)
})
