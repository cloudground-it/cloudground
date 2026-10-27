// Puts the demo's API in place of the network (#39).
//
// Nothing the page does can send a write anywhere, whatever the panel's code
// asks: every way a page has of making a request is replaced here.
//
//   fetch            /api/* is answered by the demo (api.ts); a GET of a file of
//                    this site (the page's own scripts, styles, fonts) goes to
//                    the real fetch; everything else, another origin or any
//                    method but GET and HEAD, is refused and never sent.
//   XMLHttpRequest   the uploads' way: answers the demo's refusal, sends nothing.
//   EventSource      /api/events opens and stays quiet; no other address opens.
//   WebSocket, sendBeacon   refused.
//
// The page cannot undo this: the replaced objects are the page's own, and the
// real ones are kept in this closure only. The page's Content-Security-Policy
// (nuxt.config.ts, demo build) says the same to the browser: same origin
// only, no form posts.

import { createDemo } from './api.ts'
import type { DemoResponse } from './api.ts'

interface Real {
  fetch: typeof fetch
}

/** The real functions a guard keeps for GETs of the site's own files; tests pass spies. */
export interface InstallOptions {
  real?: Real
  now?: number
}

const API = '/api/'

function refuse(): never {
  throw new TypeError('The demo sends nothing: a request to anywhere but its own files is refused.')
}

function jsonResponse(r: DemoResponse): Response {
  const body = r.status === 204 ? null : JSON.stringify(r.body)
  return new Response(body, { status: r.status, headers: { 'Content-Type': 'application/json' } })
}

function headerOf(init: RequestInit | undefined, input: unknown, name: string): string {
  const h = new Headers(init?.headers ?? (input instanceof Request ? input.headers : undefined))
  return h.get(name) ?? ''
}

async function bodyOf(init: RequestInit | undefined, input: unknown): Promise<unknown> {
  let raw: unknown = init?.body
  if (raw === undefined && input instanceof Request) raw = await input.clone().text()
  if (typeof raw !== 'string' || raw === '') return undefined
  try {
    return JSON.parse(raw)
  } catch {
    return undefined
  }
}

/** Replaces the page's ways of making requests; returns what it needs to be inspected. */
export function installDemo(win: Window & typeof globalThis, opts: InstallOptions = {}) {
  const demo = createDemo({ now: opts.now })
  const real: Real = opts.real ?? { fetch: win.fetch.bind(win) }
  const here = win.location.origin
  const lang = () => win.document?.documentElement?.lang || win.navigator?.language || 'en'

  win.fetch = async function demoFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const url = new URL(input instanceof Request ? input.url : String(input), win.location.href)
    const method = (init?.method ?? (input instanceof Request ? input.method : 'GET')).toUpperCase()
    if (url.origin === here && url.pathname.startsWith(API)) {
      const res = demo.handle({
        method, path: url.pathname, query: url.searchParams, body: await bodyOf(init, input),
        lang: headerOf(init, input, 'Accept-Language') || lang()
      })
      return jsonResponse(res)
    }
    // The page's own files only, and only to read them.
    if (url.origin === here && (method === 'GET' || method === 'HEAD') && !url.pathname.startsWith(API)) {
      return real.fetch(input, init)
    }
    return refuse()
  }

  // Uploads go through XMLHttpRequest: answered here, never opened.
  class DemoXHR extends EventTarget {
    readyState = 0
    status = 0
    statusText = ''
    responseText = ''
    response: unknown = ''
    responseType = ''
    timeout = 0
    withCredentials = false
    upload = new EventTarget()
    onload: ((e: Event) => void) | null = null
    onerror: ((e: Event) => void) | null = null
    onabort: ((e: Event) => void) | null = null
    onloadend: ((e: Event) => void) | null = null
    onreadystatechange: ((e: Event) => void) | null = null
    ontimeout: ((e: Event) => void) | null = null
    onprogress: ((e: Event) => void) | null = null
    private method = 'GET'
    private target = ''
    open(method: string, url: string | URL) {
      this.method = String(method).toUpperCase()
      this.target = String(url)
      this.readyState = 1
    }

    setRequestHeader() {}
    getResponseHeader(name: string) { return name.toLowerCase() === 'content-type' ? 'application/json' : null }
    getAllResponseHeaders() { return 'content-type: application/json\r\n' }
    abort() { this.fire('abort') }
    send() {
      const url = new URL(this.target, win.location.href)
      const res = url.origin === here && url.pathname.startsWith(API)
        ? demo.handle({ method: this.method, path: url.pathname, query: url.searchParams, body: undefined, lang: lang() })
        : { status: 403, body: { error: 'The demo sends nothing.' } }
      win.setTimeout(() => {
        this.status = res.status
        this.readyState = 4
        this.responseText = JSON.stringify(res.body)
        this.response = this.responseType === 'json' ? res.body : this.responseText
        this.fire('readystatechange')
        this.fire('load')
        this.fire('loadend')
      }, 0)
    }

    private fire(type: string) {
      const ev = new Event(type)
      this.dispatchEvent(ev)
      const handler = (this as unknown as Record<string, unknown>)['on' + type]
      if (typeof handler === 'function') handler.call(this, ev)
    }
  }
  win.XMLHttpRequest = DemoXHR as unknown as typeof XMLHttpRequest

  // The live event stream: open, and silent. No other address opens.
  class DemoEventSource extends EventTarget {
    static readonly CONNECTING = 0
    static readonly OPEN = 1
    static readonly CLOSED = 2
    readyState = 0
    url: string
    withCredentials = false
    onopen: ((e: Event) => void) | null = null
    onmessage: ((e: MessageEvent) => void) | null = null
    onerror: ((e: Event) => void) | null = null
    constructor(url: string | URL) {
      super()
      const u = new URL(String(url), win.location.href)
      if (u.origin !== here || u.pathname !== '/api/events') refuse()
      this.url = u.href
      win.setTimeout(() => {
        if (this.readyState === 2) return
        this.readyState = 1
        const ev = new Event('open')
        this.dispatchEvent(ev)
        this.onopen?.(ev)
      }, 0)
    }

    close() { this.readyState = 2 }
  }
  win.EventSource = DemoEventSource as unknown as typeof EventSource

  win.WebSocket = function DemoWebSocket() { return refuse() } as unknown as typeof WebSocket
  if (win.navigator) win.navigator.sendBeacon = () => false

  return { demo }
}
