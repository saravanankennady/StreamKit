"use client";

import { type Participant, RoomEvent, type Room, TrackSource } from "@elnova-streamkit/client";
import { useConnectionQuality, useMediaDeviceSelect, useParticipantTrack, useParticipants, useRoomContext } from "@elnova-streamkit/react";
import { Check, Circle, Hand, Lock, LockOpen, Mic, MicOff, PhoneOff, Send, Shield, Square, Users, Volume2, X } from "lucide-react";
import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { JoinError, type LinkInfo } from "@/lib/join";
import { type MeetingApi, type MeetingDetails, type MeetingState, canModerate, handRaised, initials, roleOf } from "@/lib/meeting";
import { type ChatMessage, MAX_CHAT_LENGTH } from "@/lib/messages";
import { playTestTone, speakerChoiceSupported } from "./pre-join";
import { qualityText, QualityIcon } from "./stage";
import { Button } from "./ui";

export type PanelName = "chat" | "people" | "host" | "settings";

const message = (err: unknown) => (err instanceof JoinError ? err.message : "That did not work. Try again.");

/** Runs a call to the server, says if it failed, and keeps one thing from being pressed twice. */
function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const run = async (action: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await action();
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, run };
}

function ActionError({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <p role="alert" data-testid="action-error" className="text-sm text-critical-text">
      {error}
    </p>
  );
}

/** A column beside the meeting on a large screen, and the whole of a small one. */
export function Panel({ title, testId, onClose, children }: { title: string; testId: string; onClose: () => void; children: ReactNode }) {
  const close = useRef<HTMLButtonElement>(null);
  // Focus comes in with the panel, and goes back to what opened it.
  useEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    close.current?.focus({ preventScroll: true });
    return () => opener?.focus({ preventScroll: true });
  }, []);
  return (
    <aside
      data-testid={testId}
      aria-label={title}
      onKeyDown={(e) => {
        if (e.key === "Escape") onClose();
      }}
      className="fixed inset-0 z-20 flex flex-col bg-page lg:static lg:z-auto lg:w-80 lg:shrink-0 lg:border-l lg:border-line"
    >
      <div className="flex items-center justify-between gap-2 border-b border-line px-3 py-2">
        <h2 className="text-sm font-medium">{title}</h2>
        <Button ref={close} aria-label={`Close ${title.toLowerCase()}`} data-testid="panel-close" onClick={onClose} className="px-2.5">
          <X aria-hidden />
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-3">{children}</div>
    </aside>
  );
}

export function ChatPanel({ messages, onSend, onClose }: { messages: ChatMessage[]; onSend: (text: string) => Promise<void>; onClose: () => void }) {
  const [text, setText] = useState("");
  const { busy, error, run } = useAction();
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => {
    end.current?.scrollIntoView({ block: "end" });
  }, [messages.length]);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (!trimmed) return;
    void run(async () => {
      await onSend(trimmed);
      setText("");
    });
  };
  return (
    <Panel title="Chat" testId="panel-chat" onClose={onClose}>
      <div className="flex h-full flex-col gap-3">
        <div role="log" aria-label="Messages" className="min-h-0 flex-1 space-y-3 overflow-y-auto">
          {messages.length === 0 && <p className="text-sm text-ink-3">Messages go to everyone in the meeting now. Nobody who joins later sees them, and they are not kept.</p>}
          {messages.map((m) => (
            <div key={m.id} data-testid="chat-message" data-mine={m.mine} className="text-sm">
              <p className="text-xs text-ink-3" data-testid="chat-from">
                {m.from}
              </p>
              <p data-testid="chat-text" className="whitespace-pre-wrap break-words">
                {m.text}
              </p>
            </div>
          ))}
          <div ref={end} />
        </div>
        <ActionError error={error} />
        <form onSubmit={submit} className="flex gap-2">
          <label htmlFor="chat-input" className="sr-only">
            Message
          </label>
          <input
            id="chat-input"
            data-testid="chat-input"
            value={text}
            maxLength={MAX_CHAT_LENGTH}
            autoComplete="off"
            onChange={(e) => setText(e.target.value)}
            placeholder="Send a message"
            className="h-10 min-w-0 flex-1 rounded-md border border-line-2 bg-surface px-3 text-sm"
          />
          <Button type="submit" variant="primary" disabled={busy || !text.trim()} aria-label="Send message" data-testid="chat-send" className="px-3">
            <Send aria-hidden />
          </Button>
        </form>
      </div>
    </Panel>
  );
}

