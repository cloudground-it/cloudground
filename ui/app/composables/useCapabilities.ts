import type { Capabilities } from '~/types'

let loading: Promise<void> | null = null

/**
 * What the server offers — its PHP versions, the services it can restart —
 * as the agent finds it on the machine. Read once per session and shared by
 * every page, so no page keeps its own list of versions or services. Null
 * until the first answer arrives (and while the agent cannot answer).
 */
export function useCapabilities() {
  const api = useApi()
  const caps = useState<Capabilities | null>('capabilities', () => null)
  if (!caps.value && !loading) {
    loading = api.get<Capabilities>('/api/system/capabilities')
      .then((c) => { caps.value = c })
      .catch(() => {})
      .finally(() => { loading = null })
  }
  return caps
}
