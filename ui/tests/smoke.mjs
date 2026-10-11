// Browser smoke test of the panel: every page, in every interface language,
// at a desktop and a phone width. On each page it records console errors,
// uncaught exceptions, API answers of 400 and above, and the layout probe:
//
//   - a button whose label wraps onto a second line, or that is wider than
//     520 px: the regression long German labels cause;
//   - an element whose box runs past its container's right edge, or whose
//     text is cut without an ellipsis (text that does not fit);
//   - horizontal scrolling of the page at 390 px.
//
// It needs a running panel with data, such as the uidev server
// (internal/api/uidev_test.go, build tag uidev), and Playwright:
//
//   PANEL_URL=http://127.0.0.1:5287 SHOTS=/tmp/shots \
//   PLAYWRIGHT=/path/to/node_modules/playwright/index.mjs node ui/tests/smoke.mjs
//
// Optional: LOCALES=de,it (default all five), WIDTHS=390 (default 1440,390),
// PAGES=a substring to keep only the pages whose path contains it.
// PANEL_EMAIL / PANEL_PW default to the uidev account. It prints one JSON
// report and exits 1 when anything was found.
import { mkdirSync } from 'node:fs'

const { chromium } = await import(process.env.PLAYWRIGHT || 'playwright')
const base = (process.env.PANEL_URL || 'http://127.0.0.1:5252').replace(/\/$/, '')
const out = process.env.SHOTS || './smoke-shots'
const email = process.env.PANEL_EMAIL || 'admin@example.com'
const password = process.env.PANEL_PW || 'dev-password-123'
const locales = (process.env.LOCALES || 'en,it,fr,es,de').split(',')
const widths = (process.env.WIDTHS || '1440,390').split(',').map(Number)
const only = process.env.PAGES || ''
mkdirSync(out, { recursive: true })

const problems = []
const browser = await chromium.launch()

// The layout probe, run in the page. Returns one line per finding.
function probe(mobile) {
  const found = []
  const label = el => (el.innerText || el.getAttribute('aria-label') || el.getAttribute('title') || el.tagName).trim().replace(/\s+/g, ' ').slice(0, 40)
  const where = (el) => {
    const parts = []
    for (let e = el; e && e !== document.body && parts.length < 3; e = e.parentElement) {
      parts.unshift(e.tagName.toLowerCase() + (e.id ? '#' + e.id : ''))
    }
    return parts.join('>')
  }
  const visible = (el) => {
    const r = el.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) return false
    const cs = getComputedStyle(el)
    return cs.visibility !== 'hidden' && cs.display !== 'none'
  }
  // Inside a scroller (a table, the tab bar, a code block) running wide is
  // the design; inside a dialog's or menu's portal the page layout does not
  // apply.
  // The dashboard panel's body scrolls vertically (its computed overflow-x
  // is auto too): it is the page, not a scroller.
  const scrolls = (el) => {
    for (let e = el.parentElement; e && e !== document.body; e = e.parentElement) {
      if (e.getAttribute('data-slot') === 'body') return false
      const ox = getComputedStyle(e).overflowX
      if (ox === 'auto' || ox === 'scroll') return true
    }
    return false
  }

  // 1. Stretched controls: a button whose label wraps onto a second line
  // (the regression long German labels cause), or one wider than a control
  // should be. Full-width buttons are on purpose.
  const lines = (el) => {
    const range = document.createRange()
    range.selectNodeContents(el)
    return new Set([...range.getClientRects()].filter(r => r.width > 0).map(r => Math.round(r.top))).size
  }
  for (const b of document.querySelectorAll('[data-slot=base]')) {
    if (!visible(b) || !/^(BUTTON|A)$/.test(b.tagName)) continue
    // The label of a button, the shown value of a select.
    const text = b.querySelector('[data-slot=label], [data-slot=value]')
    if (!text?.innerText?.trim()) continue
    const r = b.getBoundingClientRect()
    if (lines(text) > 1) found.push(`label wraps: ${label(b)} ${Math.round(r.width)}x${Math.round(r.height)}`)
    if (r.width > 520 && !/\bw-full\b|\bflex-1\b/.test(b.className)) found.push(`stretched: ${label(b)} ${Math.round(r.width)}x${Math.round(r.height)}`)
  }

  // 2. Boxes past their container, and text cut without an ellipsis.
  const tags = 'button, a, label, th, td, dt, dd, h1, h2, h3, p, span, code, input, [role=radio], [role=tab]'
  for (const el of document.querySelectorAll(tags)) {
    // Text only (an icon in a select's padding is not a finding), and not
    // the screen-reader-only labels (a 1 px clipped box on purpose).
    if (!visible(el) || scrolls(el) || !(el.innerText || el.value || '').trim() || el.clientWidth <= 1) continue
    const parent = el.parentElement
    if (!parent) continue
    const r = el.getBoundingClientRect()
    const pr = parent.getBoundingClientRect()
    const pcs = getComputedStyle(parent)
    const padR = parseFloat(pcs.paddingRight) || 0
    if (pr.width > 0 && r.right > pr.right - padR + 2) {
      found.push(`overflows its container: ${where(el)} "${label(el)}" (${Math.round(r.right - pr.right)} px)`)
    }
    if (r.right > window.innerWidth + 1 && !el.closest('[data-reka-popper-content-wrapper], [role=dialog], [role=menu]')) {
      found.push(`past the viewport: ${where(el)} "${label(el)}" (${Math.round(r.right - window.innerWidth)} px)`)
    }
    const cs = getComputedStyle(el)
    if (el.tagName !== 'INPUT' && el.scrollWidth > el.clientWidth + 1 && el.clientWidth > 0 && cs.display !== 'inline') {
      if ((cs.overflowX === 'hidden' || cs.overflowX === 'clip') && cs.textOverflow !== 'ellipsis') {
        found.push(`text cut: ${where(el)} "${label(el)}" (${el.scrollWidth - el.clientWidth} px)`)
      } else if (cs.overflowX === 'visible' && el.scrollWidth > el.clientWidth + 3) {
        // A word longer than its box spills over what is next to it (the
        // HUD's letter-spacing after the last letter is not a finding).
        found.push(`text past its box: ${where(el)} "${label(el)}" (${el.scrollWidth - el.clientWidth} px)`)
      }
    }
  }

  // 3. The page itself scrolls sideways: the document, or a panel's body.
  const sw = document.documentElement.scrollWidth
  if (sw > window.innerWidth + 1) found.push(`horizontal scroll: page is ${sw} px at ${window.innerWidth} px`)
  for (const body of document.querySelectorAll('[data-slot=body]')) {
    if (visible(body) && body.scrollWidth > body.clientWidth + 1) found.push(`horizontal scroll: the panel body is ${body.scrollWidth} px in ${body.clientWidth} px`)
  }
  return [...new Set(found)]
}