function roleName(role: string): string | null {
  if (role === "host") return "Host";
  if (role === "moderator") return "Moderator";
  return null;
}

function PersonRow({ p, me, api }: { p: Participant; me: Participant; api: MeetingApi }) {
  const mic = useParticipantTrack(p, TrackSource.Microphone);
  const quality = useConnectionQuality(p);
  const [confirming, setConfirming] = useState(false);
  const { busy, error, run } = useAction();
  const myRole = roleOf(me);
  const role = roleOf(p);
  const self = p === me;
  const name = p.name || p.identity;
  const micOn = Boolean(mic.publication) && !mic.isMuted;
  const canRemove = !self && canModerate(myRole) && (myRole === "host" || role === "guest");
  const raised = handRaised(p);
  return (
    <li data-testid="person" data-name={name} data-identity={p.identity} data-mic={micOn ? "on" : "off"} data-hand={raised} className="rounded-md bg-surface p-2">
      <div className="flex items-center gap-2">
        <span aria-hidden className="flex size-8 shrink-0 items-center justify-center rounded-full bg-raised text-xs font-medium">
          {initials(name)}
        </span>
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm">
            {name}
            {self && " (you)"}
          </p>
          <p className="flex flex-wrap items-center gap-x-2 text-xs text-ink-2">
            {roleName(role) && (
              <span data-testid="person-role-name" className="flex items-center gap-1">
                <Shield aria-hidden className="size-3" /> {roleName(role)}
              </span>
            )}
            <span className="flex items-center gap-1" data-testid="person-mic-state">
              {micOn ? <Mic aria-hidden className="size-3" /> : <MicOff aria-hidden className="size-3" />} {micOn ? "Mic on" : "Muted"}
            </span>
            <span className="flex items-center gap-1" data-testid="person-quality" data-quality={quality}>
              <QualityIcon quality={quality} /> {qualityText(quality)}
            </span>
            {raised && (
              <span data-testid="person-hand" className="flex items-center gap-1 text-signal">
                <Hand aria-hidden className="size-3" /> Hand raised
              </span>
            )}
          </p>
        </div>
      </div>
      {canModerate(myRole) && !self && (
        <div className="mt-2 flex flex-wrap gap-1.5">
          {micOn && (
            <Button data-testid="person-mute" aria-label={`Mute ${name}`} disabled={busy} className="min-h-8 px-2.5 text-xs" onClick={() => void run(() => api.mute(p.identity))}>
              Mute
            </Button>
          )}
          {raised && (
            <Button data-testid="person-lower-hand" aria-label={`Lower ${name}'s hand`} disabled={busy} className="min-h-8 px-2.5 text-xs" onClick={() => void run(() => api.lowerHand(p.identity))}>
              Lower hand
            </Button>
          )}
          {myRole === "host" && role !== "host" && (
            <Button
              data-testid="person-role"
              aria-label={role === "moderator" ? `Take moderator away from ${name}` : `Make ${name} a moderator`}
              disabled={busy}
              className="min-h-8 px-2.5 text-xs"
              onClick={() => void run(() => api.setRole(p.identity, role === "moderator" ? "guest" : "moderator"))}
            >
              {role === "moderator" ? "Remove moderator" : "Make moderator"}
            </Button>
          )}
          {canRemove &&
            (confirming ? (
              <>
                <Button variant="danger" data-testid="person-remove-confirm" aria-label={`Confirm removing ${name}`} disabled={busy} className="min-h-8 px-2.5 text-xs" onClick={() => void run(() => api.remove(p.identity))}>
                  Remove {name}
                </Button>
                <Button className="min-h-8 px-2.5 text-xs" onClick={() => setConfirming(false)}>
                  Keep
                </Button>
              </>
            ) : (
              <Button data-testid="person-remove" aria-label={`Remove ${name}`} className="min-h-8 px-2.5 text-xs" onClick={() => setConfirming(true)}>
                Remove
              </Button>
            ))}
        </div>
      )}
      <ActionError error={error} />
    </li>
  );
}

