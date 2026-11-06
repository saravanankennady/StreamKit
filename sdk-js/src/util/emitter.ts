import { log } from '../logger.js';

type Listener = (...args: any[]) => void;
type EventMap = { [event: string]: Listener };

/** A small event emitter with typed events. A listener that throws cannot stop the SDK's own work. */
export class TypedEmitter<E extends EventMap> {
  private listeners = new Map<keyof E, Listener[]>();

  on<K extends keyof E>(event: K, listener: E[K]): this {
    const list = this.listeners.get(event);
    if (list) list.push(listener);
    else this.listeners.set(event, [listener]);
    return this;
  }

  once<K extends keyof E>(event: K, listener: E[K]): this {
    const wrapped = ((...args: unknown[]) => {
      this.off(event, wrapped as E[K]);
      listener(...args);
    }) as E[K];
    return this.on(event, wrapped);
  }

  off<K extends keyof E>(event: K, listener: E[K]): this {
    const list = this.listeners.get(event);
    if (!list) return this;
    const at = list.indexOf(listener);
    if (at >= 0) list.splice(at, 1);
    if (list.length === 0) this.listeners.delete(event);
    return this;
  }

  emit<K extends keyof E>(event: K, ...args: Parameters<E[K]>): boolean {
    const list = this.listeners.get(event);
    if (!list) return false;
    // Copied, because a listener can remove itself while this runs.
    for (const listener of [...list]) {
      try {
        listener(...args);
      } catch (e) {
        log.error(`listener for "${String(event)}" threw`, e);
      }
    }
    return true;
  }

  removeAllListeners<K extends keyof E>(event?: K): this {
    if (event === undefined) this.listeners.clear();
    else this.listeners.delete(event);
    return this;
  }

  listenerCount<K extends keyof E>(event: K): number {
    return this.listeners.get(event)?.length ?? 0;
  }
}