async function signIn(page) {
  await page.goto(base + '/login')
  await page.waitForSelector('input[type=email]', { timeout: 20000 })
  await page.fill('input[type=email]', email)
  await page.fill('input[type=password]', password)
  await page.click('button[type=submit]')
  await page.waitForURL(u => !u.pathname.startsWith('/login'), { timeout: 20000 })
}

async function pagesOf(page) {
  const sites = await page.evaluate(async () => (await (await fetch('/api/sites')).json()))
  const shop = sites.find(s => s.php_version && !s.staging_of) || sites[0]
  const plain = sites.find(s => !s.php_version && !s.staging_of)
  const list = ['/', '/sites', '/services', '/firewall', '/logs', '/users', '/audit', '/security', '/settings']
  if (shop) {
    for (const tab of ['', 'cache', 'wordpress', 'staging', 'deployments', 'backups', 'ssl', 'database',
      'database/wordpress', 'files', 'cron', 'php', 'sftp', 'vhost', 'migration', 'logs']) {
      list.push(`/sites/${shop.id}${tab ? '/' + tab : ''}`)
    }
  }
  if (plain) list.push(`/sites/${plain.id}`, `/sites/${plain.id}/backups`, `/sites/${plain.id}/deployments`)
  return list.filter(p => p.includes(only))
}

// Quarry (/quarry), opened from the panel through a handoff for the
// shop's database: its database and table pages, every tab.
async function studioPagesOf(page) {
  const sites = await page.evaluate(async () => (await (await fetch('/api/sites')).json()))
  const shop = sites.find(s => s.php_version && !s.staging_of)
  if (!shop) return { shop: null, list: [] }
  const db = shop.config?.database?.name || 'site_2'
  const list = [`/quarry/db/${db}`]
  for (const tab of ['sql', 'search', 'export', 'import']) list.push(`/quarry/db/${db}?tab=${tab}`)
  for (const tab of ['browse', 'structure', 'sql', 'search', 'insert', 'export', 'import', 'operations']) list.push(`/quarry/db/${db}/wp_posts?tab=${tab}`)
  return { shop, list: list.filter(p => p.includes(only)) }
}

