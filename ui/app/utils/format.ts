import { ref } from 'vue'

/** The interface languages and the Intl locale each formats with. */
export const LOCALE_TAGS = {
  it: 'it-IT',
  en: 'en-US',
  fr: 'fr-FR',
  es: 'es-ES',
  de: 'de-DE'
} as const

export type LocaleCode = keyof typeof LOCALE_TAGS

export function isLocaleCode(code: unknown): code is LocaleCode {
  return typeof code === 'string' && Object.hasOwn(LOCALE_TAGS, code)
}

/**
 * The active interface language, kept in step with vue-i18n by the
 * i18n-format plugin. It is a ref so a template that formats without passing
 * a locale still re-renders when the language changes.
 */
export const formatLocale = ref<LocaleCode>('it')

function tag(locale?: string): string {
  return LOCALE_TAGS[isLocaleCode(locale) ? locale : formatLocale.value]
}

// French writes sizes in octets (o, Ko, Mo…); the others use B, KB, MB….
const byteUnits: Record<string, string[]> = {
  fr: ['o', 'Ko', 'Mo', 'Go', 'To'],
  default: ['B', 'KB', 'MB', 'GB', 'TB']
}

/** A size in binary multiples (1 KB = 1024 B), in the active locale. */
export function fmtBytes(n: number, locale?: string): string {
  const code = isLocaleCode(locale) ? locale : formatLocale.value
  const units = byteUnits[code] ?? byteUnits.default!
  if (!n || n <= 0) return `0 ${units[0]}`
  const i = Math.min(Math.floor(Math.log2(n) / 10), units.length - 1)
  const value = new Intl.NumberFormat(tag(code), {
    minimumFractionDigits: i === 0 ? 0 : 1,
    maximumFractionDigits: i === 0 ? 0 : 1
  }).format(n / 2 ** (10 * i))
  return `${value} ${units[i]}`
}

/** A number with the active locale's separators. */
export function fmtNumber(n: number, digits?: number, locale?: string): string {
  return new Intl.NumberFormat(tag(locale), digits === undefined ? {} : { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(n)
}

/** A percentage given as 0–100 ("17,5 %" in German, "17.5%" in English). */
export function fmtPercent(pct: number, digits = 0, locale?: string): string {
  return new Intl.NumberFormat(tag(locale), { style: 'percent', minimumFractionDigits: 0, maximumFractionDigits: digits }).format(pct / 100)
}

export function fmtDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s} s`
  return `${Math.floor(s / 60)} min ${s % 60} s`
}

export function fmtDateTime(iso?: string, locale?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—'
  return d.toLocaleString(tag(locale), { dateStyle: 'medium', timeStyle: 'short' })
}

/** Relative time ("3 min fa" / "3 min ago" / "vor 3 Min.") using Intl. */
export function fmtAgo(iso?: string, locale?: string): string {
  if (!iso) return '—'
  const then = new Date(iso).getTime()
  if (!then || new Date(iso).getFullYear() < 2000) return '—'
  const rtf = new Intl.RelativeTimeFormat(tag(locale), { numeric: 'auto', style: 'short' })
  const sec = Math.round((then - Date.now()) / 1000)
  const abs = Math.abs(sec)
  if (abs < 60) return rtf.format(sec, 'second')
  if (abs < 3600) return rtf.format(Math.round(sec / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(sec / 3600), 'hour')
  return rtf.format(Math.round(sec / 86400), 'day')
}

/** A compact moment for activity lists: "03:40" today, "ieri", "3 g fa". */
export function fmtWhen(iso?: string, locale?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—'
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const days = Math.round((day(new Date()) - day(d)) / 86400000)
  if (days === 0) return d.toLocaleTimeString(tag(locale), { hour: '2-digit', minute: '2-digit' })
  return new Intl.RelativeTimeFormat(tag(locale), { numeric: days === 1 ? 'auto' : 'always', style: 'narrow' }).format(-days, 'day')
}

/** "Ada Lovelace", or the e-mail when no name is set. */
export function displayName(u: { first_name?: string, last_name?: string, email?: string } | null | undefined): string {
  if (!u) return ''
  return [u.first_name, u.last_name].filter(Boolean).join(' ') || u.email || ''
}
