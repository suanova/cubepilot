// HTTP client -- every request carries the X-CubePilot-User header; the
// backend derives the operator identity from it (design §3.3.4).

const USER_HEADER = 'X-CubePilot-User'

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export async function apiFetch<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    ...(opts.headers as Record<string, string> | undefined),
  }
  const user = getCurrentUser()
  if (user) headers[USER_HEADER] = user
  const resp = await fetch(path, { ...opts, headers })
  if (!resp.ok) {
    let detail = ''
    try {
      const body = await resp.json()
      detail = body.error || ''
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(detail || `HTTP ${resp.status}`, resp.status)
  }
  return (await resp.json()) as T
}

export function getCurrentUser(): string {
  return localStorage.getItem('cubepilot.user') || 'zhang.wei'
}

export function setCurrentUser(user: string): void {
  localStorage.setItem('cubepilot.user', user)
}