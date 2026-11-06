import { useCallback, useRef, useSyncExternalStore } from 'react';

interface Emitter {
  on(event: any, listener: (...args: any[]) => void): unknown;
  off(event: any, listener: (...args: any[]) => void): unknown;
}

/**
 * Re-renders when any of an emitter's events fire. The value returned changes each time, for use as a
 * dependency of whatever reads the emitter's state. State always comes from the SDK at render time, so
 * there is no copy here to go out of date.
 */
export function useEventVersion(emitter: Emitter | undefined, events: readonly string[]): number {
  const version = useRef(0);
  const key = events.join(' ');
  const subscribe = useCallback(
    (onChange: () => void) => {
      if (!emitter) return () => {};
      const handler = () => {
        version.current++;
        onChange();
      };
      const names = key === '' ? [] : key.split(' ');
      for (const e of names) emitter.on(e, handler);
      // Something could have happened between rendering and getting here.
      handler();
      return () => {
        for (const e of names) emitter.off(e, handler);
      };
    },
    [emitter, key],
  );
  return useSyncExternalStore(
    subscribe,
    () => version.current,
    () => 0,
  );
}
