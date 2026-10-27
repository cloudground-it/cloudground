// The demo build's plugin (#39): installs the demo's API before the panel's
// first request, and puts a strip with the demo's words at the foot of the
// page. Registered by nuxt.config.ts only when CG_DEMO=1, so the real
// panel's bundle never contains it.

import { installDemo } from './install.ts'
import { messagesFor } from './messages.ts'

// Room for the strip: the panel is a fixed layout that scrolls inside itself, so
// padding on the body would not help. The strip's height goes into
// --cg-demo-bar (it grows when the text wraps on a phone) and the layout stops
// above it: nothing of the page ever sits under the strip.
const RESERVE_CSS = 'html{--cg-demo-bar:0px}'
  + '#__nuxt>.fixed.inset-0{bottom:var(--cg-demo-bar)}'
  // The panels are min-h-svh: without this they would stay a screen tall and be clipped.
  + '#__nuxt>.fixed.inset-0>*{min-height:0}'
  + '#__nuxt ol.fixed[class*="z-[100]"]{margin-bottom:var(--cg-demo-bar)}'

function reserve(bar: HTMLElement) {
  const style = document.createElement('style')
  style.textContent = RESERVE_CSS
  document.head.append(style)
  const set = () => document.documentElement.style.setProperty('--cg-demo-bar', `${bar.getBoundingClientRect().height}px`)
  set()
  if (typeof ResizeObserver !== 'undefined') new ResizeObserver(set).observe(bar)
  window.addEventListener('resize', set)
}

export default defineNuxtPlugin({
  name: 'cloudground-demo',
  // Before every other plugin and the router's first navigation: the first
  // request the panel makes (the session) is already the demo's.
  enforce: 'pre',
  setup(nuxtApp) {
    installDemo(window)

    // The language is the i18n module's, which is set up after this plugin.
    nuxtApp.hook('app:mounted', () => {
      const i18n = nuxtApp.$i18n as { locale: { value: string } }
      const bar = document.createElement('div')
      bar.setAttribute('role', 'note')
      bar.style.cssText = 'position:fixed;left:0;right:0;bottom:0;z-index:2147483647;display:flex;flex-wrap:wrap;gap:.5rem 1rem;justify-content:center;align-items:center;padding:.5rem 1rem;'
        + 'font:500 13px/1.4 system-ui,sans-serif;background:#111827;color:#f9fafb;border-top:1px solid #374151'
      const text = document.createElement('span')
      const link = document.createElement('a')
      link.href = 'https://cloudground.it/'
      link.rel = 'noopener'
      link.style.cssText = 'color:#93c5fd;font-weight:600;text-decoration:underline'
      bar.append(text, link)
      const paint = () => {
        const m = messagesFor(i18n.locale.value)
        text.textContent = m.banner
        link.textContent = m.get
      }
      paint()
      watch(() => i18n.locale.value, paint)
      document.body.append(bar)
      reserve(bar)
    })
  }
})
