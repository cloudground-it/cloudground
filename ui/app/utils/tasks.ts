/** A task kind in words ("backup.scheduled" → "Backup programmato"). */
export function taskKindLabel(t: (key: string) => string, kind: string): string {
  // vue-i18n reads dots as nesting, so the keys use underscores.
  const key = `tasks.kind.${kind.replace(/\./g, '_')}`
  const label = t(key)
  return label === key ? kind : label
}
