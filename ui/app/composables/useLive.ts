import type { EventMatch, PanelEvent } from './useEvents'

export interface LiveOptions<T> {
  /** The events after which the data is fetched again. */
  on: EventMatch
  /**
   * Updates the data from the event itself; returning true skips the
   * refetch (system.status carries the whole status).
   */
  apply?: (ev: PanelEvent, data: Ref<T | null>) => boolean
  /** Polling interval while the event stream is down (0: none, as before). */
  fallbackMs?: number
  /** A slow poll while the stream is up, in case an event was missed. */
  safetyMs?: number
}

/** Coalesces a burst of events (a task's steps, a scan's resolutions). */
const DEBOUNCE_MS = 250
const SAFETY_MS = 5 * 60_000

/**
 * Loads `fetcher` now and again whenever an event says the data changed,
 * instead of on a timer. The old timer survives as the fallback while the
 * stream is down, and as a slow safety poll while it is up. A failed refresh
 * keeps the last good data.
 */
export function useLive<T>(fetcher: () => Promise<T>, opts: LiveOptions<T>) {
  const data = ref<T | null>(null) as Ref<T | null>
  const error = ref<string | null>(null)
  const pending = ref(true)
  const events = useEvents()

  async function refresh() {
    try {
      data.value = await fetcher()
      error.value = null
    } catch (e) {
      error.value = errorMessage(e)
    } finally {
      pending.value = false
    }
  }
  // What the panel fetches on its own (an event, a resync, the timer) is
  // not the person using it: it must not keep the session alive.
  const quietly = () => inBackground(refresh)
  const soon = useDebounceFn(quietly, DEBOUNCE_MS)

  events.on((ev) => {
    if (!matchesEvent(opts.on, ev)) return
    if (opts.apply?.(ev, data)) return
    void soon()
  })
  events.onResync(() => void soon())

  // While the stream is down, poll as the page did before it had events;
  // while it is up, only rarely.
  const interval = computed(() => events.down.value ? (opts.fallbackMs ?? 0) : (opts.safetyMs ?? SAFETY_MS))
  let timer: ReturnType<typeof setInterval> | undefined
  watch(interval, (ms) => {
    clearInterval(timer)
    timer = ms > 0 ? setInterval(() => void quietly(), ms) : undefined
  }, { immediate: true })
  onScopeDispose(() => clearInterval(timer))

  void refresh()
  return { data, error, pending, refresh }
}
