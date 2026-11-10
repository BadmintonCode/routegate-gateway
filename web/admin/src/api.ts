// API client for the RouteGate Admin API (same-origin /admin/api/v1).

export class ApiError extends Error {
  status: number
  code: string
  payload: any
  constructor(status: number, code: string, message: string, payload?: any) {
    super(message)
    this.status = status
    this.code = code
    this.payload = payload
  }
}

let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

export async function api<T = any>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch('/admin/api/v1' + path, {
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) },
    ...init,
  })
  if (res.status === 401) {
    onUnauthorized?.()
    throw new ApiError(401, 'admin.unauthenticated', 'login required')
  }
  const body = await res.json().catch(() => ({}))
  if (!res.ok) {
    const err = body?.error || {}
    throw new ApiError(res.status, err.code || 'error', err.message || res.statusText, body)
  }
  return body as T
}

export const get = <T = any>(p: string) => api<T>(p)
export const post = <T = any>(p: string, body?: any) =>
  api<T>(p, { method: 'POST', body: body === undefined ? '{}' : JSON.stringify(body) })
export const put = <T = any>(p: string, body?: any) =>
  api<T>(p, { method: 'PUT', body: JSON.stringify(body ?? {}) })
export const del = <T = any>(p: string, body?: any) =>
  api<T>(p, { method: 'DELETE', body: JSON.stringify(body ?? {}) })
