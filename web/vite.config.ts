import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// The Go binary embeds the build output (internal/ui/dist) and serves it
// under /ui/, hence the base path.
export default defineConfig({
  base: '/ui/',
  plugins: [vue()],
  build: {
    outDir: '../internal/ui/dist',
    emptyOutDir: false, // keeps .gitkeep; scripts/clean.mjs removes stale assets
    sourcemap: false,
  },
  server: {
    proxy: { '/admin/api': 'http://127.0.0.1:9090' },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
  },
})
