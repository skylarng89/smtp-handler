import { describe, expect, it } from 'vitest'
import { canCancel, canRetry, describeError, formatBytes, relativeTime, shortId, stateLabel } from '../format'
import { parseHash } from '../router'

describe('format', () => {
  const now = Date.parse('2026-10-03T12:00:00Z')

  it('formats relative times in both directions', () => {
    expect(relativeTime('2026-10-03T11:55:00Z', now)).toBe('5 minutes ago')
    expect(relativeTime('2026-10-03T14:00:00Z', now)).toBe('in 2 hours')
    expect(relativeTime('2026-10-03T12:00:00Z', now)).toBe('now')
    expect(relativeTime('2026-09-01T12:00:00Z', now)).toMatch(/month/)
  })

  it('tolerates missing or invalid dates', () => {
    expect(relativeTime(undefined, now)).toBe('—')
    expect(relativeTime('garbage', now)).toBe('—')
  })

  it('formats sizes and ids', () => {
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(2048)).toBe('2.0 KB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB')
    expect(shortId('01M40R5Q8YEMEQ1D9RZG1T6VPY')).toBe('1D9RZG1T6VPY'.slice(-8))
    expect(shortId('abc')).toBe('abc')
  })

  it('mirrors the server rules for which actions are allowed', () => {
    expect(canRetry({ state: 'failed', body_dropped: false })).toBe(true)
    expect(canRetry({ state: 'failed', body_dropped: true })).toBe(false)
    expect(canRetry({ state: 'sending', body_dropped: false })).toBe(false)
    expect(canRetry({ state: 'sent', body_dropped: false })).toBe(false)
    expect(canCancel({ state: 'queued' })).toBe(true)
    expect(canCancel({ state: 'sending' })).toBe(false)
    expect(canCancel({ state: 'sent' })).toBe(false)
    expect(canCancel({ state: 'canceled' })).toBe(false)
  })

  it('labels states and errors', () => {
    expect(stateLabel('retry_scheduled')).toBe('Retry scheduled')
    expect(describeError({ last_error_class: 'permanent', last_error_code: 550 })).toBe('permanent (550)')
    expect(describeError({ last_error_class: 'config' })).toBe('config')
    expect(describeError({})).toBe('')
  })
})

describe('router', () => {
  it('parses message routes and rejects anything else', () => {
    expect(parseHash('#/m/01ABC')).toEqual({ name: 'message', id: '01ABC' })
    expect(parseHash('')).toEqual({ name: 'queue' })
    expect(parseHash('#/')).toEqual({ name: 'queue' })
    expect(parseHash('#/m/<script>')).toEqual({ name: 'queue' })
    expect(parseHash('#/m/../../etc')).toEqual({ name: 'queue' })
  })
})