function WaitingRow({ identity, name, api, onAnswered }: { identity: string; name: string; api: MeetingApi; onAnswered: () => void }) {
  const { busy, error, run } = useAction();
  const answer = (admit: boolean) =>
    void run(async () => {
      await api.answer(identity, admit);
      onAnswered();
    });
  return (
    <li data-testid="waiting-row" data-name={name} className="rounded-md bg-surface p-2">
      <p className="truncate text-sm">{name}</p>
      <div className="mt-2 flex gap-1.5">
        <Button variant="primary" data-testid="waiting-admit" aria-label={`Let ${name} in`} disabled={busy} className="min-h-8 px-2.5 text-xs" onClick={() => answer(true)}>
          <Check aria-hidden /> Let in
        </Button>
        <Button data-testid="waiting-deny" aria-label={`Turn ${name} away`} disabled={busy} className="min-h-8 px-2.5 text-xs" onClick={() => answer(false)}>
          Turn away
        </Button>
      </div>
      <ActionError error={error} />
    </li>
  );
}

/** For a waiting room full of people nobody knows. They can knock again, so this says what else would help. */
function TurnAwayEveryone({ api, onAnswered }: { api: MeetingApi; onAnswered: () => void }) {
  const { busy, error, run } = useAction();
  return (
    <div className="mt-2">
      <Button
        data-testid="waiting-deny-all"
        disabled={busy}
        className="min-h-8 px-2.5 text-xs"
        onClick={() =>
          void run(async () => {
            await api.turnAwayEveryone();
            onAnswered();
          })
        }
      >
        Turn everyone away
      </Button>
      <p className="mt-1 text-xs text-ink-3">They can ask again. Lock the meeting to keep new people out.</p>
      <ActionError error={error} />
    </div>
  );
}

export function PeoplePanel({ api, details, onChanged, onClose }: { api: MeetingApi; details: MeetingDetails | null; onChanged: () => void; onClose: () => void }) {
  const room = useRoomContext();
  // Everyone's state is read in here, so it re-renders on anything about anyone.
  const people = useParticipants();
  const waiting = details?.waiting ?? [];
  const waitingCount = details?.waiting_count ?? waiting.length;
  const sorted = [...people].sort((a, b) => Number(handRaised(b)) - Number(handRaised(a)));
  return (
    <Panel title="People" testId="panel-people" onClose={onClose}>
      {waiting.length > 0 && (
        <section aria-label="Waiting to come in" className="mb-4">
          <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-ink-3">
            Waiting to come in (<span data-testid="waiting-count">{waitingCount}</span>)
          </h3>
          <ul className="space-y-2">
            {waiting.map((w) => (
              <WaitingRow key={w.identity} identity={w.identity} name={w.name} api={api} onAnswered={onChanged} />
            ))}
          </ul>
          {waitingCount > waiting.length && (
            <p data-testid="waiting-more" className="mt-2 text-xs text-ink-3">
              And {waitingCount - waiting.length} more after these.
            </p>
          )}
          {waitingCount > 1 && <TurnAwayEveryone api={api} onAnswered={onChanged} />}
        </section>
      )}
      <h3 className="mb-2 flex items-center gap-1.5 text-xs font-medium uppercase tracking-wide text-ink-3">
        <Users aria-hidden className="size-3" /> In the meeting ({people.length})
      </h3>
      <ul className="space-y-2">
        {sorted.map((p) => (
          <PersonRow key={p.identity} p={p} me={room.localParticipant} api={api} />
        ))}
      </ul>
    </Panel>
  );
}

function Switch({ on, label, testId, disabled, onChange, icon }: { on: boolean; label: string; testId: string; disabled?: boolean; onChange: (on: boolean) => void; icon?: ReactNode }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      data-testid={testId}
      disabled={disabled}
      onClick={() => onChange(!on)}
      className="flex min-h-10 w-full items-center justify-between gap-3 rounded-md bg-surface px-3 text-left text-sm disabled:opacity-50"
    >
      <span className="flex items-center gap-2 [&_svg]:size-4">
        {icon} {label}
      </span>
      <span className={`rounded px-2 py-0.5 text-xs font-medium ${on ? "bg-signal text-signal-ink" : "bg-raised text-ink-2"}`}>{on ? "On" : "Off"}</span>
    </button>
  );
}

