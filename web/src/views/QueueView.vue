<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import StateBadge from '../components/StateBadge.vue'
import { absoluteTime, canCancel, canRetry, describeError, relativeTime, shortId, stateLabel } from '../format'
import { navigate } from '../router'
import { ALL_STATES, type Message, type MessageState, type ProjectInfo } from '../types'

const REFRESH_MS = 5000
const PAGE_SIZE = 50

const projects = ref<ProjectInfo[]>([])
const totals = ref<Record<MessageState, number> | null>(null)
const messages = ref<Message[]>([])
const nextCursor = ref('')
const loading = ref(false)
const error = ref('')
const notice = ref('')

const project = ref('')
const states = ref<MessageState[]>([])
const query = ref('')
const autoRefresh = ref(true)
const selected = ref<Set<string>>(new Set())
const bulkBusy = ref(false)

const selectedMessages = computed(() => messages.value.filter((m) => selected.value.has(m.id)))
const retryable = computed(() => selectedMessages.value.filter(canRetry))
const cancelable = computed(() => selectedMessages.value.filter(canCancel))
const allSelected = computed(() => messages.value.length > 0 && messages.value.every((m) => selected.value.has(m.id)))

function fail(e: unknown, fallback: string) {
  error.value = e instanceof ApiError ? e.message : fallback
}

async function loadStats() {
  try {
    totals.value = (await api.stats()).totals
  } catch (e) {
    fail(e, 'Could not load statistics.')
  }
}

async function load(append = false) {
  loading.value = true
  error.value = ''
  try {
    const res = await api.list({
      project: project.value,
      states: states.value,
      q: query.value,
      before: append ? nextCursor.value : undefined,
      limit: PAGE_SIZE,
    })
    messages.value = append ? [...messages.value, ...res.messages] : res.messages
    nextCursor.value = res.next_cursor ?? ''
    // Drop selections that no longer exist in the list.
    const ids = new Set(messages.value.map((m) => m.id))
    selected.value = new Set([...selected.value].filter((id) => ids.has(id)))
  } catch (e) {
    fail(e, 'Could not load messages.')
  } finally {
    loading.value = false
  }
}

async function reload() {
  await Promise.all([load(false), loadStats()])
}

function toggleState(s: MessageState) {
  states.value = states.value.includes(s) ? states.value.filter((x) => x !== s) : [...states.value, s]
}

function toggleAll() {
  selected.value = allSelected.value ? new Set() : new Set(messages.value.map((m) => m.id))
}

