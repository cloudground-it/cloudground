/**
 * The panel's live event stream (GET /api/events, internal/api/events/SPEC.md):
 * one EventSource for the whole app, reconnected with backoff, resumed from
 * the last event it saw. Views subscribe with `on` and refetch what an event
 * says changed; `onResync` fires when the stream could not be resumed, so
 * everything shown must be reloaded.
 */

export interface PanelEvent<T = Record<string, unknown>> {
  /** SSE id; absent on ping and resync. */
  id?: string
  type: string
  /** The site the event is about; absent on server-wide events. */
  site_id?: number
  at: string
  data?: T
}

type Handler = (ev: PanelEvent) => void
export type StreamStatus = 'idle' | 'connecting' | 'open' | 'down'

// One stream per tab: module state, not per component.
const status = ref<StreamStatus>('idle')
const handlers = new Set<Handler>()
const resyncHandlers = new Set<() => void>()
let source: EventSource | null = null
let lastId = ''
let wanted = false
let failedSinceOpen = false
let backoff = 1000
let retryTimer: ReturnType<typeof setTimeout> | undefined
let watchdog: ReturnType<typeof setTimeout> | undefined

/** The server sends a ping every 25 s; nothing for this long means a dead stream. */
const SILENCE_MS = 60_000
const MAX_BACKOFF_MS = 30_000

function emit(ev: PanelEvent) {
  if (ev.type === 'resync') {
    for (const fn of [...resyncHandlers]) fn()
    return
  }
  for (const fn of [...handlers]) fn(ev)
}

function armWatchdog() {
  clearTimeout(watchdog)
  watchdog = setTimeout(() => fail(), SILENCE_MS)
}

function connect() {
  if (!wanted || source) return
  status.value = status.value === 'down' ? 'down' : 'connecting'
  // A new EventSource cannot send Last-Event-ID itself: the server takes it
  // from the query too.
  const url = lastId ? `/api/events?last_event_id=${encodeURIComponent(lastId)}` : '/api/events'
  const es = new EventSource(url)
  source = es
  armWatchdog()
  es.onopen = () => {
    const recovered = failedSinceOpen
    failedSinceOpen = false
    status.value = 'open'
    backoff = 1000
    armWatchdog()
    // Back after a gap with nothing to resume from: whatever is on screen
    // may be stale.
    if (recovered && !lastId) emit({ type: 'resync', at: new Date().toISOString() })
  }
  es.onmessage = (msg: MessageEvent) => {
    armWatchdog()
    if (msg.lastEventId) lastId = msg.lastEventId
    let ev: PanelEvent
    try {
      ev = JSON.parse(msg.data)
    } catch {
      return
    }
    if (ev.type === 'ping') return
    if (msg.lastEventId) ev.id = msg.lastEventId
    emit(ev)
  }
  // The browser would retry on its own, without the ID and without backoff:
  // take over instead.
  es.onerror = () => fail()
}

function fail() {
  clearTimeout(watchdog)
  source?.close()
  source = null
  if (!wanted) return
  failedSinceOpen = true
  status.value = 'down'
  clearTimeout(retryTimer)
  const delay = backoff * (0.75 + Math.random() * 0.5)
  backoff = Math.min(backoff * 2, MAX_BACKOFF_MS)
  retryTimer = setTimeout(connect, delay)
}

function start() {
  if (wanted) return
  wanted = true
  backoff = 1000
  connect()
}

function stop() {
  wanted = false
  clearTimeout(retryTimer)
  clearTimeout(watchdog)
  source?.close()
  source = null
  lastId = ''
  failedSinceOpen = false
  status.value = 'idle'
}

/** Registers fn until the calling component's scope ends (or off is called). */
function on(fn: Handler): () => void {
  handlers.add(fn)
  const off = () => handlers.delete(fn)
  if (getCurrentScope()) onScopeDispose(off)
  return off
}

function onResync(fn: () => void): () => void {
  resyncHandlers.add(fn)
  const off = () => resyncHandlers.delete(fn)
  if (getCurrentScope()) onScopeDispose(off)
  return off
}

export function useEvents() {
  return {
    status: readonly(status),
    /** True while the stream is not delivering (after a failure, until it reconnects). */
    down: computed(() => status.value === 'down'),
    start,
    stop,
    on,
    onResync
  }
}

/**
 * Which events invalidate a view: event types (a trailing dot matches a
 * prefix, `backup.`), optionally limited to some sites.
 */
export interface EventMatch {
  types: string[]
  /** Only events about these sites (and never server-wide ones). */
  sites?: () => number | number[] | null | undefined
  /** A further test on the event. */
  where?: (ev: PanelEvent) => boolean
}

export function matchesEvent(m: EventMatch, ev: PanelEvent): boolean {
  if (!m.types.some(t => t.endsWith('.') ? ev.type.startsWith(t) : ev.type === t)) return false
  if (m.sites) {
    const want = m.sites()
    const ids = want == null ? [] : Array.isArray(want) ? want : [want]
    if (!ev.site_id || !ids.includes(ev.site_id)) return false
  }
  return m.where ? m.where(ev) : true
}
