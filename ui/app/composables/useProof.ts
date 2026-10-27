import ProofDialog from '~/components/ProofDialog.vue'
import type { Proof } from '~/types'

export interface ProofOptions {
  title: string
  description?: string
  confirmLabel?: string
  danger?: boolean
  /** Only a verified passkey will do (turning password sign-in off). */
  passkeyOnly?: boolean
}

/**
 * Asks the signed-in person to confirm it is them (api SPEC §2.4), with
 * what their account has; resolves the proof, or null when they cancel.
 */
export function useProof() {
  const overlay = useOverlay()
  const dialog = overlay.create(ProofDialog)
  const { user } = useAuth()
  return async (opts: ProofOptions): Promise<Proof | null> => {
    const u = user.value
    const result = await dialog.open({
      ...opts,
      needsPassword: u?.password_signin ?? true,
      totp: !!u?.totp_enabled,
      passkey: (u?.passkeys ?? 0) > 0 && passkeysSupported(),
      recovery: (u?.recovery_codes_left ?? 0) > 0
    })
    return (result as Proof | null) ?? null
  }
}
