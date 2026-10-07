import { assert, assertEquals } from 'jsr:@std/assert@1'
import { ProjectLimiter } from './limiter.ts'

const admitted = (a: ReturnType<ProjectLimiter['acquire']>) => {
  assert('release' in a, 'expected the request to be admitted')
  return a.release
}

const opts = { maxRequests: 0, maxWorkers: 0, workerTtlMs: 1000 }

Deno.test('requests in flight are capped per project', () => {
  const l = new ProjectLimiter({ ...opts, maxRequests: 2 })
  const a = admitted(l.acquire('a', 'a:k1'))
  admitted(l.acquire('a', 'a:k1'))
  assertEquals(l.acquire('a', 'a:k1'), { refused: 'requests' })
  admitted(l.acquire('other', 'other:k1')) // another project has its own budget
  a()
  a() // releasing twice frees one slot only
  admitted(l.acquire('a', 'a:k1'))
  assertEquals(l.acquire('a', 'a:k1'), { refused: 'requests' })
})

Deno.test('live workers are capped per project and expire after their ttl', () => {
  let t = 0
  const l = new ProjectLimiter({ ...opts, maxWorkersPerProject: 2, now: () => t })
  admitted(l.acquire('a', 'a:f1'))()
  admitted(l.acquire('a', 'a:f2'))()
  assertEquals(l.acquire('a', 'a:f3'), { refused: 'project-workers' })
  admitted(l.acquire('a', 'a:f1')) // an existing worker is reused
  admitted(l.acquire('b', 'b:f3')) // other projects count on their own
  t = 1500
  admitted(l.acquire('a', 'a:f3')) // idle workers expired, so there is room
})

Deno.test('the budget of the whole runtime is shared by all projects', () => {
  const l = new ProjectLimiter({ ...opts, maxWorkers: 3 })
  admitted(l.acquire('a', 'a:f1'))
  admitted(l.acquire('a', 'a:f2'))
  admitted(l.acquire('b', 'b:f1'))
  assertEquals(l.acquire('b', 'b:f2'), { refused: 'workers' })
  assertEquals(l.acquire('c', 'c:f1'), { refused: 'workers' })
  admitted(l.acquire('b', 'b:f1')) // warm workers keep serving
  assertEquals(l.liveWorkers(), 3)
})

Deno.test('many projects each warming many functions never exceed the budget', () => {
  let t = 0
  const budget = 16
  const l = new ProjectLimiter({
    ...opts,
    maxWorkers: budget,
    maxWorkersPerProject: 8,
    now: () => t,
  })
  let admittedKeys = 0
  for (let round = 0; round < 3; round++) {
    for (let p = 0; p < 10; p++) {
      for (let f = 0; f < 12; f++) {
        const a = l.acquire(`p${p}`, `p${p}:f${f}:${round}`)
        if ('release' in a) {
          admittedKeys++
          a.release()
        }
        assert(l.liveWorkers() <= budget, `${l.liveWorkers()} live workers, budget ${budget}`)
      }
    }
    t += 5000 // everything idles out between rounds
  }
  assertEquals(admittedKeys, 3 * budget, 'each round fills the budget exactly')
})

Deno.test('a worker stays counted for its ttl after its last request ended', () => {
  let t = 0
  const l = new ProjectLimiter({ ...opts, maxWorkers: 1, now: () => t })
  const r = admitted(l.acquire('a', 'a:f1'))
  t = 5000 // a long request: the worker is busy, not idle
  assertEquals(l.acquire('a', 'a:f2'), { refused: 'workers' }, 'busy beyond the ttl is still live')
  r()
  assertEquals(l.acquire('a', 'a:f2'), { refused: 'workers' })
  t = 5999
  assertEquals(l.acquire('a', 'a:f2'), { refused: 'workers' })
  t = 6001
  admitted(l.acquire('a', 'a:f2'))
})

Deno.test('bundle bytes of the functions in flight are charged once per function', () => {
  const l = new ProjectLimiter({ ...opts, maxBundleBytes: 100 })
  const r1 = admitted(l.acquire('a', 'a:f1', 60))
  admitted(l.acquire('a', 'a:f1', 60)) // the same function: its bundle is held once
  assertEquals(l.acquire('a', 'a:f2', 60), { refused: 'bundle-bytes' })
  admitted(l.acquire('b', 'b:f1', 60)) // another project has its own allowance
  admitted(l.acquire('a', 'a:f3', 40))
  r1() // one request of f1 is still in flight
  assertEquals(l.acquire('a', 'a:f2', 60), { refused: 'bundle-bytes' })
})

Deno.test('no cap when every limit is zero', () => {
  const l = new ProjectLimiter(opts)
  for (let i = 0; i < 100; i++) admitted(l.acquire('a', `a:k${i}`, 1 << 30))
})
