import { test, expect } from '@playwright/test'

// One smoke: the SPA boots, the shell nav renders, and Cmd-K opens the palette.
// Capabilities are served by the embedded default (community edition) when run
// against the console binary; against `vite preview` the dev proxy is absent, so
// this test is marked to run only when a backing console is reachable.
test('console boots and the command palette opens', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('mitos')).toBeVisible()
  await page.keyboard.press('Meta+k')
  await expect(page.getByLabel('Command palette input')).toBeVisible()
})
