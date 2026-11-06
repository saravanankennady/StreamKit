"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Mic, MicOff, UserMinus, Video, VideoOff } from "lucide-react";
import Link from "next/link";
import * as React from "react";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Table, Td, Th, Tr } from "@/components/ui/table";
import { toast, toastError } from "@/components/ui/toast";
import { Tooltip } from "@/components/ui/tooltip";
import { ApiError } from "@/lib/api";
import { formatBitrate, formatDuration } from "@/lib/format";
import { useProject, useProjectApi } from "@/lib/session";
import type { ParticipantStat, TrackStat } from "@/lib/types";
import { ParticipantStateBadge, QualityBadge } from "./status";

export const participantHref = (room: string, identity: string) => `/participants/${encodeURIComponent(room)}/${encodeURIComponent(identity)}`;

export const trackPath = (room: string, identity: string, sid: string) => `/tracks/${encodeURIComponent(room)}/${encodeURIComponent(identity)}/${encodeURIComponent(sid)}/mute`;

/** Stated wherever someone might try to unmute: remote unmute is off by default, and the API cannot report the setting. */
export function UnmuteNote() {
  return (
    <Callout tone="info" title="Muting always works. Unmuting from here usually does not.">
      Turning on someone&rsquo;s microphone remotely is off unless the server sets <code className="mono">room.enable_remote_unmute: true</code>. Without it the server answers 412, and you will see that message. Ask the person to unmute themselves.
    </Callout>
  );
}

export function useMuteTrack(room: string, identity: string) {
  const call = useProjectApi();
  const { projectId } = useProject();
  const qc = useQueryClient();
  return useMutation<{ sid: string; muted: boolean }, ApiError, { sid: string; muted: boolean }>({
    mutationFn: ({ sid, muted }) => call(trackPath(room, identity, sid), { method: "POST", body: { muted } }),
    onSuccess: (res) => {
      toast(res.muted ? "Track muted" : "Track unmuted", `${identity} was told.`);
      void qc.invalidateQueries({ queryKey: [projectId, "participant", room, identity] });
    },
    onError: (err, vars) => toastError(err, vars.muted ? "Could not mute" : "Could not unmute"),
  });
}

/** One microphone or camera, with a button to mute all of that kind. Empty when they publish none. */
function KindToggle({ participant, kind }: { participant: ParticipantStat; kind: "audio" | "video" }) {
  const mute = useMuteTrack(participant.room, participant.identity);
  const tracks = participant.tracks.filter((t) => t.kind === kind);
  if (tracks.length === 0) return <span className="text-xs text-ink-3">None</span>;
  const muted = kind === "audio" ? participant.audio_muted : participant.video_muted;
  const Icon = kind === "audio" ? (muted ? MicOff : Mic) : muted ? VideoOff : Video;
  const word = kind === "audio" ? "audio" : "video";
  return (
    <Tooltip content={muted ? `Muted. Click to try unmuting ${participant.identity}.` : `Publishing. Click to mute ${participant.identity}.`}>
      <Button
        variant={muted ? "danger" : "secondary"}
        size="sm"
        pending={mute.isPending}
        data-testid={`toggle-${kind}`}
        aria-label={`${muted ? "Unmute" : "Mute"} ${word} of ${participant.identity}`}
        onClick={() => tracks.forEach((t) => mute.mutate({ sid: t.sid, muted: !muted }))}
      >
        <Icon aria-hidden />
        {muted ? "Muted" : "On"}
      </Button>
    </Tooltip>
  );
}

export function RemoveParticipant({ participant, onRemoved, label }: { participant: { room: string; identity: string }; onRemoved?: () => void; label?: string }) {
  const [open, setOpen] = React.useState(false);
  const call = useProjectApi();
  return (
    <>
      <Tooltip content={`Remove ${participant.identity} from the room`}>
        <Button variant="ghost" size={label ? "md" : "iconSm"} aria-label={label ? undefined : `Remove ${participant.identity}`} data-testid="remove-participant" onClick={() => setOpen(true)}>
          <UserMinus aria-hidden />
          {label}
        </Button>
      </Tooltip>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title={`Remove ${participant.identity}?`}
        description={`They are disconnected from ${participant.room} at once. Their token still works, so they can rejoin unless your app stops them.`}
        confirmLabel="Remove participant"
        onConfirm={async () => {
          await call(`/participants/${encodeURIComponent(participant.room)}/${encodeURIComponent(participant.identity)}`, { method: "DELETE" });
          toast(`${participant.identity} removed`);
          onRemoved?.();
        }}
      />
    </>
  );
}

