import type { Task } from '~/types'
import type { PanelEvent } from './useEvents'

const KEEP = 100
let wired = false

/**
 * The recent tasks, kept live from the event stream: the list is loaded
 * once, then task.created / task.progress / task.finished update it in
 * place. Shared by every component through one state.
 */
export function useTasks() {
  const tasks = useState<Task[]>('tasks:list', () => [])
  if (!wired && import.meta.client) {
    wired = true
    const api = useApi()
    const events = useEvents()
    const load = () => api.get<Task[]>('/api/tasks', { background: true }).then((t) => { tasks.value = t ?? [] }).catch(() => {})
    // Wired for the life of the app, not of the component that asked first.
    effectScope(true).run(() => {
      events.on((ev: PanelEvent) => {
        if (!ev.type.startsWith('task.')) return
        const d = (ev.data ?? {}) as Partial<Task> & { id?: number }
        if (!d.id) return
        const i = tasks.value.findIndex(x => x.id === d.id)
        if (i < 0) {
          if (ev.type !== 'task.created') {
            // A task this list never saw (it started before the page
            // loaded and is past the newest 100): fetch the list again.
            void load()
            return
          }
          tasks.value = [{ ...(d as Task), site_id: ev.site_id ?? 0 }, ...tasks.value].slice(0, KEEP)
          return
        }
        const next = [...tasks.value]
        next[i] = { ...next[i]!, ...d } as Task
        tasks.value = next
      })
      events.onResync(() => void load())
      // Another account signed in on this tab sees its own tasks.
      const { user } = useAuth()
      watch(() => user.value?.id, (id, was) => {
        if (id !== was) tasks.value = []
        if (id) void load()
      })
    })
    void load()
  }
  return { tasks }
}
