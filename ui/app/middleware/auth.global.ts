export default defineNuxtRouteMiddleware(async (to) => {
  // Quarry has its own sign-in and session (pages/quarry): the
  // panel's does not guard it.
  if (isStudioRoute(to.path)) return
  const { ensureLoaded, enrolRequired } = useAuth()
  const user = await ensureLoaded()

  if (isPublicRoute(to.path)) {
    if (user) return navigateTo('/')
    return
  }
  if (!user) {
    return navigateTo({ path: '/login', query: to.fullPath !== '/' ? { redirect: to.fullPath } : {} })
  }
  // An admin held for 2FA enrolment can use nothing else until it enrols.
  if (enrolRequired.value && to.path !== '/security') return navigateTo('/security')
  // Operators manage only their own sites: the server pages are not theirs.
  const operatorPages = ['/', '/sites', '/security', '/whats-new', '/assistants']
  if (user.role === 'operator' && !operatorPages.some(p => to.path === p || (p !== '/' && to.path.startsWith(p + '/')))) {
    return navigateTo('/')
  }
})
