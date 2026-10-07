// The adapter between the handler and the Edge Runtime's user-worker API
// (EdgeRuntime.userWorkers, types/global.d.ts of supabase/edge-runtime v1.77.x).
// Only this file touches the global EdgeRuntime object.

import type { Runtime, RuntimeErrors, WorkerOptions } from './types.ts'

// The error classes exist only inside the runtime; stock Deno lacks them.
// deno-lint-ignore no-explicit-any
const errs = Deno.errors as any

const is = (name: string) => (e: unknown): boolean =>
  typeof errs[name] === 'function' && e instanceof errs[name]

export const runtimeErrors: RuntimeErrors = {
  isBootError: is('InvalidWorkerCreation'),
  isRequestCancelled: is('WorkerRequestCancelled'),
  isIdleTimeout: is('WorkerRequestIdleTimeout'),
  isAlreadyRetired: is('WorkerAlreadyRetired'),
  isInvalidResponse: is('InvalidWorkerResponse'),
}

export function edgeRuntime(): Runtime {
  // deno-lint-ignore no-explicit-any
  const rt = (globalThis as any).EdgeRuntime
  return {
    createWorker: (options: WorkerOptions) => rt.userWorkers.create(options),
    applyTag: (src, dest) => rt.applySupabaseTag(src, dest),
    errors: runtimeErrors,
  }
}
