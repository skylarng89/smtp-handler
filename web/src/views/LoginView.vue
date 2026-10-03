<script setup lang="ts">
import { ref } from 'vue'
import { api, ApiError } from '../api'

const emit = defineEmits<{ (e: 'signed-in', username: string): void }>()

const username = ref('')
const password = ref('')
const busy = ref(false)
const error = ref('')

async function submit() {
  busy.value = true
  error.value = ''
  try {
    const res = await api.login(username.value, password.value)
    password.value = ''
    emit('signed-in', res.username)
  } catch (e) {
    error.value = e instanceof ApiError ? (e.status === 429 ? 'Too many attempts. Wait a minute and try again.' : e.message) : 'Sign-in failed.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="login">
    <form class="card" @submit.prevent="submit">
      <h1>SMTP Handler</h1>
      <p class="muted">Sign in to manage the mail queue.</p>
      <label>
        Username
        <input v-model="username" type="text" autocomplete="username" required autofocus />
      </label>
      <label>
        Password
        <input v-model="password" type="password" autocomplete="current-password" required />
      </label>
      <p v-if="error" class="error-box" role="alert">{{ error }}</p>
      <button class="primary" type="submit" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
    </form>
  </main>
</template>

<style scoped>
.login {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: 16px;
}
form {
  width: min(380px, 100%);
  display: grid;
  gap: 12px;
}
label {
  display: grid;
  gap: 4px;
  font-weight: 600;
}
input {
  font-weight: 400;
}
</style>
