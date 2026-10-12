// https://nuxt.com/docs/api/configuration/nuxt-config
import { fileURLToPath } from 'node:url'

const apiOrigin = process.env.CLOUDGROUND_API_ORIGIN || 'http://127.0.0.1:5252'

export default defineNuxtConfig({
  modules: ['@nuxt/ui', '@vueuse/nuxt', '@nuxtjs/i18n'],

  // A single-page app embedded in the panel-api binary (ui/embed.go) and
  // served from the same origin as the API.
  ssr: false,

  devtools: { enabled: false },

  app: {
    head: {
      title: 'CloudGround',
      meta: [{ name: 'robots', content: 'noindex, nofollow' }],
      // The mark (scripts/brand-assets.py): filled for the tab, the app tile
      // for a home screen.
      link: [
        { rel: 'icon', href: '/favicon.svg', type: 'image/svg+xml' },
        { rel: 'icon', href: '/icon-192.png', type: 'image/png', sizes: '192x192' },
        { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' }
      ]
    }
  },

  css: ['~/assets/css/main.css'],

  compatibilityDate: '2026-06-30',

  // No build manifest: it is fetched from /_nuxt/builds at runtime to detect
  // new deploys, which an embedded panel never needs, and its random build
  // id made every build differ from the committed one.
  experimental: { appManifest: false },

  // A release is reproducible (internal/version SPEC, Makefile `release`):
  // the same commit must give the same bytes. Nuxt otherwise writes a
  // random build id into every HTML shell; nothing reads it with the app
  // manifest off.
  buildId: 'cloudground',

  nitro: {
    preset: 'static',
    // go:embed reads ui/dist; it cannot follow the symlink nuxt generate
    // would otherwise leave there.
    output: { publicDir: fileURLToPath(new URL('./dist', import.meta.url)) },
    // One app shell is enough: the panel serves it for every route.
    prerender: { crawlLinks: false, routes: ['/'] },
    // The shells' payload carries the time they were rendered
    // (prerenderedAt), which made two builds of one commit differ. It is
    // pinned to 1: still truthy, which is all the client checks.
    hooks: {
      'prerender:generate'(route) {
        if (typeof route.contents === 'string') {
          route.contents = route.contents.replace(/("prerenderedAt":1,"serverRendered":2\},)\d+/, (_, head: string) => head + '1')
        }
      }
    },
    devProxy: {
      '/api': { target: `${apiOrigin}/api`, changeOrigin: true }
    }
  },

  // The panel fronts a root-capable agent: nothing on its pages may load from
  // a third party. The fonts are self-hosted woff2 files declared in
  // main.css (so @nuxt/fonts stays off), and icons ship in the bundle
  // (there is no Nitro server to answer /api/_nuxt_icon).
  ui: { fonts: false },

  // Every icon is from the CloudGround set (app/assets/icons/README.md) or,
  // for the software a site type installs, the marks in
  // app/assets/icons/brand (README there): local collections bundled whole
  // into the client. provider 'none' stubs
  // Iconify's fetch, so nothing is ever requested from api.iconify.design.
  icon: {
    mode: 'svg',
    provider: 'none',
    fallbackToApi: false,
    serverBundle: false,
    customCollections: [
      { prefix: 'cg', dir: fileURLToPath(new URL('./app/assets/icons/cg', import.meta.url)) },
      { prefix: 'brand', dir: fileURLToPath(new URL('./app/assets/icons/brand', import.meta.url)) }
    ],
    clientBundle: {
      scan: { globInclude: ['**/*.{vue,ts}'], globExclude: ['node_modules', 'dist', '.nuxt', '.output'] },
      includeCustomCollections: true
    }
  },

  i18n: {
    strategy: 'no_prefix',
    defaultLocale: 'it',
    langDir: 'locales',
    locales: [
      { code: 'it', language: 'it-IT', name: 'Italiano', file: 'it.json' },
      { code: 'en', language: 'en-US', name: 'English', file: 'en.json' },
      { code: 'fr', language: 'fr-FR', name: 'Français', file: 'fr.json' },
      { code: 'es', language: 'es-ES', name: 'Español', file: 'es.json' },
      { code: 'de', language: 'de-DE', name: 'Deutsch', file: 'de.json' }
    ],
    // Before sign-in the browser's language picks among these (Italian when
    // it is none of them); once signed in, the user's saved choice wins
    // (useAuth.fetchMe).
    detectBrowserLanguage: { useCookie: true, cookieKey: 'cg_locale', redirectOn: 'root' }
  }
})
