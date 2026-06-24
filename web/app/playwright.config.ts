import { defineConfig } from '@playwright/test'

// Light smoke only, per the repo's Go-centric test philosophy. Assumes the
// console binary (or `vite preview`) serves the built SPA at PORT 4173.
export default defineConfig({
  testDir: './e2e',
  use: { baseURL: 'http://localhost:4173' },
  webServer: {
    command: 'pnpm build && pnpm preview --port 4173',
    url: 'http://localhost:4173',
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
})
