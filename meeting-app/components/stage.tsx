"use client";

import { ConnectionQuality, ParticipantEvent, type Participant, TrackSource } from "@elnova-streamkit/client";
import {
  isTrackReference,
  ParticipantTile,
  type TrackReferenceOrPlaceholder,
  getTrackReferenceId,
  useActiveSpeakers,
  useConnectionQuality,
  useEnsureTrackRef,
  useIsSpeaking,
  useParticipantTrack,
  useTracks,
  VideoTrack,
} from "@elnova-streamkit/react";
import { AudioLines, Hand, MicOff, Shield, SignalHigh, SignalLow, SignalMedium, SignalZero, WifiOff } from "lucide-react";
import { useRef } from "react";
import { handRaised, initials, roleOf, useEvents } from "@/lib/meeting";

const TILE_SOURCES = [
  { source: TrackSource.Camera, withPlaceholder: true },
  { source: TrackSource.ScreenShare, withPlaceholder: false },
];

const ATTRIBUTE_EVENTS = [ParticipantEvent.AttributesChanged];

const QUALITY_TEXT: Record<ConnectionQuality, string> = {
  [ConnectionQuality.Excellent]: "Excellent",
  [ConnectionQuality.Good]: "Good",
  [ConnectionQuality.Poor]: "Weak",
  [ConnectionQuality.Lost]: "Lost",
  [ConnectionQuality.Unknown]: "Not measured yet",
};

/** Says what the server last measured for someone's connection. Before its first report, that is nothing. */
export function qualityText(quality: ConnectionQuality): string {
  return QUALITY_TEXT[quality] ?? QUALITY_TEXT[ConnectionQuality.Unknown];
}

/** The server's measurement of a connection: the shape of the icon and its name say it, as well as the colour. */
export function QualityIcon({ quality }: { quality: ConnectionQuality }) {
  const label = `Connection: ${qualityText(quality)}`;
  const props = { "aria-label": label, role: "img", className: "size-3 shrink-0" };
  switch (quality) {
    case ConnectionQuality.Excellent:
      return <SignalHigh {...props} />;
    case ConnectionQuality.Good:
      return <SignalMedium {...props} />;
    case ConnectionQuality.Poor:
      return <SignalLow {...props} className="size-3 shrink-0 text-warn-text" />;
    case ConnectionQuality.Lost:
      return <WifiOff {...props} className="size-3 shrink-0 text-critical-text" />;
    default:
      return <SignalZero {...props} className="size-3 shrink-0 text-ink-3" />;
  }
}

function TileBody() {
  const ref = useEnsureTrackRef();
  const p = ref.participant;
  useEvents(p, ATTRIBUTE_EVENTS);
  const name = p.name || p.identity;
  const isScreen = ref.source === TrackSource.ScreenShare;
  const video = useParticipantTrack(p, ref.source, ref.publication?.trackSid);
  const audio = useParticipantTrack(p, TrackSource.Microphone);
  const quality = useConnectionQuality(p);
  const speaking = useIsSpeaking(p);
  const showVideo = isTrackReference(ref) && video.isSubscribed && !video.isMuted;
  const weak = quality === ConnectionQuality.Poor || quality === ConnectionQuality.Lost;
  const role = roleOf(p);
  return (
    <>
      {showVideo ? (
        <VideoTrack trackRef={ref} className={`absolute inset-0 size-full ${isScreen ? "object-contain" : "object-cover"} ${p.isLocal && !isScreen ? "-scale-x-100" : ""}`} />
      ) : (
        <div className="absolute inset-0 flex items-center justify-center">
          <span aria-hidden className="flex size-16 items-center justify-center rounded-full bg-raised text-xl font-medium">
            {initials(name)}
          </span>
        </div>
      )}
      {!isScreen && handRaised(p) && (
        <span data-testid="tile-hand" className="absolute left-2 top-2 flex items-center gap-1 rounded bg-signal px-2 py-1 text-xs font-medium text-signal-ink">
          <Hand aria-hidden className="size-3" /> Hand raised
        </span>
      )}
      <div className="absolute inset-x-2 bottom-2 flex items-center gap-1.5">
        <span className="flex min-w-0 items-center gap-1.5 rounded bg-black/70 px-2 py-1 text-xs">
          {!isScreen && (audio.isMuted || !audio.publication) && <MicOff role="img" aria-label="Microphone off" data-testid="tile-muted" className="size-3 shrink-0" />}
          {!isScreen && speaking && <AudioLines role="img" aria-label="Speaking" data-testid="tile-speaking" className="size-3 shrink-0 text-signal" />}
          {!isScreen && (
            <span data-testid="tile-quality" data-quality={quality} className="flex shrink-0 items-center gap-1">
              <QualityIcon quality={quality} />
              {weak && <span className="text-warn-text">{qualityText(quality)}</span>}
            </span>
          )}
          {!isScreen && role !== "guest" && <Shield role="img" aria-label={role === "host" ? "Host" : "Moderator"} data-testid="tile-role" className="size-3 shrink-0" />}
          <span data-testid="tile-name" className="truncate">
            {isScreen ? `${name}'s screen` : name}
            {p.isLocal && " (you)"}
          </span>
        </span>
      </div>
    </>
  );
}

