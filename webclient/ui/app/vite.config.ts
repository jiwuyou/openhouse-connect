import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

// In the "copied frontend" workflow, the UI is served by the webclient backend (9840),
// so during local `vite dev` we proxy API/bridge to that same backend.
const apiProxyTarget = process.env.VITE_API_PROXY_TARGET || 'http://localhost:9840'
const bridgeProxyTarget = process.env.VITE_BRIDGE_PROXY_TARGET || 'http://localhost:9840'

export default defineConfig({
  cacheDir: path.resolve(__dirname, '.vite-cache'),
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    // Emit the built UI into the embedded static directory served by webclient.
    outDir: path.resolve(__dirname, '../static'),
    // outDir is outside project root; allow cleaning it between builds.
    emptyOutDir: true,
  },
  server: {
    port: 9841,
    watch: {
      usePolling: true,
      interval: 300,
    },
    proxy: {
      '/api': {
        target: apiProxyTarget,
        changeOrigin: true,
        timeout: 45000,
      },
      '/bridge': {
        target: bridgeProxyTarget,
        changeOrigin: true,
        ws: true,
      },
    },
  },
  preview: {
    port: 9843,
  },
})
