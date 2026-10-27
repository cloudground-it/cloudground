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

test('the demo shows what v0.5.0 to v0.8.0 added', () => {
  const demo = createDemo({ now: Date.parse('2026-10-27T10:00:00Z') })
  const get = path => demo.handle(req('GET', path))
  const ok = (path) => { const r = get(path); assert.equal(r.status, 200, path); return r.body }
  const sites = ok('/api/sites')
  const node = sites.find(s => s.type === 'node')
  const python = sites.find(s => s.type === 'python')
  assert.ok(node && python, 'a Node.js and a Python site')
  for (const s of [node, python]) {
    assert.equal(s.php_version, '')
    assert.ok(s.config.app.start && s.config.app.version)
    const app = ok(`/api/sites/${s.id}/app`)
    assert.ok(app.socket && Array.isArray(app.secrets) && app.secrets.length > 0 && Object.keys(app.env).length > 0)
    const status = ok(`/api/sites/${s.id}/app/status`)
    assert.equal(status.active, true)
    assert.equal(status.health.status, 200)
    assert.match(ok(`/api/sites/${s.id}/logs?source=app`).content, /GET \/health/)
  }
  assert.ok(ok(`/api/sites/${node.id}/app/status`).restarts > 0)
  // The panel is not asked for an app of a PHP site.
  assert.equal(get(`/api/sites/${sites.find(s => s.type === 'wordpress').id}/app`).status, 404)

  // Traffic: the days asked for, ending today, with a quiet weekend and totals that add up.
  for (const days of [7, 30, 90]) {
    const t = ok(`/api/sites/1/traffic?days=${days}`)
    assert.equal(t.days.length, days)
    assert.equal(t.days.at(-1).date, '2026-10-27')
    assert.equal(t.totals.requests, t.days.reduce((a, d) => a + d.requests, 0))
    assert.ok(t.totals.pageviews > t.totals.visitors && t.top_pages.length > 3 && t.top_not_found.length > 0 && t.hours.length > 0)
  }
  const week = ok('/api/sites/1/traffic?days=7').days
  const sat = week.find(d => new Date(d.date).getUTCDay() === 6)
  assert.ok(sat.requests < Math.max(...week.map(d => d.requests)))

  // Redirects with a canonical host; git deploy turned on with deliveries.
  const redirects = ok('/api/sites/1/redirects')
  assert.equal(redirects.canonical, 'shop.example.com')
  assert.ok(redirects.rules.length >= 2 && redirects.names.includes(redirects.canonical) && redirects.counterpart_served)
  const git = ok('/api/sites/4/git-deploy')
  assert.equal(git.enabled, true)
  assert.ok(git.deliveries.some(d => d.outcome === 'queued') && git.deliveries.some(d => d.outcome === 'ignored') && git.webhook_path)
  assert.equal(ok('/api/sites/3/git-deploy').enabled, false)
  assert.equal(ok('/api/sites/1/cloudflare').real_ip, true)
  assert.ok(ok('/api/sites/1/logs/files').files.length > 2)

  // Settings: external database servers, MCP on, passkeys, an assistant connected.
  const servers = ok('/api/db-servers')
  assert.deepEqual(servers.map(s => s.kind).sort(), ['mariadb', 'postgres'])
  assert.equal(ok('/api/settings').mcp, '1')
  assert.ok(ok('/api/mcp/grants').length > 0 && ok('/api/mcp/actions').some(a => a.status === 'pending'))
  assert.equal(ok('/api/session').user.passkeys, 1)
  assert.equal(ok('/api/account/passkeys').length, 1)
  // Backups: the databases a snapshot holds, for the selective restore.
  assert.deepEqual(ok('/api/sites/1/backups')[0].databases, ['site_1'])

  // None of it can be changed.
  for (const [method, path] of [['PUT', `/api/sites/${node.id}/app`], ['POST', `/api/sites/${node.id}/app/restart`],
    ['PUT', '/api/sites/1/redirects'], ['PUT', '/api/sites/4/git-deploy'], ['POST', '/api/db-servers'], ['DELETE', '/api/mcp/grants/1'], ['POST', '/api/backups/100/archive'], ['POST', '/api/backups/100/restore']]) {
    assert.equal(demo.handle(req(method, path, { body: {} })).status, 403, `${method} ${path}`)
  }
})

