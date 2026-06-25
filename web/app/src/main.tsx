import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@mitos/brand/base.css'
import { App } from './App'
import { applyAppearanceOnLoad } from './appearance'

applyAppearanceOnLoad()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
