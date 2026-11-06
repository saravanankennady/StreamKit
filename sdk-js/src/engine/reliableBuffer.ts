// Bounded so a long session cannot grow it without limit. A resume only ever needs the last moments.
const MAX_BUFFERED_BYTES = 8 << 20;
const MAX_BUFFERED_PACKETS = 16384;

export interface BufferedPacket<T> {
  sequence: number;
  bytes: Uint8Array;
  packet: T;
  sent: boolean;
}

/**
 * Reliable packets sent or waiting to be sent, kept until the server confirms receipt on a resume. The
 * server drops anything it already has by sequence, so holding on to more than necessary costs only memory.
 */
export class ReliableBuffer<T> {
  private entries: BufferedPacket<T>[] = [];
  private size = 0;

  push(sequence: number, bytes: Uint8Array, packet: T, sent = false): BufferedPacket<T> {
    const entry = { sequence, bytes, packet, sent };
    this.entries.push(entry);
    this.size += bytes.length;
    while (this.entries.length > MAX_BUFFERED_PACKETS || this.size > MAX_BUFFERED_BYTES) {
      // Packets not sent yet are the ones the application was told would go out, so sent ones leave first.
      const at = this.entries.findIndex((e) => e.sent);
      const [dropped] = this.entries.splice(Math.max(at, 0), 1);
      this.size -= dropped.bytes.length;
    }
    return entry;
  }

  /** Forgets a packet the application was told could not be sent. */
  remove(entry: BufferedPacket<T>) {
    const at = this.entries.indexOf(entry);
    if (at < 0) return;
    this.entries.splice(at, 1);
    this.size -= entry.bytes.length;
  }

  /** Forgets everything the server already has. */
  dropThrough(sequence: number) {
    const keep = this.entries.filter((e) => e.sequence > sequence);
    this.entries = keep;
    this.size = keep.reduce((n, e) => n + e.bytes.length, 0);
  }

  all(): readonly BufferedPacket<T>[] {
    return this.entries;
  }

  /** Empties the buffer and returns the packets that never went out. */
  takeUnsent(): T[] {
    const unsent = this.entries.filter((e) => !e.sent).map((e) => e.packet);
    this.entries = [];
    this.size = 0;
    return unsent;
  }

  get length() {
    return this.entries.length;
  }
}

/** The last sequence seen from each sender. Entries lapse, so a sender gone quiet stops costing memory. */
export class SequenceTracker {
  private last = new Map<string, { sequence: number; expires: number }>();

  constructor(
    private readonly ttlMs = 30_000,
    private readonly now: () => number = Date.now,
  ) {}

  /** Records a packet and reports whether it is new. Senders resend after a resume, and those repeat. */
  accept(sender: string, sequence: number): boolean {
    const now = this.now();
    const entry = this.last.get(sender);
    if (entry && entry.expires > now && sequence <= entry.sequence) return false;
    this.last.set(sender, { sequence, expires: now + this.ttlMs });
    return true;
  }

  /** The live entries, for telling the server what this client already has. */
  states(): Array<{ sender: string; sequence: number }> {
    const now = this.now();
    const out: Array<{ sender: string; sequence: number }> = [];
    for (const [sender, e] of this.last) {
      if (e.expires <= now) this.last.delete(sender);
      else out.push({ sender, sequence: e.sequence });
    }
    return out;
  }

  clear() {
    this.last.clear();
  }
}
