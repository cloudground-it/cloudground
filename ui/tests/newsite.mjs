// Browser check of the new-site dialog and its summary (Fase 9b/2), in
// every interface language at a desktop and a phone width, against a
// running panel such as the uidev server (internal/api/uidev_test.go):
//
//   PANEL_URL=http://127.0.0.1:5287 SHOTS=/tmp/shots \
//   PLAYWRIGHT=/path/to/node_modules/playwright/index.mjs node ui/tests/newsite.mjs
//
// For each language and width it creates a WordPress site through the
// dialog and checks: the database name and user proposed from the domain,
// the summary with the chosen database and its password, the .txt download
// (the password and the note that the file holds secrets), Copy, that the
// site's GET never holds the password; then a static site asked for a
// database whose name is taken: the 409 shows on the name field. A
// database password with a quote is refused on its field. Once, a summary
// closed (or the page left) unsaved asks first. The layout probe of smoke.mjs runs on
// the form and the summary. Optional: LOCALES, WIDTHS. It prints one JSON
// report and exits 1 when anything was found.
import { mkdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const { chromium } = await import(process.env.PLAYWRIGHT || 'playwright')
const base = (process.env.PANEL_URL || 'http://127.0.0.1:5252').replace(/\/$/, '')
const out = process.env.SHOTS || './newsite-shots'
const email = process.env.PANEL_EMAIL || 'admin@example.com'
const password = process.env.PANEL_PW || 'dev-password-123'
const locales = (process.env.LOCALES || 'en,it,fr,es,de').split(',')
const widths = (process.env.WIDTHS || '1440,390').split(',').map(Number)
mkdirSync(out, { recursive: true })

const messages = Object.fromEntries(locales.map(l => [l, JSON.parse(readFileSync(fileURLToPath(new URL(`../i18n/locales/${l}.json`, import.meta.url)), 'utf8'))]))
const msg = (l, key) => key.split('.').reduce((o, k) => o?.[k], messages[l]) ?? key

const problems = []
// Domains of this run: the panel keeps the sites of an earlier one.
const run = String(Date.now() % 1000000)
const fail = (where, what) => problems.push(`[${where}] ${what}`)

// The dialog's layout: a button label that wraps, text past its box, a
// control past the dialog's edge.
function probe() {
  const found = []
  const dialog = document.querySelector('[role=dialog]')
  if (!dialog) return ['no dialog']
  const dr = dialog.getBoundingClientRect()
  if (dr.right > window.innerWidth + 1 || dr.left < -1) found.push(`the dialog is past the viewport (${Math.round(dr.left)}..${Math.round(dr.right)})`)
  const label = el => (el.innerText || el.getAttribute('aria-label') || el.tagName).trim().replace(/\s+/g, ' ').slice(0, 40)
  for (const b of dialog.querySelectorAll('button')) {
    const text = b.querySelector('[data-slot=label]')
    if (!text || !text.innerText.trim()) continue
    const range = document.createRange()
    range.selectNodeContents(text)
    const lines = new Set([...range.getClientRects()].filter(r => r.width > 0).map(r => Math.round(r.top))).size
    if (lines > 1) found.push(`label wraps: ${label(b)}`)
  }
  for (const el of dialog.querySelectorAll('button, a, label, span, dt, dd, p, input, h2')) {
    const r = el.getBoundingClientRect()
    if (r.width === 0) continue
    if (r.right > dr.right + 1) found.push(`past the dialog: ${el.tagName.toLowerCase()} "${label(el)}" (${Math.round(r.right - dr.right)} px)`)
    const cs = getComputedStyle(el)
    if (el.tagName !== 'INPUT' && cs.display !== 'inline' && el.scrollWidth > el.clientWidth + 3 && cs.overflowX === 'visible') {
      found.push(`text past its box: "${label(el)}" (${el.scrollWidth - el.clientWidth} px)`)
    }
  }
  return [...new Set(found)]
}

// The proposal the dialog makes (ui/app/utils/newSite.ts proposeDBName,
// for the plain domains used here).
const proposed = d => d.split('.').slice(0, -1).join('_').replace(/[^a-z0-9_]+/g, '_')

// A screenshot of the whole dialog: the window grown so its body does not scroll.
async function shot(page, path) {
  const vp = page.viewportSize()
  await page.setViewportSize({ width: vp.width, height: 2200 })
  await page.waitForTimeout(150)
  await page.screenshot({ path })
  await page.setViewportSize(vp)
}

// Turns the dialog's database switch on (it keeps its state between openings).
async function switchOn(dialog) {
  const sw = dialog.getByRole('switch')
  if (await sw.getAttribute('aria-checked') !== 'true') await sw.click()
}

const browser = await chromium.launch()
let guarded = false
for (const width of widths) {
  const mobile = width < 640
  const ctx = await browser.newContext({ viewport: { width, height: mobile ? 844 : 900 }, isMobile: mobile, hasTouch: mobile, acceptDownloads: true })
  await ctx.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: base })
  const page = await ctx.newPage()
  let where = `${width} login`
  let expect409 = false
  page.on('pageerror', e => fail(where, `pageerror: ${e.message}`))
  page.on('console', (m) => { if (m.type() === 'error' && !(expect409 && /409/.test(m.text()))) fail(where, `console: ${m.text()}`) })
  page.on('response', (r) => {
    if (!r.url().includes('/api/') || r.status() < 400) return
    if (expect409 && r.status() === 409 && r.url().endsWith('/api/sites')) return
    fail(where, `${r.status()} ${r.request().method()} ${r.url().replace(base, '')}`)
  })
  await page.goto(base + '/login')
  await page.fill('input[type=email]', email)
  await page.fill('input[type=password]', password)
  await page.click('button[type=submit]')
  await page.waitForURL(u => !u.pathname.startsWith('/login'), { timeout: 20000 })

  for (const l of locales) {
    where = `${width} ${l}`
    const dir = `${out}/${l}-${width}`
    mkdirSync(dir, { recursive: true })
    await page.evaluate(async code => fetch('/api/account/profile', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ locale: code }) }), l)
    await page.goto(base + '/sites')
    await page.waitForLoadState('networkidle').catch(() => {})
    const openDialog = async () => {
      await page.getByRole('button', { name: msg(l, 'site.new') }).first().click()
      await page.waitForSelector('[role=dialog] form#create-site', { timeout: 10000 })
    }

    // 1. WordPress, the proposed names, a chosen password.
    await openDialog()
    const dialog = page.locator('[role=dialog]')
    await dialog.getByRole('radio', { name: new RegExp('^' + msg(l, 'site.type.wordpress')) }).click()
    const domain = `blog${l}${width}r${run}.example.com`
    await dialog.locator('input[placeholder="example.com"]').fill(domain)
    const names = dialog.locator('input[spellcheck=false]')
    const dbName = await names.nth(1).inputValue()
    const dbUser = await names.nth(2).inputValue()
    if (dbName !== proposed(domain) || dbUser !== proposed(domain)) fail(where, `proposed ${dbName}/${dbUser}, want ${proposed(domain)}`)
    await dialog.locator('input[type=email]').fill('owner@example.com')
    const pwInputs = dialog.locator('input[autocomplete=new-password]')
    await pwInputs.nth(0).fill('wordpress-admin-pass-1')
    // A password wp-cli cannot write into wp-config.php is refused here
    // already, as the server refuses it.
    await pwInputs.nth(1).fill(`it's quoted ${l} ${width}`)
    await dialog.locator('button[type=submit]').click()
    if (!(await dialog.innerText()).includes(msg(l, 'newSite.dbPasswordChars'))) fail(where, 'a password with a quote is not refused on its field')
    const dbPassword = `chosen pw ${l} ${width}`
    await pwInputs.nth(1).fill(dbPassword)
    for (const f of await page.evaluate(probe)) fail(`${where} form`, f)
    await shot(page, `${dir}/newsite-form.png`)
    await dialog.locator('button[type=submit]').click()
    await page.waitForSelector('[data-testid=site-summary]', { timeout: 15000 }).catch(() => fail(where, 'no summary after the create'))
    await page.waitForFunction(t => document.querySelector('[data-testid=site-summary]')?.textContent.includes(t), msg(l, 'newSite.step.done'), { timeout: 15000 })
      .catch(() => fail(where, 'the creation did not show as done'))
    const summary = await page.locator('[data-testid=site-summary]').innerText()
    for (const want of [dbName, dbPassword, `https://${domain}/wp-admin/`, '127.0.0.1', '3306', '/run/mysqld/mysqld.sock', 'admin']) {
      if (!summary.includes(want)) fail(where, `the summary lacks ${want}`)
    }
    for (const f of await page.evaluate(probe)) fail(`${where} summary`, f)
    await shot(page, `${dir}/newsite-summary.png`)

    const [dl] = await Promise.all([page.waitForEvent('download'), page.getByTestId('summary-download').click()])
    const txt = readFileSync(await dl.path(), 'utf8')
    if (!dl.suggestedFilename().endsWith('.txt')) fail(where, `download named ${dl.suggestedFilename()}`)
    for (const want of [dbPassword, dbName, msg(l, 'newSite.txtSecret'), `https://${domain}/`]) {
      if (!txt.includes(want)) fail(where, `the .txt lacks ${want}`)
    }
    await page.getByTestId('summary-copy').click()
    const clip = await page.evaluate(() => navigator.clipboard.readText()).catch(e => `(${e})`)
    if (clip !== txt) fail(where, 'Copy did not put the summary on the clipboard')
    const id = await page.evaluate(async d => (await (await fetch('/api/sites')).json()).find(s => s.domain === d)?.id, domain)
    const got = await page.evaluate(async i => (await fetch(`/api/sites/${i}`)).text(), id)
    if (!id || got.includes(dbPassword)) fail(where, 'GET /api/sites/{id} holds the password (or no site)')
    await page.getByTestId('summary-close').click()
    await page.waitForSelector('[role=dialog]', { state: 'detached', timeout: 5000 }).catch(() => fail(where, 'the saved summary did not close'))

    // 2. A static site with a database whose name is taken: 409 on the field.
    await openDialog()
    await dialog.locator('input[placeholder="example.com"]').fill(`static${l}${width}r${run}.example.com`)
    await dialog.getByRole('radio', { name: new RegExp(msg(l, 'site.type.static')) }).click()
    await switchOn(dialog)
    await dialog.locator('input[spellcheck=false]').nth(1).fill(dbName)
    expect409 = true
    await dialog.locator('button[type=submit]').click()
    await page.waitForTimeout(800)
    expect409 = false
    const formText = await dialog.innerText()
    if (!formText.includes(msg(l, 'newSite.nameTaken').replace('{name}', dbName))) fail(where, 'the 409 is not shown on the database name')
    if (await page.locator('[data-testid=site-summary]').count()) fail(where, 'a summary after a refused create')
    await shot(page, `${dir}/newsite-taken.png`)
    await dialog.getByRole('button', { name: msg(l, 'common.cancel') }).click()

    // 3. Once: closing an unsaved summary asks first.
    if (!guarded) {
      guarded = true
      await openDialog()
      await dialog.locator('input[placeholder="example.com"]').fill(`guard${width}r${run}.example.com`)
      await dialog.getByRole('radio', { name: new RegExp(msg(l, 'site.type.static')) }).click()
      await switchOn(dialog)
      await dialog.locator('input[spellcheck=false]').nth(1).fill(`guard_${run}`)
      await dialog.locator('button[type=submit]').click()
      await page.waitForSelector('[data-testid=site-summary]', { timeout: 15000 })
      // Reloading or leaving the page asks too (beforeunload).
      const held = await page.evaluate(() => {
        const ev = new Event('beforeunload', { cancelable: true })
        window.dispatchEvent(ev)
        return ev.defaultPrevented
      })
      if (!held) fail(where, 'leaving the page with an unsaved summary does not ask')
      await page.getByTestId('summary-close').click()
      const asked = await page.getByText(msg(l, 'newSite.closeTitle')).isVisible().catch(() => false)
      if (!asked) fail(where, 'closing an unsaved summary did not ask')
      await page.getByRole('button', { name: msg(l, 'newSite.closeAnyway') }).click()
      await page.waitForSelector('[data-testid=site-summary]', { state: 'detached', timeout: 5000 }).catch(() => fail(where, 'Close anyway did not close'))
    }
  }
  await ctx.close()
}
await browser.close()
console.log(JSON.stringify({ base, locales, widths, problems }, null, 2))
process.exit(problems.length ? 1 : 0)