function Tile({ trackRef, large = false, featured = false }: { trackRef: TrackReferenceOrPlaceholder; large?: boolean; featured?: boolean }) {
  return (
    <ParticipantTile
      trackRef={trackRef}
      data-testid="tile"
      data-featured={featured ? "true" : undefined}
      className={`relative min-h-0 min-w-0 overflow-hidden rounded-lg bg-surface ${large ? "h-full w-full" : "aspect-video"}`}
    >
      <TileBody />
    </ParticipantTile>
  );
}

/**
 * Who a speaker view shows: whoever spoke last. Not this person: nobody needs their own face made large, and it would
 * flip back every time they said something. The server decides who is speaking, from the audio levels it receives.
 */
function useFeatured(cameras: TrackReferenceOrPlaceholder[]): TrackReferenceOrPlaceholder | undefined {
  const speakers = useActiveSpeakers();
  const last = useRef<string | null>(null);
  const loudest = speakers.find((s: Participant) => !s.isLocal);
  if (loudest) last.current = loudest.identity;
  return cameras.find((t) => t.participant.identity === last.current) ?? cameras.find((t) => !t.participant.isLocal) ?? cameras[0];
}

function Strip({ tracks }: { tracks: TrackReferenceOrPlaceholder[] }) {
  return (
    <div data-testid="stage-strip" className="flex shrink-0 gap-2 overflow-auto lg:w-64 lg:flex-col [&>*]:w-40 [&>*]:shrink-0 sm:[&>*]:w-48 lg:[&>*]:w-full">
      {tracks.map((t) => (
        <Tile key={getTrackReferenceId(t)} trackRef={t} />
      ))}
    </div>
  );
}

export type Layout = "grid" | "speaker";

export function Stage({ layout }: { layout: Layout }) {
  const tracks = useTracks(TILE_SOURCES);
  const screens = tracks.filter((t) => t.source === TrackSource.ScreenShare && isTrackReference(t));
  const cameras = tracks.filter((t) => t.source === TrackSource.Camera);
  const featured = useFeatured(cameras);
  if (screens.length > 0) {
    return (
      <div data-testid="stage" data-layout="screen" className="flex min-h-0 flex-1 flex-col gap-2 p-2 lg:flex-row">
        <div className="grid min-h-0 flex-1 auto-rows-fr gap-2">
          {screens.map((t) => (
            <Tile key={getTrackReferenceId(t)} trackRef={t} large />
          ))}
        </div>
        <Strip tracks={cameras} />
      </div>
    );
  }
  if (layout === "speaker" && featured) {
    return (
      <div data-testid="stage" data-layout="speaker" className="flex min-h-0 flex-1 flex-col gap-2 p-2 lg:flex-row">
        <div className="grid min-h-0 flex-1 auto-rows-fr gap-2">
          <Tile key={getTrackReferenceId(featured)} trackRef={featured} large featured />
        </div>
        {cameras.length > 1 && <Strip tracks={cameras.filter((t) => t !== featured)} />}
      </div>
    );
  }
  return (
    <div data-testid="stage" data-layout="grid" className="min-h-0 flex-1 overflow-auto p-2">
      <div className="mx-auto grid min-h-full max-w-[110rem] content-center grid-cols-[repeat(auto-fit,minmax(min(100%,20rem),1fr))] gap-2">
        {cameras.map((t) => (
          <Tile key={getTrackReferenceId(t)} trackRef={t} />
        ))}
      </div>
    </div>
  );
}
