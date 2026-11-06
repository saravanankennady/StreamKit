"use client";

import { ConnectionError, ConnectionState, DisconnectReason, type LocalTrackPublication, type Participant, ParticipantEvent, RoomEvent, type TrackPublication, TrackSource } from "@elnova-streamkit/client";
import { RoomAudioRenderer, RoomContext, useAudioPlayback, useConnectionState, useParticipants, useRoom, useRoomContext } from "@elnova-streamkit/react";
import { Camera, CameraOff, Circle, Hand, LayoutGrid, Link2, Lock, MessageSquare, Mic, MicOff, MonitorUp, PhoneOff, Settings, Shield, Smile, UserRound, Users } from "lucide-react";
import { type ButtonHTMLAttributes, type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { JoinError, type JoinSession, type LinkInfo, meetingTitle, serverOnlyReachableLocally } from "@/lib/join";
import { canModerate, handRaised, meetingApi, type MeetingApi, type MeetingDetails, parseMeetingState, roleOf, useEvents } from "@/lib/meeting";
import { REACTIONS, type Reaction, useChat, useReactions } from "@/lib/messages";
import type { MeetingEnd } from "./end-screen";
import { ChatPanel, HostPanel, type PanelName, PeoplePanel, SettingsPanel } from "./panels";
import type { JoinChoices } from "./pre-join";
import { type Layout, Stage } from "./stage";
import { Button } from "./ui";

// A shared screen is for reading, so text stays sharp and it should move smoothly. The lower layer stays, although one
// layer is about 25 ms quicker: on a link too thin for the top layer, one layer means no picture at all.
// Measured with tests/interoperability/sdk-harness/screen-latency.mjs.
const SCREEN_CAPTURE = { contentHint: "detail" } as const;
// Asking for audio offers it in the browser's picker. Where there is none to give, such as a whole screen on some
// systems, the browser leaves it out. The voice processing a microphone needs would damage whatever is being shared.
const SCREEN_CAPTURE_WITH_AUDIO = { ...SCREEN_CAPTURE, audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false } };
const PUBLISH_DEFAULTS = { screenShareEncoding: { maxBitrate: 5_000_000, maxFramerate: 30 } };
// How often someone running a meeting checks who is waiting and what is recording.
const DETAILS_INTERVAL_MS = 3000;
const NOTICE_MS = 5000;
const MUTE_SETTLE_MS = 150;

const ROOM_EVENTS = [RoomEvent.RoomMetadataChanged, RoomEvent.RecordingStatusChanged, RoomEvent.ConnectionStateChanged, RoomEvent.LocalTrackPublished, RoomEvent.LocalTrackUnpublished];
const SELF_EVENTS = [ParticipantEvent.AttributesChanged];

/** A short message that goes away by itself. */
function useNotice(): [string | null, (text: string | null) => void] {
  const [text, setText] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const show = (next: string | null) => {
    clearTimeout(timer.current);
    setText(next);
    if (next) timer.current = setTimeout(() => setText(null), NOTICE_MS);
  };
  useEffect(() => () => clearTimeout(timer.current), []);
  return [text, show];
}

