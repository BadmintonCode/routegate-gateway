import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The panel mounts at /admin/ inside the gateway's admin listener; the API is
// same-origin under /admin/api/v1/. Dev proxy targets a local gateway.
export default defineConfig({
  plugins: [react()],
  base: '/admin/',
  build: {
    outDir: '../../internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/admin/api': 'http://127.0.0.1:8081',
    },
  },
})
