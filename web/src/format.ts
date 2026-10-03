import type { Message, MessageState } from './types'

const STATE_LABELS: Record<MessageState, string> = {
  queued: 'Queued',
  sending: 'Sending',
  retry_scheduled: 'Retry scheduled',
  sent: 'Sent',
  failed: 'Failed',
  canceled: 'Canceled',
}

export function stateLabel(state: MessageState): string {
  return STATE_LABELS[state] ?? state
}

/** What an operator may do with a message in this state. */
export function canRetry(m: Pick<Message, 'state' | 'body_dropped'>): boolean {
  return !m.body_dropped && (m.state === 'failed' || m.state === 'canceled' || m.state === 'retry_scheduled')
}

export function canCancel(m: Pick<Message, 'state'>): boolean {
  return m.state === 'queued' || m.state === 'retry_scheduled' || m.state === 'failed'
}

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 31_536_000],
  ['month', 2_592_000],
  ['day', 86_400],
  ['hour', 3_600],
  ['minute', 60],
  ['second', 1],
]

const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })

/** "5 minutes ago" / "in 2 hours". `now` is injectable for tests. */
export function relativeTime(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return '—'
  const diff = Math.round((t - now) / 1000)
  for (const [unit, secs] of UNITS) {
    if (Math.abs(diff) >= secs || unit === 'second') {
      return rtf.format(Math.round(diff / secs), unit)
    }
  }
  return '—'
}

export function absoluteTime(iso: string | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

/** Last 8 characters of a ULID: unique enough to scan in a table. */
export function shortId(id: string): string {
  return id.length > 8 ? id.slice(-8) : id
}

export function describeError(m: Pick<Message, 'last_error_class' | 'last_error_code'>): string {
  if (!m.last_error_class) return ''
  return m.last_error_code ? `${m.last_error_class} (${m.last_error_code})` : m.last_error_class
}
