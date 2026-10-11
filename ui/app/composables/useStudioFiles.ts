import type { DBStudioFiles } from '~/types/dbstudio'

/** A task a Quarry session started, as /api/quarry/tasks/{id} shows it. */
export interface StudioTask {
  id: number
  kind: string
  status: string
  progress?: number
  message?: string
  error?: string
}

const DONE = ['succeeded', 'failed', 'canceled']

/**
 * The exports and uploads the session may see (a database user: its own),
 * and the export and import tasks it started on this page, followed until
 * they end; the files are read again when one does.
 */
export function useStudioFiles() {
  const { api } = useStudio()
  const { data: files, refresh, error } = useLoad(() => api.get<DBStudioFiles>(`${STUDIO_API}/files`))
  const tasks = ref<StudioTask[]>([])
  let timer: ReturnType<typeof setInterval> | null = null

  async function poll() {
    const open = tasks.value.filter(x => !DONE.includes(x.status))
    if (!open.length) {
      stop()
      return
    }
    let ended = false
    for (const x of open) {
      try {
        const now = await api.get<StudioTask>(`${STUDIO_API}/tasks/${x.id}`, true)
        if (DONE.includes(now.status)) ended = true
        tasks.value = tasks.value.map(y => y.id === x.id ? now : y)
      } catch {
        // Read again at the next tick.
      }
    }
    if (ended) await refresh()
  }
  function stop() {
    if (timer) clearInterval(timer)
    timer = null
  }
  function follow(task: StudioTask) {
    tasks.value = [task, ...tasks.value].slice(0, 10)
    if (!timer) timer = setInterval(() => void poll(), 2000)
  }
  onBeforeUnmount(stop)
  return { files, error, refresh, tasks, follow }
}
