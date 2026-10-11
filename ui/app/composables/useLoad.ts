/**
 * Loads `fetcher` once, for data no event invalidates (an audit page, a
 * table's structure). Data that changes while a page is open uses useLive,
 * which reloads it when an event says so. A failed refresh keeps the last
 * good data, so a transient error does not blank a page someone is reading.
 */
export function useLoad<T>(fetcher: () => Promise<T>) {
  const data = ref<T | null>(null) as Ref<T | null>
  const error = ref<string | null>(null)
  const pending = ref(true)

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

  void refresh()
  return { data, error, pending, refresh }
}
