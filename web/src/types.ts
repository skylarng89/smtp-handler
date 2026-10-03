export type MessageState = 'queued' | 'sending' | 'retry_scheduled' | 'sent' | 'failed' | 'canceled'

export const ALL_STATES: MessageState[] = ['queued', 'sending', 'retry_scheduled', 'sent', 'failed', 'canceled']

export interface Message {
  id: string
  project_id: string
  template?: string
  key_id?: string
  state: MessageState
  version: number
  attempt: number
  max_attempts: number
  next_attempt_at: string
  deadline_at: string
  lease_owner?: string
  lease_until?: string
  message_id: string
  subject: string
  from: string
  to: string
  body_dropped: boolean
  last_error_class?: string
  last_error_code?: number
  last_error?: string
  created_at: string
  updated_at: string
  sent_at?: string
}

export interface Attempt {
  attempt: number
  node: string
  started_at: string
  finished_at: string
  outcome: string
  smtp_code?: number
  enhanced_code?: string
  error?: string
}

export interface Preview {
  from: string
  reply_to?: string[]
  to: string[]
  cc?: string[]
  bcc?: string[]
  subject: string
  text?: string
  html?: string
  attachments?: { filename: string; content_type: string; size: number }[]
}

export interface MessageDetail extends Message {
  attempts: Attempt[]
  preview: Preview | null
}

export interface ListResponse {
  messages: Message[]
  next_cursor?: string
}

export interface StatsResponse {
  totals: Record<MessageState, number>
}

export interface ProjectInfo {
  id: string
  name: string
  from: string
  templates: string[]
  delivery_semantics: string
}

export interface BulkResult {
  id: string
  ok: boolean
  version?: number
  state?: string
  error?: string
  code?: string
}
