import ConfirmDialog from '~/components/ConfirmDialog.vue'

export interface ConfirmOptions {
  title: string
  description?: string
  confirmLabel?: string
  danger?: boolean
  /** When set, the operator must type this text (usually the domain). */
  typed?: string
}

/** Opens a confirmation dialog; resolves true when the operator confirms. */
export function useConfirm() {
  const overlay = useOverlay()
  const dialog = overlay.create(ConfirmDialog)
  return async (opts: ConfirmOptions): Promise<boolean> => {
    const result = await dialog.open(opts)
    return result === true
  }
}
