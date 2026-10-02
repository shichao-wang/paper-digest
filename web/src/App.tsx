import { useLocation } from './hooks'
import LegacyApp from './LegacyApp'
import LibraryApp from './LibraryApp'

export default function App() {
  const { params } = useLocation()
  const legacy = params.has('paperDate') || ['digests', 'legacy'].includes(params.get('view') || '')
  return legacy ? <LegacyApp /> : <LibraryApp />
}
