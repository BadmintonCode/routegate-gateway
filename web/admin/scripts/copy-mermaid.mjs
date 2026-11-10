// Copy the pinned Mermaid browser bundle into public/ so the panel can
// lazy-load it at runtime instead of pulling the whole Mermaid module graph
// (~300 modules incl. elk/cytoscape/katex) through the Vite/Rollup build —
// that graph OOMs the 1 GB Release Console build container (see AI-CHANGE-LOG
// 20261109-routegate-markdown-build-fix). The copied asset is gitignored;
// the version stays pinned by package.json.
import { copyFileSync, existsSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = dirname(dirname(fileURLToPath(import.meta.url)))
const src = join(root, 'node_modules', 'mermaid', 'dist', 'mermaid.min.js')
const destDir = join(root, 'public')

if (!existsSync(src)) {
  console.error('copy-mermaid: node_modules/mermaid/dist/mermaid.min.js not found — run npm install first')
  process.exit(1)
}
mkdirSync(destDir, { recursive: true })
copyFileSync(src, join(destDir, 'mermaid.min.js'))
console.log('copy-mermaid: public/mermaid.min.js updated')
