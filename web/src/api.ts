import type { BulkResult, ListResponse, MessageDetail, ProjectInfo, StatsResponse, Message, MessageState } from './types'

const BASE = '/admin/api/v1'

export interface FieldError {
  field: string
  message: string
}

/** Error from the admin API (RFC 9457 problem+json). */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public fields: FieldError[] = [],
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

type UnauthorizedHandler = () => void
let onUnauthorized: UnauthorizedHandler = () => {}

/** Called when any request (other than login) comes back 401. */
export function setUnauthorizedHandler(fn: UnauthorizedHandler): void {
  onUnauthorized = fn
}

async function request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<{ data: T; res: Response }> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: { ...headers } }
  if (body !== undefined) {
    ;(init.headers as Record<string, string>)['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }

  let res: Response
  try {
    res = await fetch(BASE + path, init)
  } catch {
    throw new ApiError(0, 'network-error', 'Could not reach the server.')
  }

  if (res.status === 204) return { data: undefined as T, res }

  const text = await res.text()
  let parsed: unknown
  try {
    parsed = text ? JSON.parse(text) : undefined
  } catch {
    parsed = undefined
  }

  if (!res.ok) {
    const p = (parsed ?? {}) as { type?: string; detail?: string; title?: string; errors?: FieldError[] }
    const code = p.type ? p.type.slice(p.type.lastIndexOf('/') + 1) : 'error'
    if (res.status === 401 && path !== '/login') onUnauthorized()
    throw new ApiError(res.status, code, p.detail || p.title || `Request failed (${res.status})`, p.errors ?? [])
  }
  return { data: parsed as T, res }
}

export interface ListQuery {
  project?: string
  states?: MessageState[]
  q?: string
  before?: string
  limit?: number
}

export function buildListQuery(q: ListQuery): string {
  const p = new URLSearchParams()
  if (q.project) p.set('project', q.project)
  if (q.states?.length) p.set('state', q.states.join(','))
  if (q.q?.trim()) p.set('q', q.q.trim())
  if (q.before) p.set('before', q.before)
  if (q.limit) p.set('limit', String(q.limit))
  const s = p.toString()
  return s ? `?${s}` : ''
}

export const api = {
  login: (username: string, password: string) => request<{ username: string }>('POST', '/login', { username, password }).then((r) => r.data),
  logout: () => request<void>('POST', '/logout').then(() => undefined),
  session: () => request<{ username: string }>('GET', '/session').then((r) => r.data),
  projects: () => request<ProjectInfo[]>('GET', '/projects').then((r) => r.data),
  stats: () => request<StatsResponse>('GET', '/stats').then((r) => r.data),
  list: (q: ListQuery) => request<ListResponse>('GET', `/messages${buildListQuery(q)}`).then((r) => r.data),
  detail: (id: string) => request<MessageDetail>('GET', `/messages/${encodeURIComponent(id)}`).then((r) => r.data),
  /** Retry/cancel carry the version the operator saw (If-Match), so a stale view cannot clobber newer state. */
  retry: (id: string, version: number) =>
    request<Message>('POST', `/messages/${encodeURIComponent(id)}/retry`, undefined, { 'If-Match': `"${version}"` }).then((r) => r.data),
  cancel: (id: string, version: number) =>
    request<Message>('POST', `/messages/${encodeURIComponent(id)}/cancel`, undefined, { 'If-Match': `"${version}"` }).then((r) => r.data),
  bulk: (action: 'retry' | 'cancel', items: { id: string; version: number }[]) =>
    request<{ results: BulkResult[] }>('POST', '/messages/bulk', { action, items }).then((r) => r.data.results),
}
