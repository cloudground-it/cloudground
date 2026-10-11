/** One row of a SummaryList: a key, its value, how to show the value. */
export interface SummaryRow {
  k: string
  v: string
  /** A technical value (path, user, host, id): monospaced. */
  mono?: boolean
  /** A copy button after the value. */
  copy?: boolean
  tone?: 'ok' | 'warn' | 'err' | 'muted'
}
