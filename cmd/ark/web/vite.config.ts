import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    proxy: {
      // Proxy API and WebSocket calls to the arkUI backend during development.
      // This avoids CORS issues and mirrors the production setup where arkUI
      // serves both the frontend and the /api/* routes from the same origin.
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true,
        // Keeps the Host header equal to the Origin, the backend rejects
        // cross-origin upgrades.
      },
    },
  },
})
