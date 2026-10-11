let loading: string | null = null

/**
 * Every task kind in words in the interface's language, read from the
 * server (internal/i18n holds the one catalogue, shared with the alerts and
 * the webhook) once per language and shared by every component. Live
 * events carry only the kind, so this is how they get a name.
 */
export function useTaskNames() {
  const api = useApi()
  const names = useState<Record<string, string>>('taskNames', () => ({}))
  const loadedFor = useState<string>('taskNames:locale', () => '')
  function load() {
    const lang = formatLocale.value
    if (loadedFor.value === lang || loading === lang) return
    loading = lang
    // Sent by the panel on its own; a page outside a panel session (the
    // sign-in page, Quarry) must not be bounced to /login by it.
    api.get<Record<string, string>>('/api/tasks/names', { background: true, skipAuthRedirect: true })
      .then((n) => {
        if (formatLocale.value !== lang) return
        names.value = n
        loadedFor.value = lang
      })
      .catch(() => {})
      .finally(() => { if (loading === lang) loading = null })
  }
  load()
  /** kind in words; asks again when the language has changed since. */
  const label = (kind: string) => {
    if (loadedFor.value !== formatLocale.value) load()
    return taskKindLabel(names.value, kind)
  }
  return { names, label }
}
