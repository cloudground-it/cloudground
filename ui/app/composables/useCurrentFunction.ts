import type { InjectionKey, Ref } from 'vue'
import type { FnView } from '~/utils/functions'

export interface CurrentFunction {
  /** The function with its code and releases (GET /api/functions/{name}). */
  fn: Ref<FnView>
  /** The functions domain, "" when none is set. */
  domain: Ref<string>
  /** /api/functions/{name}. */
  base: Ref<string>
  /** Reads the function again (after a change: a PUT's view may not carry the code). */
  refresh: () => Promise<void>
}

export const currentFunctionKey: InjectionKey<CurrentFunction> = Symbol('current-function')

/** The function whose page is open (provided by pages/functions/[name].vue). */
export function useCurrentFunction(): CurrentFunction {
  const ctx = inject(currentFunctionKey)
  if (!ctx) throw new Error('useCurrentFunction outside a function page')
  return ctx
}
