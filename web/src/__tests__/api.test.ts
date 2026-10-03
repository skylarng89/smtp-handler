import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, buildListQuery, setUnauthorizedHandler } from '../api'

function mockFetch(status: number, body?: unknown, headers: Record<string, string> = {}) {
  const fn = vi.fn().mockResolvedValue(
    new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } }),
  )
  vi.stubGlobal('fetch', fn)
  return fn
}

afterEach(() => {
  vi.unstubAllGlobals()
  setUnauthorizedHandler(() => {})
})

describe('api client', () => {
  it('builds list queries', () => {
    expect(buildListQuery({})).toBe('')
    expect(buildListQuery({ project: 'a', states: ['failed', 'queued'], q: ' hi ', before: 'X', limit: 10 })).toBe('?project=a&state=failed%2Cqueued&q=hi&before=X&limit=10')
  })

  it('sends the version in If-Match so stale views cannot overwrite newer state', async () => {
    const fn = mockFetch(200, { id: 'X', version: 8 })
    await api.retry('X', 7)
    const [url, init] = fn.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/admin/api/v1/messages/X/retry')
    expect(init.method).toBe('POST')
    expect((init.headers as Record<string, string>)['If-Match']).toBe('"7"')
    expect(init.credentials).toBe('same-origin')
  })

  it('encodes ids in paths', async () => {
    const fn = mockFetch(200, {})
    await api.detail('a/b')
    expect(fn.mock.calls[0][0]).toBe('/admin/api/v1/messages/a%2Fb')
  })

  it('maps problem+json into ApiError', async () => {
    mockFetch(412, { type: 'https://smtp-handler.dev/problems/precondition-failed', detail: 'changed since you loaded it' })
    await expect(api.cancel('X', 1)).rejects.toMatchObject({ status: 412, code: 'precondition-failed', message: 'changed since you loaded it' })
  })

  it('reports field errors', async () => {
    mockFetch(400, { type: 'x/validation-failed', detail: 'bad', errors: [{ field: 'limit', message: 'must be positive' }] })
    const err = await api.list({}).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.fields).toEqual([{ field: 'limit', message: 'must be positive' }])
  })

  it('signals sign-out on 401 but not for failed logins', async () => {
    const handler = vi.fn()
    setUnauthorizedHandler(handler)

    mockFetch(401, { type: 'x/unauthorized', detail: 'session expired' })
    await expect(api.stats()).rejects.toBeInstanceOf(ApiError)
    expect(handler).toHaveBeenCalledTimes(1)

    mockFetch(401, { type: 'x/unauthorized', detail: 'invalid username or password' })
    await expect(api.login('a', 'b')).rejects.toBeInstanceOf(ApiError)
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('handles empty 204 responses and network failures', async () => {
    mockFetch(204)
    await expect(api.logout()).resolves.toBeUndefined()
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('offline')))
    await expect(api.stats()).rejects.toMatchObject({ status: 0, code: 'network-error' })
  })

  it('survives non-JSON error bodies', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<html>bad gateway</html>', { status: 502 })))
    await expect(api.stats()).rejects.toMatchObject({ status: 502 })
  })
})
