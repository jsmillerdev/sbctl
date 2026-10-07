#!/usr/bin/env node
// Packaging helper for build.sh: node prepare-cli.mjs <placeholders.json> <app-root> <runtime-config.json>
// Stores a pristine `.supavise-tpl` copy of every file that carries a placeholder and writes the list
// the launcher uses at start. Exits non-zero if a placeholder is missing from the build output.
import { writeFileSync } from 'node:fs'

import { prepare, readPlaceholders } from './supavise-runtime-config.mjs'

const [placeholders, appRoot, out] = process.argv.slice(2)
if (!placeholders || !appRoot || !out) {
  console.error('usage: prepare-cli.mjs <placeholders.json> <app-root> <runtime-config.json>')
  process.exit(2)
}
try {
  const spec = readPlaceholders(placeholders)
  const files = prepare(appRoot, spec)
  writeFileSync(out, JSON.stringify({ version: spec.version, values: spec.values, files }, null, 1) + '\n')
  console.log(`${files.length} files carry placeholders`)
} catch (err) {
  console.error(`prepare: ${err.message}`)
  process.exit(1)
}
