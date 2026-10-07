// sbctl's main service for the Edge Runtime: the one script every request enters.
// It serves all projects of a node; see README.md.

import { loadConfig } from './src/config.ts'
import { makeHandler } from './src/handler.ts'
import { ProjectStore } from './src/projects.ts'
import { edgeRuntime } from './src/runtime.ts'

const config = loadConfig((name) => Deno.env.get(name))
const handler = makeHandler({
  store: new ProjectStore(config.projectsDir),
  runtime: edgeRuntime(),
  limits: config.limits,
  port: Deno.env.get('EDGE_RUNTIME_PORT') ?? '',
})

console.log(`sbctl functions main service started (projects in ${config.projectsDir})`)
Deno.serve(handler)
