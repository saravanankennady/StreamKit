"use client";

import { create } from "zustand";
import { api } from "./api";
import type { ProjectSnapshot } from "./types";

// The live feed: one snapshot a second over a WebSocket, straight to the backend because Next.js rewrites cannot
// carry WebSockets. The rolling charts are this browser's memory of those snapshots; the server keeps no short
// history of them. If the socket cannot be opened, the same snapshot is polled over HTTP, so figures stay real
// and the status line says which it is.

/** How many one-second samples the rolling charts keep. */
export const HISTORY_SECONDS = 180;
/** The rolling charts start with a one minute window and grow as history builds up. */
const MIN_WINDOW_MS = 60_000;
const POLL_MS = 3000;
const MAX_BACKOFF_MS = 15000;

export type LiveStatus = "idle" | "connecting" | "live" | "polling" | "offline";

export interface Sample {
  t: number;
  participants: number;
  active_rooms: number;
  connections: number;
  bitrate_in: number;
  bitrate_out: number;
  packets_in_rate: number;
  packets_out_rate: number;
  loss_percent: number;
  rtt_ms: number | null;
  jitter_ms: number | null;
  cpu_percent: number;
  process_cpu_percent: number;
  memory_percent: number;
  process_memory: number;
}

export interface ParticipantSample {
  t: number;
  bitrate_in: number;
  bitrate_out: number;
  rtt_ms: number | null;
  jitter_ms: number | null;
  loss_percent: number;
}

interface LiveState {
  status: LiveStatus;
  projectId: string | null;
  snapshot: ProjectSnapshot | null;
  lastSampleAt: number | null;
  history: Sample[];
  participantHistory: Record<string, ParticipantSample[]>;
  start: (projectId: string) => void;
  stop: () => void;
}

/** The time window for a rolling chart, or nothing before the first sample. */
export function rollingDomain(samples: readonly { t: number }[]): [number, number] | undefined {
  const first = samples[0];
  const last = samples[samples.length - 1];
  if (!first || !last) return undefined;
  return [Math.max(last.t - HISTORY_SECONDS * 1000, Math.min(first.t, last.t - MIN_WINDOW_MS)), last.t];
}

function wsOrigin(): string {
  const configured = process.env.NEXT_PUBLIC_STREAMKIT_WS_ORIGIN;
  if (configured) return configured.replace(/\/+$/, "");
  return window.location.origin.replace(/^http/, "ws");
}

// The server reports RTT and jitter of zero until someone is subscribed, because they are measured on outgoing
// media. Zero here means not measured, so it becomes null and the charts leave a gap.
const measured = (v: number) => (v > 0 ? v : null);

function toSample(s: ProjectSnapshot, t: number): Sample {
  return {
    t,
    participants: s.participants,
    active_rooms: s.active_rooms,
    connections: s.connections,
    bitrate_in: s.bitrate_in,
    bitrate_out: s.bitrate_out,
    packets_in_rate: s.packets_in_rate,
    packets_out_rate: s.packets_out_rate,
    loss_percent: s.packet_loss_ratio * 100,
    rtt_ms: measured(s.avg_rtt_ms),
    jitter_ms: measured(s.avg_jitter_ms),
    cpu_percent: s.node.cpu_percent,
    process_cpu_percent: s.node.process_cpu_percent,
    memory_percent: s.node.memory_total ? (s.node.memory_used / s.node.memory_total) * 100 : 0,
    process_memory: s.node.process_memory,
  };
}

let socket: WebSocket | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let pollTimer: ReturnType<typeof setInterval> | null = null;
let attempts = 0;
let generation = 0;

export const useLive = create<LiveState>((set, get) => {
  function ingest(snapshot: ProjectSnapshot) {
    const t = Date.parse(snapshot.at) || Date.now();
    set((state) => {
      const previous = state.history[state.history.length - 1];
      // Polling and the socket can both deliver the same sample.
      const history = previous && previous.t >= t ? state.history : [...state.history.slice(-(HISTORY_SECONDS - 1)), toSample(snapshot, t)];
      const participantHistory: Record<string, ParticipantSample[]> = {};
      for (const p of snapshot.participant_stats ?? []) {
        const existing = state.participantHistory[p.sid] ?? [];
        const last = existing[existing.length - 1];
        participantHistory[p.sid] =
          last && last.t >= t
            ? existing
            : [
                ...existing.slice(-(HISTORY_SECONDS - 1)),
                { t, bitrate_in: p.bitrate_in, bitrate_out: p.bitrate_out, rtt_ms: measured(p.rtt_ms), jitter_ms: measured(p.jitter_ms), loss_percent: p.loss_ratio * 100 },
              ];
      }
      return { snapshot, lastSampleAt: Date.now(), history, participantHistory };
    });
  }

  function poll(gen: number, projectId: string) {
    api<ProjectSnapshot>("/monitor", { project: projectId })
      .then((snapshot) => {
        if (gen !== generation) return;
        ingest(snapshot);
        // The first fetch only fills the screen while the socket opens. It is polling once the socket has failed.
        if (pollTimer && get().status !== "live") set({ status: "polling" });
      })
      .catch(() => {
        if (gen === generation && pollTimer && get().status !== "live") set({ status: "offline" });
      });
  }

  function startPolling(gen: number, projectId: string) {
    if (pollTimer) return;
    pollTimer = setInterval(() => poll(gen, projectId), POLL_MS);
  }

  function stopPolling() {
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = null;
  }

  function open(gen: number, projectId: string) {
    const ws = new WebSocket(`${wsOrigin()}/api/v1/monitor/live?project_id=${encodeURIComponent(projectId)}`);
    socket = ws;
    ws.onmessage = (event) => {
      if (gen !== generation) return;
      attempts = 0;
      stopPolling();
      if (get().status !== "live") set({ status: "live" });
      try {
        ingest(JSON.parse(event.data as string) as ProjectSnapshot);
      } catch {
        // A frame that cannot be read is skipped; the next one comes in a second.
      }
    };
    ws.onclose = () => {
      if (gen !== generation) return;
      socket = null;
      if (get().status === "live") set({ status: "polling" });
      startPolling(gen, projectId);
      const delay = Math.min(1000 * 2 ** attempts, MAX_BACKOFF_MS);
      attempts += 1;
      reconnectTimer = setTimeout(() => {
        if (gen === generation) open(gen, projectId);
      }, delay);
    };
  }

  function teardown() {
    generation += 1;
    if (reconnectTimer) clearTimeout(reconnectTimer);
    reconnectTimer = null;
    stopPolling();
    const ws = socket;
    socket = null;
    ws?.close();
  }

  return {
    status: "idle",
    projectId: null,
    snapshot: null,
    lastSampleAt: null,
    history: [],
    participantHistory: {},
    start(projectId) {
      if (get().projectId === projectId && get().status !== "idle") return;
      teardown();
      attempts = 0;
      const gen = generation;
      set({ status: "connecting", projectId, snapshot: null, lastSampleAt: null, history: [], participantHistory: {} });
      poll(gen, projectId);
      open(gen, projectId);
    },
    stop() {
      teardown();
      set({ status: "idle", projectId: null, snapshot: null, lastSampleAt: null, history: [], participantHistory: {} });
    },
  };
});
