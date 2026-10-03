import { computed, ref } from 'vue'

export type Route = { name: 'queue' } | { name: 'message'; id: string }

export function parseHash(hash: string): Route {
  const m = /^#\/m\/([0-9A-Za-z]{1,64})$/.exec(hash)
  return m ? { name: 'message', id: m[1] } : { name: 'queue' }
}

const hash = ref(typeof location === 'undefined' ? '' : location.hash)

if (typeof window !== 'undefined') {
  window.addEventListener('hashchange', () => {
    hash.value = location.hash
  })
}

export const route = computed<Route>(() => parseHash(hash.value))

export function navigate(to: Route): void {
  location.hash = to.name === 'message' ? `#/m/${to.id}` : '#/'
}
