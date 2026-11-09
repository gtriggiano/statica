// Mermaid's configuration is global (mermaid.initialize), so every diagram on
// the page renders through this one module-level queue, one at a time. A job
// runs even when the previous one failed.
let queue: Promise<unknown> = Promise.resolve()

export function serialize<T>(job: () => Promise<T>): Promise<T> {
  const run = queue.then(job, job)
  queue = run.catch(() => undefined)
  return run
}
