import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'

// ConfigProvider (locale/theme) lives inside App so the language switch can
// re-render it; see i18n.ts and App.tsx.
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