function toggleOne(id: string) {
  const next = new Set(selected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  selected.value = next
}

async function bulk(action: 'retry' | 'cancel') {
  const targets = action === 'retry' ? retryable.value : cancelable.value
  if (!targets.length) return
  if (action === 'cancel' && !window.confirm(`Cancel ${targets.length} message(s)? They will not be delivered.`)) return

  bulkBusy.value = true
  notice.value = ''
  error.value = ''
  try {
    const results = await api.bulk(action, targets.map((m) => ({ id: m.id, version: m.version })))
    const ok = results.filter((r) => r.ok).length
    const failed = results.length - ok
    notice.value = `${ok} message(s) ${action === 'retry' ? 'queued for retry' : 'canceled'}` + (failed ? `, ${failed} skipped because they changed or no longer qualify.` : '.')
    selected.value = new Set()
  } catch (e) {
    fail(e, 'The bulk action failed.')
  } finally {
    bulkBusy.value = false
    await reload()
  }
}

let debounce: ReturnType<typeof setTimeout> | undefined
watch([project, states], () => void reload(), { deep: true })
watch(query, () => {
  clearTimeout(debounce)
  debounce = setTimeout(() => void reload(), 300)
})

let timer: ReturnType<typeof setInterval> | undefined
onMounted(async () => {
  try {
    projects.value = await api.projects()
  } catch (e) {
    fail(e, 'Could not load projects.')
  }
  await reload()
  timer = setInterval(() => {
    // Only refresh the first page; paging further means the operator is reading history.
    if (autoRefresh.value && !loading.value && !bulkBusy.value && messages.value.length <= PAGE_SIZE) void reload()
  }, REFRESH_MS)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  clearTimeout(debounce)
})
</script>

<template>
  <section>
    <div v-if="totals" class="tiles">
      <button v-for="s in ALL_STATES" :key="s" class="tile" :class="{ on: states.includes(s) }" :aria-pressed="states.includes(s)" @click="toggleState(s)">
        <span class="count">{{ totals[s] }}</span>
        <span class="label">{{ stateLabel(s) }}</span>
      </button>
    </div>

    <div class="card">
      <div class="row filters">
        <select v-model="project" aria-label="Project">
          <option value="">All projects</option>
          <option v-for="p in projects" :key="p.id" :value="p.id">{{ p.name }}</option>
        </select>
        <input v-model="query" type="search" placeholder="Search subject, recipient or ID…" aria-label="Search" />
        <button v-if="states.length" @click="states = []">Clear state filter</button>
        <span class="spacer"></span>
        <label class="row auto"><input v-model="autoRefresh" type="checkbox" /> Auto-refresh</label>
        <button :disabled="loading" @click="reload">Refresh</button>
      </div>

      <div v-if="selected.size" class="row bulk" role="toolbar" aria-label="Bulk actions">
        <strong>{{ selected.size }} selected</strong>
        <button class="primary" :disabled="bulkBusy || !retryable.length" @click="bulk('retry')">Retry ({{ retryable.length }})</button>
        <button class="danger" :disabled="bulkBusy || !cancelable.length" @click="bulk('cancel')">Cancel ({{ cancelable.length }})</button>
        <button @click="selected = new Set()">Clear</button>
      </div>

      <p v-if="error" class="error-box" role="alert">{{ error }}</p>
      <p v-if="notice" class="notice" role="status">{{ notice }}</p>

      <div class="scroll">
        <table>
          <thead>
            <tr>
              <th class="check"><input type="checkbox" :checked="allSelected" aria-label="Select all" @change="toggleAll" /></th>
              <th>Status</th>
              <th>Subject</th>
              <th>To</th>
              <th>Project</th>
              <th>Attempts</th>
              <th>Created</th>
              <th>Last error</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in messages" :key="m.id" class="item" @click="navigate({ name: 'message', id: m.id })">
              <td class="check" @click.stop><input type="checkbox" :checked="selected.has(m.id)" :aria-label="`Select ${m.subject}`" @change="toggleOne(m.id)" /></td>
              <td><StateBadge :state="m.state" /></td>
              <td class="subject">
                <a :href="`#/m/${m.id}`" @click.stop>{{ m.subject || '(no subject)' }}</a>
                <span class="muted mono"> {{ shortId(m.id) }}</span>
              </td>
              <td class="to">{{ m.to }}</td>
              <td>{{ m.project_id }}</td>
              <td>{{ m.attempt }}/{{ m.max_attempts }}</td>
              <td :title="absoluteTime(m.created_at)">{{ relativeTime(m.created_at) }}</td>
              <td class="muted">{{ describeError(m) }}</td>
            </tr>
            <tr v-if="!messages.length && !loading">
              <td colspan="8" class="empty muted">No messages match.</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="row more">
        <button v-if="nextCursor" :disabled="loading" @click="load(true)">Load more</button>
        <span v-if="loading" class="muted">Loading…</span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.tiles {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: 10px;
  margin-bottom: 16px;
}
.tile {
  display: grid;
  gap: 2px;
  text-align: left;
  padding: 10px 14px;
}
.tile.on {
  border-color: var(--accent);
  box-shadow: inset 0 0 0 1px var(--accent);
}
.count {
  font-size: 22px;
  font-weight: 700;
}
.label {
  color: var(--muted);
  font-size: 12px;
}
.filters {
  margin-bottom: 12px;
}
.filters input[type='search'] {
  min-width: min(320px, 100%);
}
.auto {
  gap: 6px;
}
.bulk {
  background: var(--info-bg);
  border-radius: var(--radius);
  padding: 8px 12px;
  margin-bottom: 12px;
}
.notice {
  background: var(--ok-bg);
  color: var(--ok);
  border-radius: var(--radius);
  padding: 10px 12px;
}
.scroll {
  overflow-x: auto;
}
table {
  width: 100%;
  border-collapse: collapse;
}
th,
td {
  text-align: left;
  padding: 8px;
  border-bottom: 1px solid var(--border);
  vertical-align: top;
}
th {
  color: var(--muted);
  font-weight: 600;
  white-space: nowrap;
}
.check {
  width: 32px;
}
.item {
  cursor: pointer;
}
.item:hover {
  background: var(--bg);
}
.subject {
  max-width: 320px;
  overflow-wrap: anywhere;
}
.to {
  max-width: 220px;
  overflow-wrap: anywhere;
}
.empty {
  text-align: center;
  padding: 32px;
}
.more {
  margin-top: 12px;
}
</style>