// openStudio exchanges a handoff the panel mints, as the Database tab does.
async function openStudio(page, shop) {
  const r = await page.evaluate(async id => (await (await fetch(`/api/sites/${id}/quarry/handoff`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}'
  })).json()), shop.id)
  await page.goto(base + r.url)
  await page.waitForURL(u => u.pathname.startsWith('/quarry/db'), { timeout: 20000 })
}

// The panel's and Quarry's bodies scroll inside the window: grow the
// window to the body's height so the screenshot holds the whole page.
async function shoot(page, file) {
  const tall = await page.evaluate(() => Math.max(...[...document.querySelectorAll('[data-slot=body]')].map(b => b.scrollHeight - b.clientHeight), 0))
  const vp = page.viewportSize()
  if (tall > 0) await page.setViewportSize({ width: vp.width, height: Math.min(vp.height + tall, 12000) })
  await page.screenshot({ path: file, fullPage: true })
  if (tall > 0) await page.setViewportSize(vp)
}

const name = p => (p === '/' ? 'dashboard' : p.slice(1).replace(/\//g, '-')) || 'dashboard'

for (const width of widths) {
  const mobile = width < 640
  const ctx = await browser.newContext({
    viewport: { width, height: mobile ? 844 : 900 },
    deviceScaleFactor: 1,
    isMobile: mobile,
    hasTouch: mobile,
    colorScheme: 'light'
  })
  const page = await ctx.newPage()
  let where = 'login'
  page.on('console', (m) => {
    if (m.type() === 'error' || /Refused to|Content Security Policy/i.test(m.text())) problems.push(`[${width} ${where}] console: ${m.text()}`)
  })
  page.on('pageerror', e => problems.push(`[${width} ${where}] pageerror: ${e.message}`))
  page.on('response', (r) => {
    if (r.url().includes('/api/') && r.status() >= 400) problems.push(`[${width} ${where}] ${r.status()} ${r.request().method()} ${r.url().replace(base, '')}`)
  })

  await signIn(page)
  const paths = await pagesOf(page)
  const studio = await studioPagesOf(page)
  for (const locale of locales) {
    where = `${locale} profile`
    // The language is the user's: saved in the profile, applied at load.
    const status = await page.evaluate(async code => (await fetch('/api/account/profile', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ locale: code })
    })).status, locale)
    if (status !== 200) problems.push(`[${width}] saving the locale ${locale} answered ${status}`)
    const dir = `${out}/${locale}-${width}`
    mkdirSync(dir, { recursive: true })
    for (const path of paths) {
      where = `${locale} ${path}`
      await page.goto(base + path)
      await page.waitForLoadState('networkidle', { timeout: 15000 }).catch(() => {})
      await page.waitForTimeout(500)
      const lang = await page.evaluate(() => document.documentElement.lang)
      if (!lang.startsWith(locale)) problems.push(`[${width} ${where}] html lang is ${lang}`)
      for (const f of await page.evaluate(probe, mobile)) problems.push(`[${width} ${where}] ${f}`)
      await shoot(page, `${dir}/${name(path)}.png`)
    }
    if (studio.shop && studio.list.length) {
      where = `${locale} dbstudio handoff`
      await openStudio(page, studio.shop)
      for (const path of studio.list) {
        where = `${locale} ${path}`
        await page.goto(base + path)
        await page.waitForLoadState('networkidle', { timeout: 15000 }).catch(() => {})
        await page.waitForTimeout(500)
        for (const f of await page.evaluate(probe, mobile)) problems.push(`[${width} ${where}] ${f}`)
        await shoot(page, `${dir}/${name(path).replace(/[?=]/g, '-')}.png`)
      }
    }
  }
  // The sign-in page, signed out.
  where = 'login'
  await ctx.clearCookies()
  await page.goto(base + '/login')
  await page.waitForSelector('input[type=email]', { timeout: 20000 })
  for (const f of await page.evaluate(probe, mobile)) problems.push(`[${width} login] ${f}`)
  await page.screenshot({ path: `${out}/login-${width}.png`, fullPage: true })
  // Quarry's own sign-in, signed out of both.
  where = 'dbstudio sign-in'
  await page.goto(base + '/quarry')
  await page.waitForSelector('input[type=password]', { timeout: 20000 })
  for (const f of await page.evaluate(probe, mobile)) problems.push(`[${width} dbstudio sign-in] ${f}`)
  await page.screenshot({ path: `${out}/dbstudio-signin-${width}.png`, fullPage: true })
  await ctx.close()
}
await browser.close()

console.log(JSON.stringify({ base, locales, widths, problems }, null, 2))
process.exit(problems.length ? 1 : 0)
