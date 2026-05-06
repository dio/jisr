import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // In dev mode, proxy /api/* to Envoy (port 10000) so the SPA can call
    // the api-backend filter without CORS issues.
    // Start Envoy first: ENVOY_DYNAMIC_MODULES_SEARCH_PATH=. envoy -c envoy.yaml
    proxy: {
      '/api': 'http://localhost:10000',
    },
  },
})
