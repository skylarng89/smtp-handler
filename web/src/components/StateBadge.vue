<script setup lang="ts">
import { computed } from 'vue'
import { stateLabel } from '../format'
import type { MessageState } from '../types'

const props = defineProps<{ state: MessageState }>()

const tone = computed(() => {
  switch (props.state) {
    case 'sent':
      return 'ok'
    case 'failed':
      return 'danger'
    case 'retry_scheduled':
      return 'warn'
    case 'sending':
      return 'info'
    default:
      return 'neutral'
  }
})
</script>

<template>
  <span class="badge" :class="tone" :data-state="state">{{ stateLabel(state) }}</span>
</template>

<style scoped>
.badge {
  display: inline-block;
  padding: 1px 9px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}
.ok {
  background: var(--ok-bg);
  color: var(--ok);
}
.danger {
  background: var(--danger-bg);
  color: var(--danger);
}
.warn {
  background: var(--warn-bg);
  color: var(--warn);
}
.info {
  background: var(--info-bg);
  color: var(--info);
}
.neutral {
  background: var(--neutral-bg);
  color: var(--neutral);
}
</style>
