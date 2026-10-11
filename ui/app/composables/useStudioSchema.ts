import type { DBStudioTables } from '~/types/dbstudio'

/**
 * What Quarry's tree shows, shared by the tree and the pages: the
 * databases the session may open, and each one's tables, views and
 * routines once it has been opened. A page that drops or creates a table
 * reloads its database's list.
 */
export function useStudioSchema() {
  const { api, dbBase } = useStudio()
  const databases = useState<string[] | null>('studio:databases', () => null)
  const tables = useState<Record<string, DBStudioTables>>('studio:tables', () => ({}))
  const errors = useState<Record<string, string>>('studio:tableErrors', () => ({}))

  async function loadDatabases(force = false) {
    if (databases.value && !force) return databases.value
    const r = await api.get<{ databases: string[] }>(`${STUDIO_API}/databases`)
    databases.value = r.databases ?? []
    return databases.value
  }

  async function loadTables(db: string, force = false) {
    if (tables.value[db] && !force) return tables.value[db]
    try {
      const r = await api.get<DBStudioTables>(`${dbBase(db)}/tables`)
      tables.value = { ...tables.value, [db]: r }
      const { [db]: _gone, ...rest } = errors.value
      errors.value = rest
      return r
    } catch (e) {
      errors.value = { ...errors.value, [db]: errorMessage(e) }
      throw e
    }
  }

  return { databases, tables, errors, loadDatabases, loadTables }
}

/** Forgets the tree: a new session sees its own databases. */
export function clearStudioSchema() {
  clearNuxtState(['studio:databases', 'studio:tables', 'studio:tableErrors'])
}
