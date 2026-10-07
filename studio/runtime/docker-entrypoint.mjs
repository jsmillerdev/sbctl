#!/usr/bin/env node
// Entrypoint of the sbctl platform-mode Studio artifact. Same contract as the slim-services
// studio artifact (bin/studio runs this file with cwd = app/): read the *_FILE secrets, start
// apps/studio/server.js, forward SIGTERM and SIGINT, mirror the exit status. The one addition is
// that the per-install values are substituted into the build first (see sbctl-runtime-config.mjs).

import { spawn } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { constants as osConstants } from 'node:os'
import { fileURLToPath } from 'node:url'

import { applyRuntimeConfig } from './sbctl-runtime-config.mjs'

function fileEnv(name, defaultValue = '') {
  const value = process.env[name]
  const fileValue = process.env[`${name}_FILE`]

  if (value && fileValue) {
    console.error(`error: both ${name} and ${name}_FILE are set (but are exclusive)`)
    process.exit(1)
  }

  let resolved = defaultValue
  if (value) {
    resolved = value
  } else if (fileValue) {
    resolved = readFileSync(fileValue, 'utf8').trimEnd()
  }

  process.env[name] = resolved
  delete process.env[`${name}_FILE`]
}

fileEnv('POSTGRES_PASSWORD')
fileEnv('SUPABASE_ANON_KEY')
fileEnv('SUPABASE_SERVICE_KEY')

// This file lives at <root>/app/apps/studio/docker-entrypoint.mjs.
const root = fileURLToPath(new URL('../../..', import.meta.url))
try {
  Object.assign(process.env, applyRuntimeConfig(root, process.env))
} catch (err) {
  console.error(`sbctl-studio: ${err.message}`)
  // 78 = EX_CONFIG: bad operator input, systemd should not restart-loop on it quickly.
  process.exit(err.configError ? 78 : 1)
}

const args = process.argv.slice(2)
const command = args.length > 0 ? args[0] : process.execPath
const commandArgs = args.length > 0 ? args.slice(1) : ['apps/studio/server.js']

const child = spawn(command, commandArgs, {
  env: process.env,
  stdio: 'inherit',
})

const forwardSignal = (signal) => {
  if (!child.killed) {
    child.kill(signal)
  }
}

process.on('SIGTERM', forwardSignal)
process.on('SIGINT', forwardSignal)

child.on('exit', (code, signal) => {
  if (signal) {
    const signalNumber = osConstants.signals[signal] ?? 1
    process.exit(128 + signalNumber)
  }

  process.exit(code ?? 1)
})
