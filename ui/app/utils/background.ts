/**
 * Requests the panel sends on its own (a poll, a refetch after an event)
 * carry this header, so the server does not count them as use of the
 * session: only what the person does keeps it from the 2-hour idle timeout
 * (internal/api/SPEC.md §2.2).
 */
export const BACKGROUND_HEADER = 'X-CG-Background'

let depth = 0

/**
 * Runs `fn` with every API request it starts synchronously marked as
 * background. The request reads the mark when it is built, before its first
 * await, so a fetcher must call the API before awaiting anything else (every
 * fetcher in the app does).
 */
export function inBackground<T>(fn: () => T): T {
  depth++
  try {
    return fn()
  } finally {
    depth--
  }
}

/** Whether a request being built now is a background one. */
export function isBackground(): boolean {
  return depth > 0
}
