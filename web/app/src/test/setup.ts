// Loaded by Vitest before every test file: registers jest-dom matchers
// (toBeInTheDocument, etc.) and cleans the DOM between tests.
import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

afterEach(() => {
  cleanup()
})
