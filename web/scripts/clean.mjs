// Removes the previous build without touching .gitkeep, so the Go embed
// directive always has a file to match.
import { rmSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const dist = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'internal', 'ui', 'dist')
rmSync(join(dist, 'assets'), { recursive: true, force: true })
rmSync(join(dist, 'index.html'), { force: true })
rmSync(join(dist, 'favicon.svg'), { force: true })
