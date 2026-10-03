<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, setUnauthorizedHandler } from './api'
import LoginView from './views/LoginView.vue'
import MessageView from './views/MessageView.vue'
import QueueView from './views/QueueView.vue'
import { navigate, route } from './router'

// null = still checking the session, '' = signed out
const user = ref<string | null>(null)

setUnauthorizedHandler(() => {
  user.value = ''
})

onMounted(async () => {
  try {
    user.value = (await api.session()).username
  } catch {
    user.value = ''
  }
})

async function signOut() {
  try {
    await api.logout()
  } finally {
    user.value = ''
  }
}
</script>

<template>
  <div v-if="user === null" class="boot muted">Loading…</div>
  <LoginView v-else-if="user === ''" @signed-in="(u: string) => (user = u)" />
  <div v-else class="shell">
    <header>
      <a class="brand" href="#/" @click.prevent="navigate({ name: 'queue' })">✉ SMTP Handler</a>
      <span class="spacer"></span>
      <span class="muted">{{ user }}</span>
      <button @click="signOut">Sign out</button>
    </header>
    <main>
      <QueueView v-if="route.name === 'queue'" />
      <MessageView v-else :id="route.id" />
    </main>
  </div>
</template>

<style scoped>
.boot {
  padding: 48px;
  text-align: center;
}
header {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 24px;
  background: var(--surface);
  border-bottom: 1px solid var(--border);
}
.brand {
  font-weight: 700;
  font-size: 16px;
  color: var(--text);
  text-decoration: none;
}
.spacer {
  flex: 1;
}
main {
  max-width: 1200px;
  margin: 0 auto;
  padding: 24px 16px 64px;
}
</style>