function Banners({ notice, onDismiss, mutedByHost, onDismissMuted }: { notice: string | null; onDismiss: () => void; mutedByHost: boolean; onDismissMuted: () => void }) {
  const room = useRoomContext();
  const state = useConnectionState();
  const { canPlayAudio, startAudio } = useAudioPlayback();
  const reconnecting = state === ConnectionState.Reconnecting || state === ConnectionState.SignalReconnecting;
  const [back, setBack] = useNotice();
  const [recordingNews, setRecordingNews] = useNotice();

  useEffect(() => {
    const onBack = () => setBack("Connection restored.");
    const onRecording = (recording: boolean) => setRecordingNews(recording ? "Recording started. Everyone in the meeting is being recorded." : "Recording stopped.");
    room.on(RoomEvent.Reconnected, onBack);
    room.on(RoomEvent.RecordingStatusChanged, onRecording);
    return () => {
      room.off(RoomEvent.Reconnected, onBack);
      room.off(RoomEvent.RecordingStatusChanged, onRecording);
    };
    // The setters only touch this component's state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room]);

  return (
    <div className="flex flex-col gap-1 px-2 empty:hidden">
      {state === ConnectionState.Connecting && (
        <p role="status" data-testid="connecting" className="rounded bg-surface px-3 py-2 text-sm text-ink-2">
          Joining&hellip;
        </p>
      )}
      {reconnecting && (
        <p role="status" data-testid="reconnecting" className="rounded bg-surface px-3 py-2 text-sm text-warn-text">
          Connection lost. Trying to reconnect&hellip;
        </p>
      )}
      {back && !reconnecting && (
        <p role="status" data-testid="reconnected" className="rounded bg-surface px-3 py-2 text-sm">
          {back}
        </p>
      )}
      {recordingNews && (
        <p role="status" data-testid="recording-news" className="rounded bg-surface px-3 py-2 text-sm">
          {recordingNews}
        </p>
      )}
      {mutedByHost && (
        <div role="status" className="flex flex-wrap items-center justify-between gap-2 rounded bg-surface px-3 py-2 text-sm">
          <span data-testid="muted-by-host">Someone running the meeting muted your microphone. You can unmute when you are ready.</span>
          <Button onClick={onDismissMuted}>Dismiss</Button>
        </div>
      )}
      {!canPlayAudio && (
        <div className="flex flex-wrap items-center gap-2 rounded bg-surface px-3 py-2 text-sm">
          <span>Your browser is holding back the sound.</span>
          <Button data-testid="enable-audio" onClick={() => void startAudio()}>
            Turn sound on
          </Button>
        </div>
      )}
      {notice && (
        <div role="alert" className="flex flex-wrap items-center justify-between gap-2 rounded bg-surface px-3 py-2 text-sm text-critical-text">
          <span data-testid="room-notice">{notice}</span>
          <Button onClick={onDismiss}>Dismiss</Button>
        </div>
      )}
    </div>
  );
}

function Header({ title, inviteUrl, locked, recording }: { title: string; inviteUrl: string | null; locked: boolean; recording: boolean }) {
  const people = useParticipants();
  const [copied, setCopied] = useState(false);
  const canCopy = typeof navigator.clipboard?.writeText === "function";
  return (
    <header className="flex min-h-12 flex-wrap items-center justify-between gap-2 px-3 py-1.5">
      <h1 className="min-w-0 truncate text-sm font-medium" data-testid="room-title">
        {title}
      </h1>
      <div className="flex flex-wrap items-center gap-3 text-sm text-ink-2">
        {recording && (
          <span role="status" data-testid="recording-indicator" className="flex items-center gap-1.5 rounded bg-critical px-2 py-0.5 text-xs font-medium text-white">
            <Circle aria-hidden className="size-2.5 fill-current" /> Recording
          </span>
        )}
        {locked && (
          <span data-testid="locked-indicator" className="flex items-center gap-1.5 text-xs">
            <Lock aria-hidden className="size-3.5" /> Locked
          </span>
        )}
        <span className="flex items-center gap-1" aria-label={`${people.length} in the meeting`}>
          <Users aria-hidden className="size-4" /> <span data-testid="people-count">{people.length}</span>
        </span>
        {canCopy && inviteUrl && (
          <Button
            data-testid="copy-link"
            onClick={() => {
              void navigator.clipboard.writeText(inviteUrl).then(() => {
                setCopied(true);
                setTimeout(() => setCopied(false), 1500);
              });
            }}
          >
            <Link2 aria-hidden /> {copied ? "Copied" : "Copy link"}
          </Button>
        )}
      </div>
    </header>
  );
}

function deviceNotice(err: Error): string {
  if (err.name === "NotAllowedError") return "Your browser blocked the camera or microphone. Allow it in the address bar, then try again.";
  if (err.name === "NotFoundError") return "No camera or microphone was found on this device.";
  if (err.name === "NotReadableError") return "Another app may be using your camera or microphone.";
  return `That did not work: ${err.message}`;
}

function failureMessage(err: Error, session: JoinSession): string {
  if (serverOnlyReachableLocally(session.url)) {
    return "This server is set up with a localhost address, which only works on the computer running it. Tell whoever sent you the link: they need to set public_ws_url to an address other people can reach.";
  }
  if (err instanceof ConnectionError && err.status === 401) {
    // These are the words the server's admission check uses. See api/routes/meetings.go.
    if (err.message.includes("locked")) return "This meeting is locked. Ask the host to unlock it, then try again.";
    if (err.message.includes("removed")) return "You were removed from this meeting.";
    if (err.message.includes("waiting room")) return "This meeting now has a waiting room. Try again, and a host can let you in.";
    if (err.message.includes("ended")) return "This meeting has ended.";
    if (err.message.includes("did not let you in")) return "The host did not let you in.";
    if (err.message.includes("turned off")) return "The link you came in with has been turned off. Ask for a new one.";
    if (err.message.includes("no longer valid")) return "This invitation is no longer valid. Open the link again.";
    return "The meeting refused the connection. It may be full. Try again in a moment.";
  }
  return `Could not connect to the meeting: ${err.message}`;
}

function ControlButton({ label, active, badge, children, className = "", ...props }: { label: string; active?: boolean; badge?: number; variant?: "neutral" | "danger"; children: ReactNode } & Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children">) {
  return (
    <Button {...props} className={`relative max-sm:px-3 ${active ? "ring-2 ring-signal ring-inset" : ""} ${className}`}>
      {children} <span className="max-sm:sr-only">{label}</span>
      {badge ? (
        <span data-testid="control-badge" className="absolute -right-1 -top-1 flex min-w-4 items-center justify-center rounded-full bg-signal px-1 text-[0.65rem] font-semibold text-signal-ink">
          {badge}
        </span>
      ) : null}
    </Button>
  );
}

function ReactionPicker({ onPick, onClose }: { onPick: (emoji: string) => void; onClose: () => void }) {
  return (
    <div
      data-testid="reaction-picker"
      role="group"
      aria-label="Send a reaction"
      onKeyDown={(e) => {
        if (e.key === "Escape") onClose();
      }}
      className="flex flex-wrap justify-center gap-1 px-3 pt-2"
    >
      {REACTIONS.map((r) => (
        <Button key={r.emoji} aria-label={r.label} title={r.label} data-testid="reaction-send" data-emoji={r.emoji} className="px-2.5 text-lg" onClick={() => onPick(r.emoji)}>
          <span aria-hidden>{r.emoji}</span>
        </Button>
      ))}
    </div>
  );
}

function ReactionOverlay({ reactions }: { reactions: Reaction[] }) {
  return (
    <div role="status" aria-label="Reactions" className="pointer-events-none absolute bottom-3 left-3 z-10 flex max-w-[70%] flex-col gap-1">
      {reactions.map((r) => (
        <p key={r.id} data-testid="reaction" data-emoji={r.emoji} data-from={r.from} className="reaction-float flex w-fit items-center gap-2 rounded-full bg-black/70 px-3 py-1 text-sm">
          <span className="text-xl">{r.emoji}</span> <span className="truncate">{r.from}</span>
        </p>
      ))}
    </div>
  );
}

function Inner({ link, api, mic, camera, screen, screenAudioAsked, setMic, setCamera, setScreen, onLeave, notice, onDismiss, mutedByHost, onDismissMuted, onNotice }: {
  link: LinkInfo;
  api: MeetingApi;
  mic: boolean;
  camera: boolean;
  screen: boolean;
  screenAudioAsked: boolean;
  setMic: (on: boolean) => void;
  setCamera: (on: boolean) => void;
  setScreen: (on: boolean) => void;
  onLeave: () => void;
  notice: string | null;
  onDismiss: () => void;
  mutedByHost: boolean;
  onDismissMuted: () => void;
  onNotice: (text: string) => void;
}) {
  const room = useRoomContext();
  const state = useConnectionState();
  useEvents(room, ROOM_EVENTS);
  useEvents(room.localParticipant, SELF_EVENTS);

  const [panel, setPanel] = useState<PanelName | null>(null);
  const [layout, setLayout] = useState<Layout>("grid");
  const [picking, setPicking] = useState(false);
  const [details, setDetails] = useState<MeetingDetails | null>(null);
  const chat = useChat(room, panel === "chat");
  const { reactions, send: sendReaction } = useReactions(room);

  const me = room.localParticipant;
  const role = roleOf(me);
  const moderating = canModerate(role);
  const raised = handRaised(me);
  const meeting = parseMeetingState(room.metadata);
  const title = meeting.title || meetingTitle(link);
  const canShareScreen = link.can_publish && typeof navigator.mediaDevices?.getDisplayMedia === "function";
  const canCapture = link.can_publish && typeof navigator.mediaDevices?.getUserMedia === "function";
  const sharingAudio = Boolean(me.getTrackPublication(TrackSource.ScreenShareAudio));

  // Those running the meeting keep an eye on the door. There is no message for this: the server only tells a room
  // about itself, and the people waiting are not in it yet.
  const refresh = useRef(() => {});
  useEffect(() => {
    if (!moderating || state !== ConnectionState.Connected) {
      refresh.current = () => {};
      return;
    }
    let live = true;
    const load = () => {
      api
        .details()
        .then((d) => {
          if (live) setDetails(d);
        })
        .catch(() => {
          // A missed look is fine: the next one is a few seconds away.
        });
    };
    refresh.current = load;
    load();
    const timer = setInterval(load, DETAILS_INTERVAL_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [api, moderating, state]);

  // The browser's own "stop sharing" button ends a share without passing through these controls.
  useEffect(() => {
    const onUnpublished = (pub: LocalTrackPublication) => {
      if (pub.source === TrackSource.ScreenShare) setScreen(false);
    };
    room.on(RoomEvent.LocalTrackUnpublished, onUnpublished);
    return () => {
      room.off(RoomEvent.LocalTrackUnpublished, onUnpublished);
    };
  }, [room, setScreen]);

  const toggle = (name: PanelName) => setPanel((open) => (open === name ? null : name));
  const waitingCount = moderating ? (details?.waiting_count ?? 0) : 0;
  // A host's own link must never be what they hand out. A guest's is the link they came in with.
  const inviteUrl = moderating ? (details?.invite_url ?? null) : link.role === "guest" ? location.href : null;

  return (
    <main className="flex h-dvh min-h-0 flex-col" data-testid="meeting-room" data-state={state} data-role={role}>
      <Header title={title} inviteUrl={inviteUrl} locked={meeting.locked} recording={room.isRecording} />
      <Banners notice={notice} onDismiss={onDismiss} mutedByHost={mutedByHost} onDismissMuted={onDismissMuted} />
      <div className="flex min-h-0 flex-1">
        <div className="relative flex min-h-0 min-w-0 flex-1 flex-col">
          <Stage layout={layout} />
          <ReactionOverlay reactions={reactions} />
        </div>
        {panel === "chat" && <ChatPanel messages={chat.messages} onSend={chat.send} onClose={() => setPanel(null)} />}
        {panel === "people" && <PeoplePanel api={api} details={details} onChanged={() => refresh.current()} onClose={() => setPanel(null)} />}
        {panel === "host" && moderating && <HostPanel api={api} state={meeting} title={title} details={details} recording={room.isRecording} onChanged={() => refresh.current()} onClose={() => setPanel(null)} />}
        {panel === "settings" && <SettingsPanel link={link} title={title} canCapture={canCapture} onClose={() => setPanel(null)} />}
      </div>
      <RoomAudioRenderer />
      {picking && (
        <ReactionPicker
          onClose={() => setPicking(false)}
          onPick={(emoji) => {
            sendReaction(emoji).catch(() => onNotice("Could not send that reaction."));
          }}
        />
      )}
      {screen && screenAudioAsked && (
        <p role="status" data-testid="screen-audio-state" data-sharing={sharingAudio} className="px-3 pt-1 text-center text-xs text-ink-3">
          {sharingAudio ? "Your screen is being shared with its sound." : "Your screen is being shared without sound."}
        </p>
      )}
      <div className="flex flex-wrap items-center justify-center gap-1.5 px-2 py-2 sm:gap-2 sm:px-3 sm:py-3">
        {canCapture && (
          <>
            <ControlButton label={mic ? "Mute" : "Unmute"} variant={mic ? "neutral" : "danger"} aria-pressed={mic} data-testid="control-mic" onClick={() => setMic(!mic)}>
              {mic ? <Mic aria-hidden /> : <MicOff aria-hidden />}
            </ControlButton>
            <ControlButton label={camera ? "Stop camera" : "Start camera"} variant={camera ? "neutral" : "danger"} aria-pressed={camera} data-testid="control-camera" onClick={() => setCamera(!camera)}>
              {camera ? <Camera aria-hidden /> : <CameraOff aria-hidden />}
            </ControlButton>
          </>
        )}
        {canShareScreen && (
          <ControlButton label={screen ? "Stop sharing" : "Share screen"} active={screen} aria-pressed={screen} data-testid="control-screen" onClick={() => setScreen(!screen)}>
            <MonitorUp aria-hidden />
          </ControlButton>
        )}
        <ControlButton
          label={raised ? "Lower hand" : "Raise hand"}
          active={raised}
          aria-pressed={raised}
          data-testid="control-hand"
          onClick={() => {
            api.setHand(!raised).catch((err: unknown) => onNotice(err instanceof JoinError ? err.message : "Could not change your hand."));
          }}
        >
          <Hand aria-hidden />
        </ControlButton>
        <ControlButton label="React" active={picking} aria-expanded={picking} data-testid="control-react" onClick={() => setPicking((p) => !p)}>
          <Smile aria-hidden />
        </ControlButton>
        <ControlButton label="Chat" active={panel === "chat"} aria-expanded={panel === "chat"} badge={chat.unread} data-testid="control-chat" onClick={() => toggle("chat")}>
          <MessageSquare aria-hidden />
        </ControlButton>
        <ControlButton label="People" active={panel === "people"} aria-expanded={panel === "people"} badge={waitingCount} data-testid="control-people" onClick={() => toggle("people")}>
          <Users aria-hidden />
        </ControlButton>
        <ControlButton label={layout === "grid" ? "Speaker view" : "Grid view"} data-testid="control-layout" onClick={() => setLayout((l) => (l === "grid" ? "speaker" : "grid"))}>
          {layout === "grid" ? <UserRound aria-hidden /> : <LayoutGrid aria-hidden />}
        </ControlButton>
        <ControlButton label="Settings" active={panel === "settings"} aria-expanded={panel === "settings"} data-testid="control-settings" onClick={() => toggle("settings")}>
          <Settings aria-hidden />
        </ControlButton>
        {moderating && (
          <ControlButton label="Host controls" active={panel === "host"} aria-expanded={panel === "host"} data-testid="control-host" onClick={() => toggle("host")}>
            <Shield aria-hidden />
          </ControlButton>
        )}
        <ControlButton label="Leave" variant="danger" data-testid="control-leave" onClick={onLeave}>
          <PhoneOff aria-hidden />
        </ControlButton>
      </div>
    </main>
  );
}

export function MeetingRoom({ link, session, choices, onEnded }: { link: LinkInfo; session: JoinSession; choices: JoinChoices; onEnded: (end: MeetingEnd) => void }) {
  const [mic, setMic] = useState(choices.mic);
  const [camera, setCamera] = useState(choices.camera);
  const [screen, setScreen] = useState(false);
  const [screenAudio, setScreenAudio] = useState(true);
  const [micId, setMicId] = useState(choices.micId);
  const [cameraId, setCameraId] = useState(choices.cameraId);
  const [notice, setNotice] = useState<string | null>(null);
  const [mutedByHost, setMutedByHost] = useState(false);

  const startedAt = useRef<number | null>(null);
  const others = useRef(new Set<string>());
  const names = useRef(new Set<string>());
  const recorded = useRef(false);
  const ended = useRef(false);
  const lastToggle = useRef<"screen" | "other">("other");
  const micWanted = useRef(mic);
  micWanted.current = mic;

  const finish = (end: Pick<MeetingEnd, "reason" | "failure">) => {
    if (ended.current) return;
    ended.current = true;
    const seconds = startedAt.current === null ? 0 : Math.round((Date.now() - startedAt.current) / 1000);
    onEnded({ ...end, seconds, others: others.current.size, names: [...names.current], recorded: recorded.current, token: room.accessToken, room: session.room, role: session.role });
  };

  const { room } = useRoom({
    serverUrl: session.url,
    token: session.token,
    audio: link.can_publish && mic ? (micId ? { deviceId: { exact: micId } } : true) : false,
    video: link.can_publish && camera ? (cameraId ? { deviceId: { exact: cameraId } } : true) : false,
    screen: link.can_publish && screen ? (screenAudio ? SCREEN_CAPTURE_WITH_AUDIO : SCREEN_CAPTURE) : false,
    options: { adaptiveStream: true, dynacast: true, publishDefaults: PUBLISH_DEFAULTS, audioOutput: choices.speakerId ? { deviceId: choices.speakerId } : undefined },
    onConnected: () => {
      startedAt.current ??= Date.now();
      if (room.isRecording) recorded.current = true;
    },
    onDisconnected: (reason?: DisconnectReason) => {
      if (startedAt.current !== null) {
        finish({ reason });
        return;
      }
      // A join that fails also disconnects, and the error saying why follows at once, so it gets a moment to arrive.
      setTimeout(() => finish({ reason }), 500);
    },
    onError: (err) => {
      if (startedAt.current === null && room.state !== ConnectionState.Connected) {
        finish({ failure: failureMessage(err, session) });
        return;
      }
      // Some browsers refuse a request for a screen's audio outright. Sharing without it beats not sharing.
      if (lastToggle.current === "screen" && err.name === "TypeError" && screenAudio) {
        setScreenAudio(false);
        setScreen(false);
        setTimeout(() => setScreen(true));
        return;
      }
      // What failed is back to what it really is, so the buttons do not claim otherwise.
      const lp = room.localParticipant;
      setMic(lp.isMicrophoneEnabled);
      setCamera(lp.isCameraEnabled);
      setScreen(lp.isScreenShareEnabled);
      // Closing the screen picker without choosing is not a problem to report.
      if (lastToggle.current === "screen" && err.name === "NotAllowedError") return;
      setNotice(deviceNotice(err));
    },
  });

  const api = useMemo(() => meetingApi(room, session.room), [room, session.room]);

  useEffect(() => {
    const onJoin = (p: Participant) => {
      others.current.add(p.identity);
      names.current.add(p.name || p.identity);
    };
    const onConnected = () => {
      for (const p of room.remoteParticipants.values()) onJoin(p);
    };
    const onRecording = (recording: boolean) => {
      if (recording) recorded.current = true;
    };
    // A host's mute reaches the track directly. The buttons follow, and without this, the next change to any device
    // would turn the microphone back on, because they all apply together. This looks a moment later, because the same
    // event comes from someone's own quick presses of the button, and what counts is where those end up.
    let settle: ReturnType<typeof setTimeout> | undefined;
    const onMute = (pub: TrackPublication, p: Participant) => {
      if (!p.isLocal || pub.source !== TrackSource.Microphone) return;
      clearTimeout(settle);
      settle = setTimeout(() => {
        if (pub.isMuted && micWanted.current) setMutedByHost(true);
        setMic(!pub.isMuted);
      }, MUTE_SETTLE_MS);
    };
    // Whatever gets published later, such as a microphone that was off until now, uses the device chosen last.
    const onDevice = (kind: MediaDeviceKind, id: string) => {
      if (kind === "audioinput") setMicId(id);
      if (kind === "videoinput") setCameraId(id);
    };
    onConnected();
    room.on(RoomEvent.ParticipantConnected, onJoin);
    room.on(RoomEvent.Connected, onConnected);
    room.on(RoomEvent.RecordingStatusChanged, onRecording);
    room.on(RoomEvent.TrackMuted, onMute);
    room.on(RoomEvent.TrackUnmuted, onMute);
    room.on(RoomEvent.ActiveDeviceChanged, onDevice);
    return () => {
      clearTimeout(settle);
      room.off(RoomEvent.ParticipantConnected, onJoin);
      room.off(RoomEvent.Connected, onConnected);
      room.off(RoomEvent.RecordingStatusChanged, onRecording);
      room.off(RoomEvent.TrackMuted, onMute);
      room.off(RoomEvent.TrackUnmuted, onMute);
      room.off(RoomEvent.ActiveDeviceChanged, onDevice);
    };
  }, [room]);

  return (
    <RoomContext.Provider value={room}>
      <Inner
        link={link}
        api={api}
        mic={mic}
        camera={camera}
        screen={screen}
        screenAudioAsked={screenAudio}
        setMic={(on) => {
          lastToggle.current = "other";
          setNotice(null);
          setMutedByHost(false);
          setMic(on);
        }}
        setCamera={(on) => {
          lastToggle.current = "other";
          setNotice(null);
          setCamera(on);
        }}
        setScreen={(on) => {
          lastToggle.current = "screen";
          setNotice(null);
          setScreen(on);
        }}
        notice={notice}
        onDismiss={() => setNotice(null)}
        onNotice={setNotice}
        mutedByHost={mutedByHost}
        onDismissMuted={() => setMutedByHost(false)}
        onLeave={() => {
          // Leaving before the connection is up cancels the attempt, which reports nothing.
          const started = startedAt.current !== null;
          void room.disconnect();
          if (!started) finish({ reason: DisconnectReason.CLIENT_INITIATED });
        }}
      />
    </RoomContext.Provider>
  );
}
