export interface ReconnectContext {
  /** How many attempts have already failed. */
  retryCount: number;
  elapsedMs: number;
}

/** Decides how long to wait before each reconnect attempt. Null means give up. */
export interface ReconnectPolicy {
  nextRetryDelayInMs(context: ReconnectContext): number | null;
}

// Quick at first, because most losses are brief, then steady. Attempts total about 40 seconds, well past
// the time a server keeps a session open for a client to return.
const DEFAULT_DELAYS_MS = [0, 300, 1200, 2700, 4800, 7000, 7000, 7000, 7000, 7000];

export class DefaultReconnectPolicy implements ReconnectPolicy {
  constructor(
    private readonly delays: number[] = DEFAULT_DELAYS_MS,
    private readonly random: () => number = Math.random,
  ) {}

  nextRetryDelayInMs(context: ReconnectContext): number | null {
    if (context.retryCount >= this.delays.length) return null;
    const delay = this.delays[context.retryCount];
    // Spread clients out, so a server restart does not bring them all back in the same instant.
    if (context.retryCount === 0) return delay + this.random() * 500;
    return delay * (0.5 + this.random());
  }
}
