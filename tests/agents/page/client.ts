// A person in a browser: the official livekit-client, used here only as a black-box test client. It records what it hears from
// the agent, and when, by decoding the subscribed track in the browser's own audio stack.
import { Room, RoomEvent, Track, type RemoteParticipant, type RemoteTrack } from 'livekit-client';

declare global {
  interface Window {
    agentTest: ReturnType<typeof makeApi>;
  }
}

const LOUD = 0.01;

function makeApi() {
  const room = new Room();
  const context = new AudioContext({ sampleRate: 48000 });
  const chunks: Float32Array[] = [];
  const state = {
    connected: false,
    agentIdentity: '',
    agentAttributes: {} as Record<string, string>,
    firstHeardAt: 0,
    lastHeardAt: 0,
    micLastLoudAt: 0,
    micFirstLoudAt: 0,
    transcripts: [] as Array<{ identity: string; text: string; attributes: Record<string, string> }>,
    errors: [] as string[],
    subscribed: false,
  };

  function listen(track: MediaStreamTrack, onSamples: (samples: Float32Array, at: number) => void): void {
    const source = context.createMediaStreamSource(new MediaStream([track]));
    const processor = context.createScriptProcessor(2048, 1, 1);
    processor.onaudioprocess = (event) => onSamples(new Float32Array(event.inputBuffer.getChannelData(0)), Date.now());
    source.connect(processor);
    processor.connect(context.destination);
  }

  function rms(samples: Float32Array): number {
    let sum = 0;
    for (const s of samples) sum += s * s;
    return Math.sqrt(sum / samples.length);
  }

  room.on(RoomEvent.TrackSubscribed, (track: RemoteTrack, _pub, participant: RemoteParticipant) => {
    if (track.kind !== Track.Kind.Audio) return;
    state.subscribed = true;
    // Chrome only delivers a remote stream to Web Audio while an element is playing it.
    const element = track.attach();
    element.muted = true;
    document.body.appendChild(element);
    void element.play().catch(() => {});
    state.agentIdentity = participant.identity;
    listen(track.mediaStreamTrack, (samples, at) => {
      chunks.push(samples);
      if (rms(samples) >= LOUD) {
        if (!state.firstHeardAt) state.firstHeardAt = at;
        state.lastHeardAt = at;
      }
    });
  });

  room.on(RoomEvent.ParticipantAttributesChanged, (_changed, participant) => {
    if (participant.identity === state.agentIdentity) state.agentAttributes = { ...participant.attributes };
  });

  room.registerTextStreamHandler('lk.transcription', async (reader, participant) => {
    const text = await reader.readAll();
    state.transcripts.push({ identity: participant.identity, text, attributes: { ...reader.info.attributes } });
  });

  return {
    async connect(url: string, token: string) {
      await context.resume();
      await room.connect(url, token);
      state.connected = true;
    },
    async enableMic() {
      const publication = await room.localParticipant.setMicrophoneEnabled(true);
      listen(publication!.track!.mediaStreamTrack, (samples, at) => {
        if (rms(samples) >= LOUD) {
          if (!state.micFirstLoudAt) state.micFirstLoudAt = at;
          state.micLastLoudAt = at;
        }
      });
    },
    state: () => JSON.parse(JSON.stringify(state)),
    resetHeard() {
      chunks.length = 0;
      state.firstHeardAt = 0;
      state.lastHeardAt = 0;
    },
    /** What was heard, as base64 of 16-bit PCM at the context's rate. */
    heardPcm() {
      const total = chunks.reduce((sum, c) => sum + c.length, 0);
      const pcm = new Int16Array(total);
      let at = 0;
      for (const chunk of chunks) {
        for (const s of chunk) pcm[at++] = Math.max(-32768, Math.min(32767, Math.round(s * 32767)));
      }
      let binary = '';
      const bytes = new Uint8Array(pcm.buffer);
      for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
      return { rate: context.sampleRate, data: btoa(binary) };
    },
    async sendChat(text: string) {
      await room.localParticipant.sendText(text, { topic: 'lk.chat' });
    },
    disconnect: () => room.disconnect(),
  };
}

window.agentTest = makeApi();
document.title = 'ready';
