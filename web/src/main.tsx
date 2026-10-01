import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource/literata/latin-500.css'
import '@fontsource/literata/latin-600.css'
import App from './App'
import './styles.css'

createRoot(document.getElementById('root')!).render(<StrictMode><App /></StrictMode>)
