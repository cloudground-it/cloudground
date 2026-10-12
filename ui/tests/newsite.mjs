// Browser check of the new-site wizard (pages/sites/new.vue) and its
// summary, in every interface language at a desktop and a phone width,
// against a running panel such as the uidev server
// (internal/api/uidev_test.go):
//
//   PANEL_URL=http://127.0.0.1:5287 SHOTS=/tmp/shots \
//   PLAYWRIGHT=/path/to/node_modules/playwright/index.mjs node ui/tests/newsite.mjs
//
// For each language and width it creates a WordPress site through the
// wizard's five steps and checks: Next stops on an empty domain, the
// database name and user proposed from the domain, a database password
// with a quote refused on its field, a review without passwords, the
// summary with the chosen database and its password, the .txt download
// (the password and the note that the file holds secrets), Copy, that the
// site's GET never holds the password; then a static site asked for a
// database whose name is taken: Create goes back to the database step with
// the 409 on the name field. Once, a reload keeps the draft but not its
// passwords, and a summary closed (or the page left) unsaved asks first.
// The layout probe runs on every step and the summary. Optional: LOCALES,
// WIDTHS. It prints one JSON report and exits 1 when anything was found.
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

// The page's layout: a button label that wraps, text past its box, a
// control past the panel's edge.
function probe() {
  const found = []
  // The page's content column: the wizard or the summary and its header.
  const root = document.querySelector('[data-testid=site-wizard], [data-testid=site-summary]')?.parentElement
  if (!root) return ['no page']
  const dr = root.getBoundingClientRect()
  if (dr.right > window.innerWidth + 1 || dr.left < -1) found.push(`the page is past the viewport (${Math.round(dr.left)}..${Math.round(dr.right)})`)
  const label = el => (el.innerText || el.getAttribute('aria-label') || el.tagName).trim().replace(/\s+/g, ' ').slice(0, 40)
  for (const b of root.querySelectorAll('button')) {
    const text = b.querySelector('[data-slot=label]')
    if (!text || !text.innerText.trim()) continue
    const range = document.createRange()
    range.selectNodeContents(text)
    const lines = new Set([...range.getClientRects()].filter(r => r.width > 0).map(r => Math.round(r.top))).size
    if (lines > 1) found.push(`label wraps: ${label(b)}`)
  }
  for (const el of root.querySelectorAll('button, a, label, span, dt, dd, p, input, h2')) {
    const r = el.getBoundingClientRect()
    if (r.width === 0) continue
    if (r.right > dr.right + 1) found.push(`past the page: ${el.tagName.toLowerCase()} "${label(el)}" (${Math.round(r.right - dr.right)} px)`)
    const cs = getComputedStyle(el)
    if (el.tagName !== 'INPUT' && cs.display !== 'inline' && el.scrollWidth > el.clientWidth + 3 && cs.overflowX === 'visible') {
      found.push(`text past its box: "${label(el)}" (${el.scrollWidth - el.clientWidth} px)`)
    }
  }
  return [...new Set(found)]
}

// The proposal the wizard makes (ui/app/utils/newSite.ts proposeDBName,
// for the plain domains used here).
const proposed = d => d.split('.').slice(0, -1).join('_').replace(/[^a-z0-9_]+/g, '_')

// A screenshot of the whole page: the window grown so its body does not scroll.
async function shot(page, path) {
  const vp = page.viewportSize()
  await page.setViewportSize({ width: vp.width, height: 2200 })
  await page.waitForTimeout(150)
  await page.screenshot({ path })
  await page.setViewportSize(vp)
}

// Turns the database switch on.
async function switchOn(form) {
  const sw = form.getByRole('switch')
  if (await sw.getAttribute('aria-checked') !== 'true') await sw.click()
}

