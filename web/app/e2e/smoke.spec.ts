import { test, expect } from '@playwright/test'

// One smoke: the SPA boots, the shell nav renders, and Cmd-K opens the palette.
// Requires a backing console serving /console/capabilities; intended for CI
// against `cmd/console -dev`. Running locally without that binary will fail.
test('console boots and the command palette opens', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('mitos')).toBeVisible()
  await page.keyboard.press('Meta+k')
  await expect(page.getByLabel('Command palette input')).toBeVisible()
})

test('mobile: menu button opens the nav drawer', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/')
  const menu = page.getByRole('button', { name: /menu/i })
  await expect(menu).toBeVisible()
  await menu.click()
  await expect(page.getByRole('navigation', { name: /primary/i })).toBeVisible()
})
