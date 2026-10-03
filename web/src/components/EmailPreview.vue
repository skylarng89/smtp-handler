<script setup lang="ts">
import { computed, ref } from 'vue'
import { formatBytes } from '../format'
import type { Preview } from '../types'

const props = defineProps<{ preview: Preview }>()

const hasHtml = computed(() => !!props.preview.html)
const hasText = computed(() => !!props.preview.text)
const tab = ref<'html' | 'text'>(props.preview.html ? 'html' : 'text')

// Stored emails are untrusted input. The HTML is shown in an iframe with an
// empty sandbox (no scripts, no same-origin access, no forms, no popups) and
// never injected into this page's DOM. The srcdoc also inherits the page CSP,
// which blocks remote images, so previewing cannot fire tracking pixels.
const frameDoc = computed(
  () => `<!doctype html><meta charset="utf-8"><base target="_blank"><style>body{font:14px system-ui;margin:12px;color:#111;background:#fff}</style>${props.preview.html ?? ''}`,
)
</script>

<template>
  <div class="preview">
    <dl class="headers">
      <dt>From</dt>
      <dd>{{ preview.from }}</dd>
      <template v-if="preview.reply_to?.length">
        <dt>Reply-To</dt>
        <dd>{{ preview.reply_to.join(', ') }}</dd>
      </template>
      <dt>To</dt>
      <dd>{{ preview.to.join(', ') }}</dd>
      <template v-if="preview.cc?.length">
        <dt>Cc</dt>
        <dd>{{ preview.cc.join(', ') }}</dd>
      </template>
      <template v-if="preview.bcc?.length">
        <dt>Bcc</dt>
        <dd>{{ preview.bcc.join(', ') }}</dd>
      </template>
      <dt>Subject</dt>
      <dd>
        <strong>{{ preview.subject }}</strong>
      </dd>
    </dl>

    <div v-if="hasHtml && hasText" class="tabs" role="tablist">
      <button role="tab" :aria-selected="tab === 'html'" :class="{ active: tab === 'html' }" @click="tab = 'html'">HTML</button>
      <button role="tab" :aria-selected="tab === 'text'" :class="{ active: tab === 'text' }" @click="tab = 'text'">Plain text</button>
    </div>

    <iframe v-if="hasHtml && tab === 'html'" class="frame" sandbox="" referrerpolicy="no-referrer" title="Email HTML preview" :srcdoc="frameDoc"></iframe>
    <pre v-else-if="hasText" class="text">{{ preview.text }}</pre>
    <p v-else class="muted">This message has no body.</p>

    <ul v-if="preview.attachments?.length" class="attachments">
      <li v-for="a in preview.attachments" :key="a.filename">
        📎 {{ a.filename }} <span class="muted">({{ a.content_type }}, {{ formatBytes(a.size) }})</span>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.headers {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 2px 14px;
  margin: 0 0 12px;
}
dt {
  color: var(--muted);
}
dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.tabs {
  display: flex;
  gap: 4px;
  margin-bottom: 8px;
}
.tabs .active {
  border-color: var(--accent);
  color: var(--accent);
}
.frame {
  width: 100%;
  min-height: 320px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: #fff;
}
.text {
  margin: 0;
  padding: 12px;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  background: var(--bg);
  border-radius: var(--radius);
  font-family: var(--mono);
  font-size: 12.5px;
}
.attachments {
  list-style: none;
  padding: 0;
  margin: 12px 0 0;
}
</style>
