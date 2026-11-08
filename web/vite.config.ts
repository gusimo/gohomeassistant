import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// base './' keeps all asset URLs relative, so the UI works under HA Ingress.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: { outDir: '../internal/server/dist/app', emptyOutDir: true, chunkSizeWarningLimit: 4096 },
  server: { proxy: { '/api': 'http://localhost:8099' } },
})
