/**
 * A task kind in words ("backup.scheduled" → "Backup programmato"), from the
 * server's catalogue (GET /api/tasks/names, useTaskNames): the alerts and
 * the webhook use the same names. The kind itself until the names arrive.
 */
export function taskKindLabel(names: Record<string, string> | null | undefined, kind: string): string {
  return names?.[kind] || kind
}
