// Resizes the raw 2880x1800 captures to 1600 px wide and compresses them to palette PNGs under
// MAX_KB each (stepping the palette quality down until they fit). Nothing else is changed.
//
//   node optimize.mjs <raw dir> <out dir>
import { existsSync, mkdirSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'

import sharp from 'sharp'

const [src, dst] = process.argv.slice(2)
const WIDTH = Number(process.env.WIDTH ?? 1600)
const MAX_KB = Number(process.env.MAX_KB ?? 380)
if (!existsSync(src)) { console.log(`no ${src}: nothing to optimize`); process.exit(0) }
mkdirSync(dst, { recursive: true })
let total = 0
for (const f of readdirSync(src).filter((n) => n.endsWith('.png')).sort()) {
  const resized = await sharp(join(src, f)).resize({ width: WIDTH, kernel: 'lanczos3' }).toBuffer()
  let out
  for (const [quality, colours] of [[95, 256], [90, 256], [85, 192], [80, 128], [70, 96], [60, 64]]) {
    out = join(dst, f)
    await sharp(resized).png({ palette: true, quality, colours, effort: 10, compressionLevel: 9, dither: 0.6 }).toFile(out)
    if (statSync(out).size <= MAX_KB * 1024) break
  }
  const kb = Math.round(statSync(out).size / 1024)
  total += kb
  console.log(`${f}: ${kb} KB${kb > MAX_KB ? '  (over the limit)' : ''}`)
}
console.log(`total ${total} KB`)