// The wizard's Next (Create on the review), and a pause for the step to render.
async function next(page) {
  await page.getByTestId('wizard-next').click()
  await page.waitForTimeout(150)
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
    const openWizard = async () => {
      // Every way in is a link to the page; the list's button is one.
      await page.goto(base + '/sites')
      await page.waitForLoadState('networkidle').catch(() => {})
      await page.getByRole('link', { name: msg(l, 'site.new') }).first().click()
      await page.waitForSelector('[data-testid=site-wizard]', { timeout: 10000 })
    }
    const form = page.getByTestId('site-wizard')
    const check = async (name) => {
      for (const f of await page.evaluate(probe)) fail(`${where} ${name}`, f)
      await shot(page, `${dir}/newsite-${name}.png`)
    }

    // 1. WordPress, the proposed names, a chosen password.
    await openWizard()
    await form.locator('input[data-type=wordpress]').check()
    await check('type')
    await next(page)
    await next(page)
    if (!(await form.innerText()).includes(msg(l, 'newSite.domainRequired'))) fail(where, 'Next went past an empty domain')
    const domain = `blog${l}${width}r${run}.example.com`
    await form.locator('input[placeholder="example.com"]').fill(domain)
    await check('domain')
    await next(page)
    await form.locator('input[type=email]').fill('owner@example.com')
    await form.locator('input[autocomplete=new-password]').fill('wordpress-admin-pass-1')
    await check('setup')
    await next(page)
    const names = form.locator('input[spellcheck=false]')
    const dbName = await names.nth(0).inputValue()
    const dbUser = await names.nth(1).inputValue()
    if (dbName !== proposed(domain) || dbUser !== proposed(domain)) fail(where, `proposed ${dbName}/${dbUser}, want ${proposed(domain)}`)
    // A password wp-cli cannot write into wp-config.php is refused here
    // already, as the server refuses it.
    const dbPw = form.locator('input[autocomplete=new-password]')
    await dbPw.fill(`it's quoted ${l} ${width}`)
    await next(page)
    if (!(await form.innerText()).includes(msg(l, 'newSite.dbPasswordChars'))) fail(where, 'a password with a quote is not refused on its field')
    const dbPassword = `chosen pw ${l} ${width}`
    await dbPw.fill(dbPassword)
    await check('database')
    await next(page)
    const review = await page.getByTestId('site-review').innerText().catch(() => '')
    for (const want of [domain, dbName, 'owner@example.com']) if (!review.includes(want)) fail(where, `the review lacks ${want}`)
    if (review.includes(dbPassword) || review.includes('wordpress-admin-pass-1')) fail(where, 'the review shows a password')
    await check('review')
    await next(page)
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
    await page.waitForURL(u => u.pathname === '/sites', { timeout: 5000 }).catch(() => fail(where, 'the saved summary did not close'))

    // 2. A static site with a database whose name is taken: Create goes
    // back to the database step, the 409 on the field.
    await openWizard()
    await form.locator('input[data-type=static]').check()
    await next(page)
    await form.locator('input[placeholder="example.com"]').fill(`static${l}${width}r${run}.example.com`)
    await next(page)
    await next(page)
    await switchOn(form)
    await form.locator('input[spellcheck=false]').nth(0).fill(dbName)
    await next(page)
    expect409 = true
    await next(page)
    await page.waitForTimeout(800)
    expect409 = false
    const formText = await form.innerText()
    if (!formText.includes(msg(l, 'newSite.nameTaken').replace('{name}', dbName))) fail(where, 'the 409 is not shown on the database name')
    if (await page.locator('[data-testid=site-summary]').count()) fail(where, 'a summary after a refused create')
    await shot(page, `${dir}/newsite-taken.png`)
    await form.getByRole('button', { name: msg(l, 'common.cancel') }).click()

    // 3. Once: a reload keeps the draft without its passwords; closing an
    // unsaved summary asks first.
    if (!guarded) {
      guarded = true
      await openWizard()
      await form.locator('input[data-type=static]').check()
      await next(page)
      await form.locator('input[placeholder="example.com"]').fill(`guard${width}r${run}.example.com`)
      await next(page)
      await next(page)
      await switchOn(form)
      await form.locator('input[spellcheck=false]').nth(0).fill(`guard_${run}`)
      await page.waitForTimeout(150)
      await page.reload()
      await page.waitForSelector('[data-testid=site-wizard]')
      await page.waitForTimeout(300)
      const stored = await page.evaluate(() => sessionStorage.getItem('cg:new-site') ?? '')
      if (!stored.includes(`guard${width}r${run}`) || /password/.test(stored)) fail(where, `the stored draft: ${stored}`)
      if ((await form.locator('input[spellcheck=false]').nth(0).inputValue().catch(() => '')) !== `guard_${run}`) fail(where, 'a reload lost the draft')
      await next(page)
      await next(page)
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
      await page.waitForURL(u => u.pathname === '/sites', { timeout: 5000 }).catch(() => fail(where, 'Close anyway did not close'))
    }
  }
  await ctx.close()
}
await browser.close()
console.log(JSON.stringify({ base, locales, widths, problems }, null, 2))
process.exit(problems.length ? 1 : 0)
