/**
 * Runs an operator action with a busy flag and a toast for the outcome.
 * Returns true on success so callers can chain a refresh.
 */
export function useAction() {
  const toast = useToast()
  const { t } = useI18n()
  const busy = ref(false)

  async function run(fn: () => Promise<unknown>, success?: string): Promise<boolean> {
    busy.value = true
    try {
      await fn()
      if (success) toast.add({ title: success, color: 'success', icon: 'i-cg-check-circle' })
      return true
    } catch (e) {
      toast.add({ title: t('common.failed'), description: errorMessage(e), color: 'error', icon: 'i-cg-alert-circle' })
      return false
    } finally {
      busy.value = false
    }
  }

  return { busy, run }
}