export function ParticipantsTable({ participants, now, showRoom = true }: { participants: ParticipantStat[]; now: number; showRoom?: boolean }) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Identity</Th>
          {showRoom && <Th className="hidden md:table-cell">Room</Th>}
          <Th className="hidden xl:table-cell">SID</Th>
          <Th className="hidden sm:table-cell">Status</Th>
          <Th className="text-right">
            <span title="Tracks they publish">Pub.</span>
          </Th>
          <Th className="text-right">
            <span title="Tracks they receive">Sub.</span>
          </Th>
          <Th>Audio</Th>
          <Th>Video</Th>
          <Th>Network</Th>
          <Th className="hidden text-right lg:table-cell">In</Th>
          <Th className="hidden text-right lg:table-cell">Out</Th>
          <Th className="hidden text-right md:table-cell">Joined for</Th>
          <Th className="w-8">
            <span className="sr-only">Remove</span>
          </Th>
        </tr>
      </thead>
      <tbody>
        {participants.map((p) => (
          <Tr key={p.sid} data-testid="participant-row" data-identity={p.identity}>
            <Td className="max-w-44">
              <Link className="link block truncate font-medium" href={participantHref(p.room, p.identity)}>
                {p.identity}
              </Link>
              {p.name && p.name !== p.identity && <div className="truncate text-xs text-ink-3">{p.name}</div>}
            </Td>
            {showRoom && (
              <Td className="hidden max-w-40 truncate md:table-cell">
                <Link className="link" href={`/rooms/${encodeURIComponent(p.room)}`}>
                  {p.room}
                </Link>
              </Td>
            )}
            <Td className="mono hidden text-xs text-ink-2 xl:table-cell">{p.sid}</Td>
            <Td className="hidden sm:table-cell">
              <ParticipantStateBadge state={p.state} />
            </Td>
            <Td className="num text-right">{p.tracks.length}</Td>
            <Td className="num text-right">{p.subscribed_count}</Td>
            <Td>
              <KindToggle participant={p} kind="audio" />
            </Td>
            <Td>
              <KindToggle participant={p} kind="video" />
            </Td>
            <Td>
              <QualityBadge quality={p.quality} />
            </Td>
            <Td className="num hidden text-right whitespace-nowrap lg:table-cell" data-testid="participant-bitrate-in" data-value={p.bitrate_in}>
              {formatBitrate(p.bitrate_in)}
            </Td>
            <Td className="num hidden text-right whitespace-nowrap lg:table-cell">{formatBitrate(p.bitrate_out)}</Td>
            <Td className="num hidden text-right whitespace-nowrap md:table-cell">{formatDuration(Math.max(0, now / 1000 - p.joined_at))}</Td>
            <Td className="w-8 px-1 text-right">
              <RemoveParticipant participant={p} />
            </Td>
          </Tr>
        ))}
      </tbody>
    </Table>
  );
}

export function TracksTable({ participant, tracks }: { participant: { room: string; identity: string }; tracks: TrackStat[] }) {
  const mute = useMuteTrack(participant.room, participant.identity);
  return (
    <Table>
      <thead>
        <tr>
          <Th>Track</Th>
          <Th>Source</Th>
          <Th className="hidden sm:table-cell">Codec</Th>
          <Th className="text-right">Bitrate</Th>
          <Th className="hidden text-right md:table-cell">Resolution</Th>
          <Th className="hidden text-right md:table-cell">FPS</Th>
          <Th className="hidden text-right lg:table-cell">Layers</Th>
          <Th className="hidden text-right lg:table-cell">Packets in</Th>
          <Th className="hidden text-right lg:table-cell">Lost</Th>
          <Th className="hidden text-right sm:table-cell">Subscribers</Th>
          <Th>State</Th>
        </tr>
      </thead>
      <tbody>
        {tracks.map((t) => (
          <Tr key={t.sid} data-testid="track-row" data-kind={t.kind}>
            <Td className="mono text-xs text-ink-2">{t.sid}</Td>
            <Td className="whitespace-nowrap capitalize">{t.source.toLowerCase().replace(/_/g, " ")}</Td>
            <Td className="hidden whitespace-nowrap text-ink-2 sm:table-cell">{t.mime_type.split("/")[1] ?? t.mime_type}</Td>
            <Td className="num text-right whitespace-nowrap">{formatBitrate(t.bitrate_in)}</Td>
            <Td className="num hidden text-right whitespace-nowrap md:table-cell">{t.kind === "video" && t.width ? `${t.width}×${t.height}` : "–"}</Td>
            <Td className="num hidden text-right md:table-cell">{t.kind === "video" ? t.fps : "–"}</Td>
            <Td className="num hidden text-right lg:table-cell">{t.layers}</Td>
            <Td className="num hidden text-right lg:table-cell">{t.packets_in.toLocaleString("en")}</Td>
            <Td className="num hidden text-right lg:table-cell">{t.packets_lost.toLocaleString("en")}</Td>
            <Td className="num hidden text-right sm:table-cell">{t.subscribers}</Td>
            <Td>
              <Button
                size="sm"
                variant={t.muted ? "danger" : "secondary"}
                pending={mute.isPending && mute.variables?.sid === t.sid}
                data-testid="track-mute"
                aria-label={`${t.muted ? "Unmute" : "Mute"} ${t.source.toLowerCase()} track`}
                onClick={() => mute.mutate({ sid: t.sid, muted: !t.muted })}
              >
                {t.muted ? "Muted, unmute" : "On, mute"}
              </Button>
            </Td>
          </Tr>
        ))}
      </tbody>
    </Table>
  );
}
