import { describe, expect, it } from 'vitest';
import { DefaultReconnectPolicy } from '../src/engine/reconnectPolicy.js';

describe('DefaultReconnectPolicy', () => {
  it('backs off on schedule, then gives up after ten attempts', () => {
    const policy = new DefaultReconnectPolicy(undefined, () => 0.5);
    const delays = Array.from({ length: 11 }, (_, retryCount) => policy.nextRetryDelayInMs({ retryCount, elapsedMs: 0 }));
    expect(delays).toEqual([250, 300, 1200, 2700, 4800, 7000, 7000, 7000, 7000, 7000, null]);
  });
  it('spreads clients out', () => {
    const low = new DefaultReconnectPolicy(undefined, () => 0);
    const high = new DefaultReconnectPolicy(undefined, () => 1);
    expect(low.nextRetryDelayInMs({ retryCount: 0, elapsedMs: 0 })).toBe(0);
    expect(high.nextRetryDelayInMs({ retryCount: 0, elapsedMs: 0 })).toBe(500);
    expect(low.nextRetryDelayInMs({ retryCount: 2, elapsedMs: 0 })).toBe(600);
    expect(high.nextRetryDelayInMs({ retryCount: 2, elapsedMs: 0 })).toBe(1800);
  });
});
