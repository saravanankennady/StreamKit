import { StatusBadge, type Tone } from "@/components/ui/badge";
import type { RoomState } from "@/lib/types";

const roomStates: Record<RoomState, { tone: Tone; label: string; title: string }> = {
  creating: { tone: "neutral", label: "Waiting", title: "Created, and nobody has joined yet. It closes when the empty timeout runs out." },
  active: { tone: "good", label: "Active", title: "Someone is in this room." },
  empty: { tone: "neutral", label: "Empty", title: "Everyone left. It closes when the departure timeout runs out." },
  closing: { tone: "warn", label: "Closing", title: "Disconnecting everyone." },
  closed: { tone: "neutral", label: "Closed", title: "Closed." },
  failed: { tone: "critical", label: "Failed", title: "The room failed." },
};

export function RoomStateBadge({ state }: { state: RoomState }) {
  const s = roomStates[state] ?? { tone: "neutral" as const, label: state, title: "" };
  return (
    <StatusBadge tone={s.tone} title={s.title}>
      {s.label}
    </StatusBadge>
  );
}

export function QualityBadge({ quality }: { quality: string }) {
  const map: Record<string, { tone: Tone; label: string }> = {
    EXCELLENT: { tone: "good", label: "Excellent" },
    GOOD: { tone: "good", label: "Good" },
    POOR: { tone: "serious", label: "Poor" },
    LOST: { tone: "critical", label: "Lost" },
  };
  const q = map[quality] ?? { tone: "neutral" as const, label: "Unknown" };
  return <StatusBadge tone={q.tone}>{q.label}</StatusBadge>;
}

export function ParticipantStateBadge({ state }: { state: string }) {
  const map: Record<string, { tone: Tone; label: string }> = {
    ACTIVE: { tone: "good", label: "Active" },
    JOINED: { tone: "neutral", label: "Joined" },
    JOINING: { tone: "neutral", label: "Joining" },
    DISCONNECTED: { tone: "serious", label: "Disconnected" },
  };
  const s = map[state] ?? { tone: "neutral" as const, label: state.toLowerCase() };
  return <StatusBadge tone={s.tone}>{s.label}</StatusBadge>;
}