test('the demo shows v0.9.0\'s standalone functions', () => {
  const demo = createDemo({ now: Date.parse('2026-10-27T10:00:00Z') })
  const get = path => demo.handle(req('GET', path))
  const ok = (path) => { const r = get(path); assert.equal(r.status, 200, path); return r.body }

  const list = ok('/api/functions')
  assert.equal(list.domain, 'fn.example.com')
  assert.deepEqual(list.functions.map(f => f.name), ['contact-form', 'nightly-report', 'stripe-webhook'])
  assert.deepEqual(list.functions.map(f => f.runtime), ['node', 'python', 'node'])
  for (const f of list.functions) {
    assert.ok(f.urls[0] === `https://fn.example.com/${f.name}` && f.last && f.last.status && typeof f.has_token === 'boolean')
    const one = ok(`/api/functions/${f.name}`)
    assert.ok(one.files[one.entry], `${f.name} has its entry file`)
    assert.equal(one.release, one.releases[0])
    // Release IDs are ULIDs whose first ten characters are a time in the past.
    for (const id of one.releases) assert.match(id, /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/)
    const logs = ok(`/api/functions/${f.name}/logs`)
    assert.ok(logs.invocations.length >= 5 && logs.output.length > 0 && logs.invocations.every(i => i.path && i.source))
    assert.ok(Array.isArray(ok(`/api/functions/${f.name}/domains/status`).domains))
  }
  const contact = ok('/api/functions/contact-form')
  assert.ok(contact.files['package.json'] && contact.domains.includes('forms.example.com') && contact.auth === 'public')
  assert.equal(contact.urls.length, 2)
  assert.ok(contact.releases.length >= 3)
  const report = list.functions.find(f => f.name === 'nightly-report')
  assert.ok(report.schedule && report.auth === 'token' && report.has_token && !('token' in report))
  assert.equal(ok('/api/functions/contact-form/domains/status').domains[0].dns.status, 'here')
  const domain = ok('/api/functions/domain/status')
  assert.ok(domain.dns.status === 'here' && domain.certificate.exists && domain.acme_email)
  assert.equal(get('/api/functions/nope').status, 404)
  const rt = ok('/api/runtimes')
  assert.ok(rt.installed.node.length && rt.available.python.length)
  // Functions are no site's any more.
  assert.equal(get('/api/sites/6/functions').status, 404)
})

test('every change but this browser\'s own is refused, in the language asked', () => {
  const demo = createDemo()
  const writes = [
    ['POST', '/api/sites'], ['PATCH', '/api/sites/1'], ['DELETE', '/api/sites/1'], ['POST', '/api/sites/1/purge'], ['PUT', '/api/sites/1/maintenance'],
    ['POST', '/api/sites/1/backups'], ['PUT', '/api/settings'], ['POST', '/api/users'], ['DELETE', '/api/users/2'], ['POST', '/api/tokens'],
    ['POST', '/api/sites/1/files/upload'], ['PUT', '/api/sites/1/files'], ['POST', '/api/sites/1/database/query'], ['POST', '/api/system/update'],
    ['POST', '/api/firewall/rules'], ['POST', '/api/functions'], ['PUT', '/api/functions/contact-form'], ['POST', '/api/functions/contact-form/invoke'],
    ['PUT', '/api/functions/domain'], ['DELETE', '/api/functions/stripe-webhook'], ['POST', '/api/anything/at/all']
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
  assert.equal(demo.handle(req('GET', '/api/sites')).body.length, 7)
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