export function HostPanel({ api, state, title, details, recording, onChanged, onClose }: {
  api: MeetingApi;
  state: MeetingState;
  title: string;
  details: MeetingDetails | null;
  recording: boolean;
  onChanged: () => void;
  onClose: () => void;
}) {
  const room = useRoomContext();
  const role = roleOf(room.localParticipant);
  const isHost = role === "host";
  const { busy, error, run } = useAction();
  const [muted, setMuted] = useState<number | null>(null);
  const [draft, setDraft] = useState(title);
  const [ending, setEnding] = useState(false);
  const job = details?.recording;

  useEffect(() => setDraft(title), [title]);

  return (
    <Panel title="Host controls" testId="panel-host" onClose={onClose}>
      <div className="space-y-4">
        <section className="space-y-2" aria-label="Everyone">
          <Button
            className="w-full"
            data-testid="host-mute-all"
            disabled={busy}
            onClick={() =>
              void run(async () => {
                const { muted: count } = await api.muteAll();
                setMuted(count);
              })
            }
          >
            <MicOff aria-hidden /> Mute everyone
          </Button>
          {muted !== null && (
            <p role="status" data-testid="host-muted-count" className="text-xs text-ink-2">
              {muted === 0 ? "Nobody had a microphone on." : `Muted ${muted} ${muted === 1 ? "person" : "people"}. They can unmute themselves.`}
            </p>
          )}
        </section>

        {isHost && (
          <>
            <section className="space-y-2" aria-label="Recording">
              <h3 className="text-xs font-medium uppercase tracking-wide text-ink-3">Recording</h3>
              {recording && job?.status !== "ending" ? (
                <Button variant="danger" className="w-full" data-testid="host-record" disabled={busy || !job} onClick={() => job && void run(async () => {
                  await api.stopRecording(job.id);
                  onChanged();
                })}>
                  <Square aria-hidden /> Stop recording
                </Button>
              ) : (
                <Button
                  className="w-full"
                  data-testid="host-record"
                  disabled={busy || Boolean(job)}
                  onClick={() =>
                    void run(async () => {
                      await api.startRecording();
                      onChanged();
                    })
                  }
                >
                  <Circle aria-hidden /> {job?.status === "ending" ? "Finishing the recording…" : job ? "Starting the recording…" : "Start recording"}
                </Button>
              )}
              <p className="text-xs text-ink-3">Everyone in the meeting sees when this is on. The file goes to your project's Recordings page in the dashboard.</p>
            </section>

            <section className="space-y-2" aria-label="Who can come in">
              <h3 className="text-xs font-medium uppercase tracking-wide text-ink-3">Who can come in</h3>
              <Switch
                on={state.locked}
                label="Lock the meeting"
                testId="host-lock"
                disabled={busy}
                icon={state.locked ? <Lock aria-hidden /> : <LockOpen aria-hidden />}
                onChange={(locked) => void run(() => api.update({ locked }))}
              />
              <p className="text-xs text-ink-3">Nobody new can join, even with a link. People here can reconnect, and hosts can always come in.</p>
              <Switch
                on={state.waiting_room}
                label="Waiting room"
                testId="host-waiting"
                disabled={busy}
                onChange={(waiting_room) => void run(() => api.update({ waiting_room }))}
              />
              <p className="text-xs text-ink-3">People with the link wait until you or a moderator let them in, from the People panel.</p>
            </section>

            <section aria-label="Meeting title">
              <form
                className="space-y-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  void run(() => api.update({ title: draft.trim() }));
                }}
              >
                <label className="flex flex-col gap-1 text-xs font-medium uppercase tracking-wide text-ink-3" htmlFor="host-title">
                  Meeting title
                </label>
                <div className="flex gap-2">
                  <input id="host-title" data-testid="host-title" value={draft} maxLength={100} onChange={(e) => setDraft(e.target.value)} className="h-10 min-w-0 flex-1 rounded-md border border-line-2 bg-surface px-3 text-sm" />
                  <Button type="submit" disabled={busy || draft.trim() === title} data-testid="host-title-save">
                    Save
                  </Button>
                </div>
              </form>
            </section>

            <section className="space-y-2" aria-label="End the meeting">
              {ending ? (
                <div className="space-y-2 rounded-md bg-surface p-3">
                  <p className="text-sm">This disconnects everyone and stops any recording. The guest link stops working until a host starts the meeting again.</p>
                  <div className="flex flex-wrap gap-2">
                    <Button variant="danger" data-testid="host-end-confirm" disabled={busy} onClick={() => void run(() => api.end())}>
                      <PhoneOff aria-hidden /> End for everyone
                    </Button>
                    <Button onClick={() => setEnding(false)}>Keep going</Button>
                  </div>
                </div>
              ) : (
                <Button variant="danger" className="w-full" data-testid="host-end" onClick={() => setEnding(true)}>
                  <PhoneOff aria-hidden /> End meeting for everyone
                </Button>
              )}
            </section>
          </>
        )}
        <ActionError error={error} />
      </div>
    </Panel>
  );
}

