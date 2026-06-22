import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The SPA builds to ./dist, which cmd/console embeds via go:embed. In dev,
// /console and /auth are proxied to a locally running console binary.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    proxy: {
      '/console': 'http://localhost:8080',
      '/auth': 'http://localhost:8080',
    },
  },
})
