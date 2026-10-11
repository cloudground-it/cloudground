/** Quarry's pages behind its sign-in: without a Quarry session, to it. */
export default defineNuxtRouteMiddleware(async () => {
  const { session, load } = useStudio()
  if (!session.value) await load()
  if (!session.value) return navigateTo(STUDIO_HOME)
})