function DeviceRow({ room, kind, label, testId, onError }: { room: Room; kind: MediaDeviceKind; label: string; testId: string; onError: (msg: string | null) => void }) {
  const { devices, activeDeviceId, setActiveDevice } = useMediaDeviceSelect({ kind, room, requestPermissions: false });
  const named = devices.filter((d) => d.deviceId);
  if (named.length === 0) return null;
  return (
    <label className="flex min-w-0 flex-col gap-1 text-xs text-ink-2">
      {label}
      <select
        data-testid={testId}
        value={activeDeviceId ?? named[0]?.deviceId ?? ""}
        onChange={(e) => {
          onError(null);
          setActiveDevice(e.target.value)
            .then((ok) => {
              if (!ok) onError(`Could not switch to that ${label.toLowerCase()}.`);
            })
            .catch(() => onError(`Could not switch to that ${label.toLowerCase()}.`));
        }}
        className="h-9 w-full min-w-0 rounded-md border border-line-2 bg-surface px-2 text-sm text-ink"
      >
        {named.map((d, i) => (
          <option key={d.deviceId} value={d.deviceId}>
            {d.label || `${label} ${i + 1}`}
          </option>
        ))}
      </select>
    </label>
  );
}

export function SettingsPanel({ link, title, canCapture, onClose }: { link: LinkInfo; title: string; canCapture: boolean; onClose: () => void }) {
  const room = useRoomContext();
  const people = useParticipants({ updateOnlyOn: [RoomEvent.ParticipantConnected, RoomEvent.ParticipantDisconnected] });
  const [deviceError, setDeviceError] = useState<string | null>(null);
  const [testing, setTesting] = useState(false);
  const [toneResult, setToneResult] = useState<string | null>(null);
  const canPickSpeaker = speakerChoiceSupported();
  const role = roleOf(room.localParticipant);
  return (
    <Panel title="Settings" testId="panel-settings" onClose={onClose}>
      <div className="space-y-4">
        <section className="space-y-3" aria-label="Devices">
          {canCapture && <DeviceRow room={room} kind="audioinput" label="Microphone" testId="settings-mic" onError={setDeviceError} />}
          {canCapture && <DeviceRow room={room} kind="videoinput" label="Camera" testId="settings-camera" onError={setDeviceError} />}
          {canPickSpeaker ? (
            <DeviceRow room={room} kind="audiooutput" label="Speaker" testId="settings-speaker" onError={setDeviceError} />
          ) : (
            <p data-testid="settings-speaker-unsupported" className="text-xs text-ink-3">
              This browser plays sound through its default speaker, and cannot choose another from a page.
            </p>
          )}
          <Button
            data-testid="settings-test-speaker"
            disabled={testing}
            onClick={() => {
              setTesting(true);
              setToneResult(null);
              playTestTone(room.getActiveDevice("audiooutput") ?? "")
                .then((r) => setToneResult(`Played a tone on ${r.sinkLabel}.`))
                .catch(() => setToneResult("Could not play a sound on this speaker."))
                .finally(() => setTesting(false));
            }}
          >
            <Volume2 aria-hidden /> {testing ? "Playing…" : "Test speaker"}
          </Button>
          {toneResult && (
            <p role="status" data-testid="settings-tone-result" className="text-xs text-ink-2">
              {toneResult}
            </p>
          )}
          {deviceError && (
            <p role="alert" data-testid="settings-device-error" className="text-sm text-critical-text">
              {deviceError}
            </p>
          )}
        </section>
        <section aria-label="About this meeting">
          <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-ink-3">About this meeting</h3>
          <dl data-testid="meeting-info" className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
            <dt className="text-ink-3">Title</dt>
            <dd data-testid="info-title" className="min-w-0 break-words">
              {title}
            </dd>
            <dt className="text-ink-3">Room</dt>
            <dd data-testid="info-room" className="min-w-0 break-all">
              {link.room}
            </dd>
            <dt className="text-ink-3">You are</dt>
            <dd data-testid="info-role">{role === "host" ? "A host" : role === "moderator" ? "A moderator" : "A guest"}</dd>
            <dt className="text-ink-3">People here</dt>
            <dd data-testid="info-people">{people.length}</dd>
            <dt className="text-ink-3">Link expires</dt>
            <dd data-testid="info-expires">{new Date(link.expires_at).toLocaleString()}</dd>
          </dl>
        </section>
      </div>
    </Panel>
  );
}
