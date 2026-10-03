<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import EmailPreview from '../components/EmailPreview.vue'
import StateBadge from '../components/StateBadge.vue'
import { absoluteTime, canCancel, canRetry, relativeTime } from '../format'
import { navigate } from '../router'
import type { MessageDetail } from '../types'

const props = defineProps<{ id: string }>()

const msg = ref<MessageDetail | null>(null)
const error = ref('')
const notice = ref('')
const busy = ref(false)

async function load() {
  error.value = ''
  try {
    msg.value = await api.detail(props.id)
  } catch (e) {
    msg.value = null
    error.value = e instanceof ApiError ? e.message : 'Could not load the message.'
  }
}

async function act(kind: 'retry' | 'cancel') {
  if (!msg.value) return
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    await (kind === 'retry' ? api.retry : api.cancel)(msg.value.id, msg.value.version)
    notice.value = kind === 'retry' ? 'Queued for another delivery attempt.' : 'Message canceled.'
  } catch (e) {
    // 412/409: someone else (or a worker) changed it; show fresh state instead of overwriting.
    error.value = e instanceof ApiError ? e.message : 'The action failed.'
  } finally {
    busy.value = false
    await load()
  }
}

onMounted(load)
watch(() => props.id, load)
</script>

<template>
  <section>
    <p><a href="#/" @click.prevent="navigate({ name: 'queue' })">← Back to queue</a></p>
    <p v-if="error" class="error-box" role="alert">{{ error }}</p>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>

    <div v-if="msg" class="grid">
      <div class="card">
        <div class="row">
          <h1>{{ msg.subject }}</h1>
          <StateBadge :state="msg.state" />
          <span class="spacer"></span>
          <button class="primary" :disabled="busy || !canRetry(msg)" :title="msg.body_dropped ? 'The body was removed by the retention policy' : ''" @click="act('retry')">Retry now</button>
          <button class="danger" :disabled="busy || !canCancel(msg)" @click="act('cancel')">Cancel</button>
        </div>
        <p class="muted mono">{{ msg.id }}</p>

        <div v-if="msg.last_error_class" class="error-box">
          <strong>{{ msg.last_error_class }}<template v-if="msg.last_error_code"> · SMTP {{ msg.last_error_code }}</template></strong>
          <div class="mono">{{ msg.last_error }}</div>
        </div>

        <dl class="facts">
          <dt>Project</dt>
          <dd>{{ msg.project_id }}<span v-if="msg.template" class="muted"> · {{ msg.template }}</span></dd>
          <dt>Attempts</dt>
          <dd>{{ msg.attempt }} of {{ msg.max_attempts }}</dd>
          <dt>Created</dt>
          <dd :title="absoluteTime(msg.created_at)">{{ relativeTime(msg.created_at) }}</dd>
          <template v-if="msg.sent_at">
            <dt>Sent</dt>
            <dd :title="absoluteTime(msg.sent_at)">{{ relativeTime(msg.sent_at) }}</dd>
          </template>
          <template v-if="msg.state === 'retry_scheduled' || msg.state === 'queued'">
            <dt>Next attempt</dt>
            <dd :title="absoluteTime(msg.next_attempt_at)">{{ relativeTime(msg.next_attempt_at) }}</dd>
          </template>
          <template v-if="msg.state === 'sending'">
            <dt>Held by</dt>
            <dd class="mono">{{ msg.lease_owner }}</dd>
          </template>
          <dt>Gives up</dt>
          <dd :title="absoluteTime(msg.deadline_at)">{{ relativeTime(msg.deadline_at) }}</dd>
          <dt>Message-ID</dt>
          <dd class="mono">&lt;{{ msg.message_id }}&gt;</dd>
          <dt>API key</dt>
          <dd class="mono">{{ msg.key_id || '—' }}</dd>
        </dl>
      </div>

      <div class="card">
        <h2>Delivery attempts</h2>
        <p v-if="!msg.attempts.length" class="muted">No attempts recorded yet.</p>
        <table v-else>
          <thead>
            <tr><th>#</th><th>When</th><th>Node</th><th>Outcome</th><th>Detail</th></tr>
          </thead>
          <tbody>
            <tr v-for="a in msg.attempts" :key="a.node + a.started_at + a.outcome">
              <td>{{ a.attempt }}</td>
              <td :title="absoluteTime(a.started_at)">{{ relativeTime(a.started_at) }}</td>
              <td class="mono">{{ a.node }}</td>
              <td>{{ a.outcome }}<span v-if="a.smtp_code" class="muted"> · {{ a.smtp_code }}<template v-if="a.enhanced_code"> {{ a.enhanced_code }}</template></span></td>
              <td class="mono detail">{{ a.error }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card">
        <h2>Message</h2>
        <EmailPreview v-if="msg.preview" :preview="msg.preview" />
        <p v-else class="muted">The message body is no longer stored (removed by the retention policy).</p>
      </div>
    </div>
    <p v-else-if="!error" class="muted">Loading…</p>
  </section>
</template>

<style scoped>
.grid {
  display: grid;
  gap: 16px;
}
.notice {
  background: var(--ok-bg);
  color: var(--ok);
  border-radius: var(--radius);
  padding: 10px 12px;
}
.facts {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 4px 16px;
  margin: 16px 0 0;
}
.facts dt {
  color: var(--muted);
}
.facts dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.error-box {
  margin: 12px 0;
}
table {
  width: 100%;
  border-collapse: collapse;
}
th,
td {
  text-align: left;
  padding: 6px 8px;
  border-bottom: 1px solid var(--border);
  vertical-align: top;
}
th {
  color: var(--muted);
  font-weight: 600;
}
.detail {
  overflow-wrap: anywhere;
}
</style>
