"use client";

import { createLocalAudioTrack, createLocalVideoTrack } from "@elnova-streamkit/client";
import { Camera, CameraOff, Mic, MicOff, Volume2 } from "lucide-react";
import { type FormEvent, type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { isWaiting, JoinError, joinWithLink, type JoinSession, type LinkInfo, meetingTitle } from "@/lib/join";
import { Button, Centered } from "./ui";

export interface JoinChoices {
  mic: boolean;
  camera: boolean;
  micId?: string;
  cameraId?: string;
  speakerId?: string;
}

const NAME_KEY = "streamkit.meeting.name";

function remembered(): string {
  try {
    return localStorage.getItem(NAME_KEY) ?? "";
  } catch {
    return "";
  }
}

function remember(name: string) {
  try {
    localStorage.setItem(NAME_KEY, name);
  } catch {
    // Private windows can refuse this. The name only saves some typing.
  }
}

function deviceMessage(err: unknown, what: string): string {
  const name = err instanceof Error ? err.name : "";
  if (name === "NotAllowedError") return `Your browser blocked the ${what}. Allow it in the address bar, then turn it on again.`;
  if (name === "NotFoundError") return `No ${what} found on this device.`;
  if (name === "NotReadableError") return `Another app may be using the ${what}.`;
  return `Could not start the ${what}.`;
}

function DeviceSelect({ label, devices, value, onChange, testId }: { label: string; devices: MediaDeviceInfo[]; value: string; onChange: (id: string) => void; testId: string }) {
  if (devices.length === 0) return null;
  return (
    <label className="flex min-w-0 flex-col gap-1 text-xs text-ink-2">
      {label}
      <select data-testid={testId} value={value} onChange={(e) => onChange(e.target.value)} className="h-9 w-full min-w-0 rounded-md border border-line-2 bg-surface px-2 text-sm text-ink">
        {devices.map((d, i) => (
          <option key={d.deviceId || i} value={d.deviceId}>
            {d.label || `${label} ${i + 1}`}
          </option>
        ))}
      </select>
    </label>
  );
}

function Toggle({ on, onChange, disabled, testId, children }: { on: boolean; onChange: (on: boolean) => void; disabled?: boolean; testId: string; children: ReactNode }) {
  return (
    <Button variant={on ? "neutral" : "danger"} aria-pressed={on} disabled={disabled} data-testid={testId} onClick={() => onChange(!on)}>
      {children}
    </Button>
  );
}

/**
 * Whether a page can choose where sound comes out. Chrome and Edge can. Safari and browsers on iOS cannot, and Firefox
 * only for a device the person has picked in its own prompt, so those play through the default speaker.
 */
export function speakerChoiceSupported(): boolean {
  return typeof HTMLMediaElement !== "undefined" && "setSinkId" in HTMLMediaElement.prototype;
}

export interface ToneResult {
  /** The output the browser says it used. Empty means its default. */
  sinkId: string;
  sinkLabel: string;
  /** How loud the tone was on its way to that output, from 0 to 1. */
  peak: number;
}

/** Plays a short tone on the chosen speaker, and reports what was really played where. */
export async function playTestTone(speakerId: string): Promise<ToneResult> {
  const ctx = new AudioContext();
  try {
    await ctx.resume();
    const out = ctx.createMediaStreamDestination();
    const gain = ctx.createGain();
    const osc = ctx.createOscillator();
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 2048;
    osc.frequency.value = 660;
    gain.gain.setValueAtTime(0.0001, ctx.currentTime);
    gain.gain.exponentialRampToValueAtTime(0.25, ctx.currentTime + 0.03);
    gain.gain.exponentialRampToValueAtTime(0.0001, ctx.currentTime + 0.8);
    osc.connect(gain);
    gain.connect(out);
    gain.connect(analyser);
    const el = new Audio() as HTMLAudioElement & { setSinkId?: (id: string) => Promise<void>; sinkId?: string };
    el.srcObject = out.stream;
    if (speakerId && el.setSinkId) await el.setSinkId(speakerId);
    await el.play();
    osc.start();
    const samples = new Float32Array(analyser.fftSize);
    let peak = 0;
    const measuring = setInterval(() => {
      analyser.getFloatTimeDomainData(samples);
      for (const s of samples) peak = Math.max(peak, Math.abs(s));
    }, 40);
    await new Promise<void>((resolve) => {
      osc.onended = () => resolve();
      osc.stop(ctx.currentTime + 0.85);
    });
    clearInterval(measuring);
    const sinkId = el.sinkId ?? "";
    el.srcObject = null;
    // No device list at all on a page that is not secure.
    const devices = (await navigator.mediaDevices?.enumerateDevices().catch(() => [])) ?? [];
    const named = devices.find((d) => d.kind === "audiooutput" && d.deviceId === (sinkId || "default"))?.label;
    return { sinkId, sinkLabel: named || "your default speaker", peak };
  } finally {
    await ctx.close();
  }
}

export function PreJoin({ code, link, onJoined, onWaiting }: {
  code: string;
  link: LinkInfo;
  onJoined: (session: JoinSession, choices: JoinChoices) => void;
  onWaiting: (ticket: string, choices: JoinChoices) => void;
}) {
  // Browsers only offer cameras and microphones on secure pages: https, or localhost.
  const secure = typeof navigator.mediaDevices?.getUserMedia === "function";
  const canCapture = link.can_publish && secure;

  const [name, setName] = useState(remembered);
  const [camera, setCamera] = useState(canCapture);
  const [mic, setMic] = useState(canCapture);
  const [cameraId, setCameraId] = useState("");
  const [micId, setMicId] = useState("");
  const [speakerId, setSpeakerId] = useState("");
  const [devices, setDevices] = useState<MediaDeviceInfo[]>([]);
  const [cameraError, setCameraError] = useState<string | null>(null);
  const [micError, setMicError] = useState<string | null>(null);
  const [joining, setJoining] = useState(false);
  const [joinError, setJoinError] = useState<string | null>(null);
  const [testing, setTesting] = useState(false);
  const [toneError, setToneError] = useState<string | null>(null);
  const [tone, setTone] = useState<ToneResult | null>(null);

  const video = useRef<HTMLVideoElement>(null);
  const meter = useRef<HTMLDivElement>(null);

  const refreshDevices = useCallback(() => {
    // Device names only appear once the page has been given a device, so this runs after each capture starts.
    navigator.mediaDevices
      .enumerateDevices()
      .then(setDevices)
      .catch(() => setDevices([]));
  }, []);

  // A choice of device is exact. Left as a preference, Chrome can pick another, and say nothing.
  useEffect(() => {
    if (!canCapture || !camera) return;
    let live = true;
    let stop = () => {};
    createLocalVideoTrack(cameraId ? { deviceId: { exact: cameraId } } : {})
      .then((track) => {
        if (!live) {
          track.stop();
          return;
        }
        setCameraError(null);
        const el = video.current;
        if (el) track.attach(el);
        stop = () => {
          if (el) track.detach(el);
          track.stop();
        };
        refreshDevices();
      })
      .catch((err) => {
        if (!live) return;
        setCameraError(deviceMessage(err, "camera"));
        setCamera(false);
      });
    return () => {
      live = false;
      stop();
    };
  }, [canCapture, camera, cameraId, refreshDevices]);

  useEffect(() => {
    if (!canCapture || !mic) return;
    let live = true;
    let stop = () => {};
    createLocalAudioTrack(micId ? { deviceId: { exact: micId } } : {})
      .then((track) => {
        if (!live) {
          track.stop();
          return;
        }
        setMicError(null);
        const ctx = new AudioContext();
        void ctx.resume();
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 512;
        ctx.createMediaStreamSource(new MediaStream([track.mediaStreamTrack])).connect(analyser);
        const samples = new Float32Array(analyser.fftSize);
        let frame = requestAnimationFrame(function draw() {
          analyser.getFloatTimeDomainData(samples);
          const rms = Math.sqrt(samples.reduce((sum, s) => sum + s * s, 0) / samples.length);
          const level = Math.min(1, rms * 4);
          if (meter.current) {
            meter.current.style.transform = `scaleX(${level})`;
            meter.current.dataset.level = level.toFixed(3);
            meter.current.parentElement?.setAttribute("aria-valuenow", String(Math.round(level * 100)));
          }
          frame = requestAnimationFrame(draw);
        });
        stop = () => {
          cancelAnimationFrame(frame);
          void ctx.close();
          track.stop();
        };
        refreshDevices();
      })
      .catch((err) => {
        if (!live) return;
        setMicError(deviceMessage(err, "microphone"));
        setMic(false);
      });
    return () => {
      live = false;
      stop();
    };
  }, [canCapture, mic, micId, refreshDevices]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setJoining(true);
    setJoinError(null);
    try {
      const trimmed = name.trim();
      const answer = await joinWithLink(code, trimmed);
      remember(trimmed);
      const choices = { mic: mic && canCapture, camera: camera && canCapture, micId: micId || undefined, cameraId: cameraId || undefined, speakerId: speakerId || undefined };
      if (isWaiting(answer)) onWaiting(answer.ticket, choices);
      else onJoined(answer, choices);
    } catch (err) {
      setJoinError(err instanceof JoinError ? err.message : "Could not join. Try again.");
      setJoining(false);
    }
  };

  const canPickSpeaker = speakerChoiceSupported();
  const of = (kind: MediaDeviceKind) => devices.filter((d) => d.kind === kind && d.deviceId);

  return (
    <Centered wide={canCapture}>
      <div data-testid="prejoin" className={canCapture ? "grid gap-6 md:grid-cols-[3fr_2fr]" : ""}>
        {canCapture && (
          <div className="flex min-w-0 flex-col gap-3">
            <div className="relative aspect-video overflow-hidden rounded-lg bg-surface">
              <video ref={video} muted playsInline autoPlay data-testid="preview-video" className={`size-full -scale-x-100 object-cover ${camera ? "" : "hidden"}`} />
              {!camera && <div className="flex size-full items-center justify-center text-sm text-ink-3">Camera is off</div>}
            </div>
            <div className="flex flex-wrap gap-2">
              <Toggle on={mic} onChange={setMic} testId="prejoin-mic">
                {mic ? <Mic aria-hidden /> : <MicOff aria-hidden />} {mic ? "Microphone on" : "Microphone off"}
              </Toggle>
              <Toggle on={camera} onChange={setCamera} testId="prejoin-camera">
                {camera ? <Camera aria-hidden /> : <CameraOff aria-hidden />} {camera ? "Camera on" : "Camera off"}
              </Toggle>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-raised" role="meter" aria-label="Microphone level" aria-valuemin={0} aria-valuemax={100} aria-valuenow={0}>
              <div ref={meter} data-testid="mic-level" className="h-full origin-left scale-x-0 bg-signal" />
            </div>
            {(cameraError || micError) && (
              <p role="alert" className="text-sm text-critical-text">
                {[micError, cameraError].filter(Boolean).join(" ")}
              </p>
            )}
            <div className="grid gap-2 sm:grid-cols-3">
              <DeviceSelect label="Microphone" testId="device-mic" devices={of("audioinput")} value={micId} onChange={setMicId} />
              <DeviceSelect label="Camera" testId="device-camera" devices={of("videoinput")} value={cameraId} onChange={setCameraId} />
              {canPickSpeaker && <DeviceSelect label="Speaker" testId="device-speaker" devices={of("audiooutput")} value={speakerId} onChange={setSpeakerId} />}
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Button
                data-testid="test-speaker"
                disabled={testing}
                onClick={() => {
                  setTesting(true);
                  setToneError(null);
                  setTone(null);
                  playTestTone(speakerId)
                    .then(setTone)
                    .catch(() => setToneError("Could not play a sound on this speaker."))
                    .finally(() => setTesting(false));
                }}
              >
                <Volume2 aria-hidden /> {testing ? "Playing…" : "Test speaker"}
              </Button>
              {tone && (
                <span role="status" data-testid="speaker-tone-result" data-sink={tone.sinkId} data-peak={tone.peak.toFixed(3)} className="text-xs text-ink-2">
                  Played a tone on {tone.sinkLabel}. If you heard nothing, try another speaker.
                </span>
              )}
              {toneError && (
                <span role="alert" data-testid="speaker-tone-error" className="text-sm text-critical-text">
                  {toneError}
                </span>
              )}
            </div>
            {!canPickSpeaker && (
              <p data-testid="speaker-unsupported" className="text-xs text-ink-3">
                This browser plays sound through its default speaker, and cannot choose another from a page.
              </p>
            )}
          </div>
        )}
        <form onSubmit={submit} className="flex min-w-0 flex-col gap-3 self-center">
          <div>
            <p className="text-xs text-ink-3">You are joining</p>
            <h1 data-testid="meeting-title" className="text-xl font-semibold break-words">
              {meetingTitle(link)}
            </h1>
          </div>
          {link.role === "host" && (
            <p data-testid="prejoin-host" className="text-sm text-ink-2">
              {link.ended ? "You are starting this meeting again, as a host." : "You are joining as a host: you can mute and remove people, lock the meeting, record it and end it."}
            </p>
          )}
          {link.role !== "host" && link.locked && (
            <p role="alert" data-testid="prejoin-locked" className="text-sm text-warn-text">
              This meeting is locked. You can join once a host unlocks it.
            </p>
          )}
          {link.role !== "host" && !link.locked && link.waiting_room && (
            <p data-testid="prejoin-waiting" className="text-sm text-ink-2">
              Someone running the meeting will let you in after you ask to join.
            </p>
          )}
          {!link.can_publish && (
            <p data-testid="watch-only" className="text-sm text-ink-2">
              This link lets you watch and listen. Your camera and microphone stay off.
            </p>
          )}
          {link.can_publish && !secure && (
            <p role="alert" data-testid="insecure-page" className="text-sm text-warn-text">
              This page is not on a secure (https) address, so your browser will not let it use a camera or microphone. You can still join to watch and listen.
            </p>
          )}
          <label className="flex flex-col gap-1 text-sm font-medium">
            Your name
            <input data-testid="display-name" value={name} maxLength={100} autoComplete="name" onChange={(e) => setName(e.target.value)} placeholder="Guest" className="h-10 rounded-md border border-line-2 bg-surface px-3 font-normal" />
          </label>
          {joinError && (
            <p role="alert" data-testid="join-error" className="text-sm text-critical-text">
              {joinError}
            </p>
          )}
          <Button type="submit" variant="primary" disabled={joining} data-testid="join-button">
            {joining ? "Joining…" : link.role !== "host" && link.waiting_room ? "Ask to join" : "Join now"}
          </Button>
        </form>
      </div>
    </Centered>
  );
}
