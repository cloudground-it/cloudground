/** Error thrown for every non-2xx answer; the API reports {"error": "..."}. */
export class ApiError extends Error {
  readonly status: number
  readonly data: Record<string, unknown> | null

  constructor(status: number, message: string, data: Record<string, unknown> | null = null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.data = data
  }
}

export function isApiError(error: unknown): error is ApiError {
  return error instanceof ApiError
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
