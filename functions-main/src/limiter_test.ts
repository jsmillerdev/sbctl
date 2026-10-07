import { assert, assertEquals } from 'jsr:@std/assert@1'
import { ProjectLimiter } from './limiter.ts'

const admitted = (a: ReturnType<ProjectLimiter['acquire']>) => {
  assert('release' in a, 'expected the request to be admitted')
  return a.release
}

Deno.test('requests in flight are capped per project', () => {
  const l = new ProjectLimiter({ maxRequests: 2, maxWorkers: 0, workerTtlMs: 1000 })
  const a = admitted(l.acquire('a', 'k1'))
  admitted(l.acquire('a', 'k1'))
  assertEquals(l.acquire('a', 'k1'), { refused: 'requests' })
  admitted(l.acquire('other', 'k1')) // another project has its own budget
  a()
  a() // releasing twice frees one slot only
  admitted(l.acquire('a', 'k1'))
  assertEquals(l.acquire('a', 'k1'), { refused: 'requests' })
})

Deno.test('live workers are capped per project and expire after their ttl', () => {
  let t = 0
  const l = new ProjectLimiter({ maxRequests: 0, maxWorkers: 2, workerTtlMs: 1000, now: () => t })
  admitted(l.acquire('a', 'f1'))()
  admitted(l.acquire('a', 'f2'))()
  assertEquals(l.acquire('a', 'f3'), { refused: 'workers' }, 'a third worker is over the cap')
  admitted(l.acquire('a', 'f1')) // an existing worker is reused
  admitted(l.acquire('b', 'f3')) // other projects count on their own
  t = 1500
  admitted(l.acquire('a', 'f3')) // idle workers expired, so there is room
})

Deno.test('a worker stays counted for its ttl after its last request ended', () => {
  let t = 0
  const l = new ProjectLimiter({ maxRequests: 0, maxWorkers: 1, workerTtlMs: 1000, now: () => t })
  const r = admitted(l.acquire('a', 'f1'))
  t = 5000 // a long request: the worker is busy, not idle
  r()
  assertEquals(l.acquire('a', 'f2'), { refused: 'workers' })
  t = 5999
  assertEquals(l.acquire('a', 'f2'), { refused: 'workers' })
  t = 6001
  admitted(l.acquire('a', 'f2'))
})

Deno.test('no cap when both limits are zero', () => {
  const l = new ProjectLimiter({ maxRequests: 0, maxWorkers: 0, workerTtlMs: 1000 })
  for (let i = 0; i < 100; i++) admitted(l.acquire('a', `k${i}`))
})
