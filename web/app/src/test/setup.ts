// Loaded by Vitest before every test file: registers jest-dom matchers
// (toBeInTheDocument, etc.) and cleans the DOM between tests.
import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

// jsdom does not implement window.scrollTo; TanStack Router calls it during
// navigation and emits "Not implemented: window.scrollTo" warnings without
// this stub. Replace it with a no-op so test output stays clean.
Object.defineProperty(window, 'scrollTo', { value: () => {}, writable: true })

afterEach(() => {
  cleanup()
})
