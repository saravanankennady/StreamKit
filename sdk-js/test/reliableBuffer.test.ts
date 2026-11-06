import { describe, expect, it } from 'vitest';
import { ReliableBuffer, SequenceTracker } from '../src/engine/reliableBuffer.js';

const bytes = (n: number) => new Uint8Array(n);

describe('ReliableBuffer', () => {
  it('forgets what the server has confirmed', () => {
    const b = new ReliableBuffer<string>();
    for (let i = 1; i <= 5; i++) b.push(i, bytes(1), `p${i}`);
    b.dropThrough(3);
    expect(b.all().map((e) => e.sequence)).toEqual([4, 5]);
  });
  it('hands back only unsent packets when a session ends', () => {
    const b = new ReliableBuffer<string>();
    b.push(1, bytes(1), 'a', true);
    b.push(2, bytes(1), 'b', false);
    b.push(3, bytes(1), 'c', false);
    expect(b.takeUnsent()).toEqual(['b', 'c']);
    expect(b.length).toBe(0);
  });
  it('is bounded, and gives up sent packets before unsent ones', () => {
    const b = new ReliableBuffer<number>();
    const big = bytes(1 << 20);
    b.push(1, big, 1, false);
    for (let i = 2; i <= 9; i++) b.push(i, big, i, true);
    b.push(10, big, 10, true);
    const kept = b.all().map((e) => e.sequence);
    expect(kept).toContain(1);
    expect(kept.length).toBeLessThanOrEqual(8);
    expect(kept).toContain(10);
  });
});

describe('SequenceTracker', () => {
  it('accepts increasing sequences, per sender', () => {
    const t = new SequenceTracker();
    expect(t.accept('a', 1)).toBe(true);
    expect(t.accept('a', 2)).toBe(true);
    expect(t.accept('a', 2)).toBe(false);
    expect(t.accept('a', 1)).toBe(false);
    expect(t.accept('b', 1)).toBe(true);
  });
  it('forgets a sender after a while', () => {
    let now = 0;
    const t = new SequenceTracker(30_000, () => now);
    t.accept('a', 10);
    now = 29_999;
    expect(t.accept('a', 5)).toBe(false);
    now = 30_000;
    expect(t.states()).toEqual([]);
    expect(t.accept('a', 5)).toBe(true);
  });
  it('an accepted packet extends the time', () => {
    let now = 0;
    const t = new SequenceTracker(30_000, () => now);
    t.accept('a', 1);
    now = 20_000;
    t.accept('a', 2);
    now = 40_000;
    expect(t.states()).toEqual([{ sender: 'a', sequence: 2 }]);
  });
});
