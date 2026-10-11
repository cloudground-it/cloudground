import type { Me } from '~/types'

export function useAuth() {
  const api = useApi()
  const i18n = useNuxtApp().$i18n
  const user = useState<Me | null>('auth:user', () => null)
  const loaded = useState<boolean>('auth:loaded', () => false)
  /**
   * An admin the "Require 2FA" setting holds back until it enrols: the API
   * answers it only on the enrolment routes, so the panel keeps it on the
   * Security page and shows there only what it can use.
   */
  const enrolRequired = useState<boolean>('auth:enrol', () => false)

  const isAdmin = computed(() => user.value?.role === 'admin')
  const isOperator = computed(() => user.value?.role === 'operator')
  /** Admins and operators can change things; read-only accounts cannot. */
  const canManage = computed(() => user.value?.role === 'admin' || user.value?.role === 'operator')

  async function fetchMe(): Promise<Me | null> {
    try {
      const s = await api.get<{ user: Me | null, enrol_required?: boolean }>('/api/session', { skipAuthRedirect: true })
      user.value = s.user
      enrolRequired.value = !!s.user && !!s.enrol_required
    } catch {
      user.value = null
      enrolRequired.value = false
    }
    loaded.value = true
    await applyUserLocale()
    return user.value
  }

  /**
   * A signed-in user sees the panel in the language they chose, whatever
   * the browser says; before they choose, the browser's language (or the
   * cookie of an earlier choice on this device) stays in charge.
   */
  async function applyUserLocale() {
    const chosen = user.value?.locale
    if (isLocaleCode(chosen) && chosen !== i18n.locale.value) await i18n.setLocale(chosen)
  }

  /** Switches the interface language and saves it to the user's profile. */
  async function setUserLocale(code: LocaleCode) {
    await i18n.setLocale(code)
    // Held for enrolment, the profile cannot be saved yet: the choice stays
    // on this device until then.
    if (!user.value || enrolRequired.value) return
    const updated = await api.put<Me>('/api/account/profile', { locale: code })
    user.value = { ...user.value, locale: updated.locale }
  }

  async function ensureLoaded() {
    return loaded.value ? user.value : fetchMe()
  }

  function redirectTarget(): string {
    const redirect = useRouter().currentRoute.value.query.redirect
    return typeof redirect === 'string' && redirect.startsWith('/') && !redirect.startsWith('//') ? redirect : '/'
  }

  async function logout() {
    try {
      await api.post('/api/logout', {}, { skipAuthRedirect: true })
    } finally {
      user.value = null
      await navigateTo('/login')
    }
  }

  return { user, loaded, enrolRequired, isAdmin, isOperator, canManage, fetchMe, ensureLoaded, redirectTarget, logout, setUserLocale }
}
