import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

const apiProxyTarget = process.env.VITE_API_PROXY_TARGET || 'http://localhost:9820'
const bridgeProxyTarget = process.env.VITE_BRIDGE_PROXY_TARGET || 'http://localhost:9810'

export default defineConfig({
  cacheDir: path.resolve(__dirname, '.vite-cache'),
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 9822,
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
    port: 9823,
  },
})
