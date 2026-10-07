#!/usr/bin/env node
// Packaging-time fixups applied to the built Next output before the placeholder scan. These edit
// generated files, not Studio's source, so the three-patch rule still holds. Run by build.sh:
//
//   node package-fixups.mjs <app-root>     (app-root contains apps/studio/.next and apps/studio/public)
//
// 1. /api/incident-banner. Studio asks this route for incident.io banners on every page. Without
//    an incident.io key it answers 500, react-query retries it (1 s, 4 s, 16 s) and the sign-in
//    page awaits that query while it resets the query cache, so the redirect after sign-in took
//    21 s in the spike (research/08 section 9). The fixup rewrites the route to a static
//    `{"incidents": []}`, which is what the banner code expects when there is no incident.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

export const INCIDENT_SOURCE = '/api/incident-banner'
export const INCIDENT_DESTINATION = '/sbctl/incident-banner.json'

export function applyFixups(appRoot) {
  const studio = join(appRoot, 'apps', 'studio')
  const manifestPath = join(studio, '.next', 'routes-manifest.json')
  if (!existsSync(manifestPath)) throw new Error(`no routes-manifest.json at ${manifestPath}`)
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
  if (manifest.basePath) throw new Error(`basePath is ${JSON.stringify(manifest.basePath)}; the fixup assumes none`)
  if (!manifest.staticRoutes?.some((r) => r.page === INCIDENT_SOURCE)) {
    throw new Error(`${INCIDENT_SOURCE} is not a route of this build; Studio changed, review this fixup`)
  }
  const before = (manifest.rewrites.beforeFiles ??= [])
  if (!before.some((r) => r.source === INCIDENT_SOURCE)) {
    before.unshift({
      source: INCIDENT_SOURCE,
      destination: INCIDENT_DESTINATION,
      // what Next's path-to-regexp produces for this source (see the other entries of the manifest)
      regex: '^/api/incident\\-banner(?:/)?$',
    })
    writeFileSync(manifestPath, JSON.stringify(manifest))
  }
  const dir = join(studio, 'public', 'sbctl')
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, 'incident-banner.json'), '{"incidents":[]}\n')
}

if (import.meta.url === `file://${process.argv[1]}`) {
  try {
    applyFixups(process.argv[2])
    console.log('package fixups applied')
  } catch (err) {
    console.error(`package-fixups: ${err.message}`)
    process.exit(1)
  }
}
