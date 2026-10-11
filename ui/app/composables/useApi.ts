type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

interface RequestOptions {
  /** Do not bounce to /login on 401 (used while probing the session). */
  skipAuthRedirect?: boolean
  /** Sent by the panel on its own, not by the person: not a use of the session. */
  background?: boolean
}

export const PUBLIC_ROUTES = ['/login']
export const isPublicRoute = (path: string) => PUBLIC_ROUTES.includes(path) || path.startsWith('/setup/') || isStudioRoute(path)

/**
 * Same-origin client for the panel API. Every request with a body is sent as
 * application/json: the API refuses state-changing requests in any other
 * content type (S-H4), so a cross-site form post cannot reach it.
 */
export function useApi() {
  const nuxtApp = useNuxtApp()

  async function request<T>(method: Method, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
    // The interface's language, so text the server writes (alerts) matches it.
    const headers: Record<string, string> = { 'Accept': 'application/json', 'Accept-Language': formatLocale.value }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (opts.background || isBackground()) headers[BACKGROUND_HEADER] = '1'
    let resp: Response
    try {
      resp = await fetch(path, {
        method,
        headers,
        body: body !== undefined ? JSON.stringify(body) : undefined,
        credentials: 'same-origin'
      })
    } catch (error) {
      throw new ApiError(0, errorMessage(error))
    }
    const text = await resp.text()
    let data: unknown = null
    try {
      data = text ? JSON.parse(text) : null
    } catch {
      // A non-JSON body (a proxy error page): report the status instead.
    }
    if (!resp.ok) {
      const obj = data && typeof data === 'object' ? data as Record<string, unknown> : null
      const message = obj && typeof obj.error === 'string' ? obj.error : `HTTP ${resp.status}`
      if (resp.status === 401 && !opts.skipAuthRedirect) {
        useState('auth:user').value = null
        const route = nuxtApp.$router.currentRoute.value
        if (!isPublicRoute(route.path)) {
          await nuxtApp.runWithContext(() => navigateTo({ path: '/login', query: { redirect: route.fullPath } }))
        }
      }
      throw new ApiError(resp.status, message, obj)
    }
    return data as T
  }

  return {
    get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, undefined, opts),
    post: <T>(path: string, body: unknown = {}, opts?: RequestOptions) => request<T>('POST', path, body, opts),
    put: <T>(path: string, body: unknown, opts?: RequestOptions) => request<T>('PUT', path, body, opts),
    patch: <T>(path: string, body: unknown, opts?: RequestOptions) => request<T>('PATCH', path, body, opts),
    del: <T>(path: string, opts?: RequestOptions, body?: unknown) => request<T>('DELETE', path, body, opts)
  }
}
