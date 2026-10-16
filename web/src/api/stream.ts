/** A tiny pub/sub hub whose subscribers are async iterables. */
export class Hub<T> {
  private subs = new Set<(v: T) => void>();

  publish(v: T) {
    for (const s of this.subs) s(v);
  }

  /** Yields `initial` first (if given), then every published value until the signal aborts. */
  async *subscribe(signal: AbortSignal, initial?: T[]): AsyncGenerator<T> {
    const queue: T[] = [...(initial ?? [])];
    let wake: (() => void) | undefined;
    const push = (v: T) => {
      queue.push(v);
      wake?.();
    };
    const onAbort = () => wake?.();
    this.subs.add(push);
    signal.addEventListener("abort", onAbort);
    try {
      while (!signal.aborted) {
        if (queue.length) {
          yield queue.shift()!;
          continue;
        }
        await new Promise<void>((r) => (wake = r));
        wake = undefined;
      }
    } finally {
      this.subs.delete(push);
      signal.removeEventListener("abort", onAbort);
    }
  }
}

/** Runs `fn` for every item of the stream, restarting after errors with backoff, until the signal aborts. */
export async function consume<T>(
  open: (signal: AbortSignal) => AsyncIterable<T>,
  fn: (v: T) => void,
  signal: AbortSignal,
  onError?: (e: unknown) => void,
) {
  let delay = 500;
  while (!signal.aborted) {
    try {
      for await (const v of open(signal)) {
        fn(v);
        delay = 500;
      }
      if (signal.aborted) return;
    } catch (e) {
      if (signal.aborted) return;
      onError?.(e);
    }
    await new Promise((r) => setTimeout(r, delay));
    delay = Math.min(delay * 2, 15_000);
  }
}
