// supavise's main service for the Edge Runtime: the one script every request enters.
// It serves all projects of a node; see README.md.

import { loadConfig } from './src/config.ts'
import { makeHandler } from './src/handler.ts'
import { ProjectStore } from './src/projects.ts'
import { edgeRuntime } from './src/runtime.ts'

const config = loadConfig((name) => Deno.env.get(name))
const handler = makeHandler({
  store: new ProjectStore(config.root),
  runtime: edgeRuntime(),
  limits: config.limits,
  port: Deno.env.get('EDGE_RUNTIME_PORT') ?? '',
  proxyToken: config.proxyToken,
})

console.log(`supavise functions main service started (functions in ${config.root})`)
Deno.serve(handler)
