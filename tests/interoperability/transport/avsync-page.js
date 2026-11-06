// Injected into test pages before anything else runs. It holds what a publisher and a viewer do to measure how far apart
// audio and video really are: ground truth, because a browser's own statistics report what it believes (GAP-034, GAP-039).
(() => {
  const params = new URLSearchParams(location.search);
  const videoDelayMs = Number(params.get('videoDelayMs') ?? -1);

  // A delay between encoder and packetizer, which leaves RTP timestamps and sender reports on the capture clock. That is
  // what a slow hardware encoder looks like from outside. It has to be set up as the connection is made, and every
  // sender on such a connection has to pipe its frames through, or nothing is sent.
  if (videoDelayMs >= 0) {
    const Native = window.RTCPeerConnection;
    const pipe = (sender, delayMs) => {
      const { readable, writable } = sender.createEncodedStreams();
      const reader = readable.getReader();
      const writer = writable.getWriter();
      (async () => {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) return;
          if (delayMs > 0) setTimeout(() => writer.write(value), delayMs);
          else writer.write(value);
        }
      })();
    };
    window.RTCPeerConnection = class extends Native {
      constructor(config = {}, ...rest) {
        super({ ...config, encodedInsertableStreams: true }, ...rest);
      }
      setConfiguration(config = {}) {
        // Without this the change to the setting is refused, and the client's connection never comes up.
        return super.setConfiguration({ ...config, encodedInsertableStreams: true });
      }
      addTransceiver(track, init) {
        const transceiver = super.addTransceiver(track, init);
        const kind = typeof track === 'string' ? track : track.kind;
        pipe(transceiver.sender, kind === 'video' ? videoDelayMs : 0);
        return transceiver;
      }
    };
  }

  // Paints a white frame and starts a tone in the same callback, every 1.5 s.
  window.avPublish = async () => {
    const { room, Track } = window.harness;
    const canvas = document.createElement('canvas');
    canvas.width = 320;
    canvas.height = 180;
    const g = canvas.getContext('2d');
    const paint = (color) => {
      g.fillStyle = color;
      g.fillRect(0, 0, 320, 180);
    };
    paint('#000');
    const video = canvas.captureStream(30).getVideoTracks()[0];

    const ctx = new AudioContext({ sampleRate: 48000 });
    await ctx.resume();
    const oscillator = ctx.createOscillator();
    oscillator.frequency.value = 1000;
    const gain = ctx.createGain();
    gain.gain.value = 0;
    const out = ctx.createMediaStreamDestination();
    oscillator.connect(gain).connect(out);
    oscillator.start();
    const audio = out.stream.getAudioTracks()[0];

    await room.localParticipant.publishTrack(video, { name: 'camera', source: Track.Source.Camera, simulcast: false, videoCodec: 'vp8', backupCodec: false, videoEncoding: { maxBitrate: 400000, maxFramerate: 30 } });
    await room.localParticipant.publishTrack(audio, { name: 'microphone', source: Track.Source.Microphone, dtx: false, red: false });

    let flashing = false;
    setInterval(() => {
      // Keep frames coming, or nothing gets encoded between flashes.
      if (!flashing) paint('#000');
      g.fillStyle = '#111';
      g.fillRect(Math.random() * 300, 0, 8, 8);
    }, 33);
    window.avFlashes = 0;
    setInterval(() => {
      flashing = true;
      paint('#fff');
      gain.gain.setValueAtTime(0.8, ctx.currentTime);
      window.avFlashes++;
      setTimeout(() => {
        flashing = false;
        gain.gain.setValueAtTime(0, ctx.currentTime);
        paint('#000');
      }, 200);
    }, 1500);
    return true;
  };

  const toneDetector = `
    class ToneDetector extends AudioWorkletProcessor {
      constructor() {
        super();
        this.loud = false;
      }
      process(inputs) {
        const input = inputs[0] && inputs[0][0];
        if (!input) return true;
        let sum = 0;
        for (let i = 0; i < input.length; i++) sum += input[i] * input[i];
        const rms = Math.sqrt(sum / input.length);
        const loud = rms > 0.05;
        if (loud !== this.loud) {
          this.loud = loud;
          if (loud) this.port.postMessage(currentTime);
        }
        return true;
      }
    }
    registerProcessor('tone-detector', ToneDetector);
  `;

  // Detects flashes on the video element and tones on the audio track, each on this page's clock.
  window.avWatch = async (identity) => {
    const { room } = window.harness;
    const flashes = [];
    const tones = [];
    const find = (kind) => {
      const p = room.remoteParticipants.get(identity);
      return p && [...p.trackPublications.values()].find((x) => x.kind === kind)?.track;
    };
    for (let i = 0; i < 300 && !(find('video') && find('audio') && document.querySelector('audio')); i++) await new Promise((r) => setTimeout(r, 100));
    const videoTrack = find('video');
    const audioTrack = find('audio');
    if (!videoTrack || !audioTrack) throw new Error('the tracks did not arrive');

    // Playing out loud is what makes the browser line the audio up. The tone detector taps the same stream.
    document.querySelectorAll('audio').forEach((el) => {
      el.muted = false;
      el.volume = 0.01;
      el.play().catch(() => {});
    });
    const ctx = new AudioContext({ sampleRate: 48000 });
    await ctx.resume();
    await ctx.audioWorklet.addModule(URL.createObjectURL(new Blob([toneDetector], { type: 'text/javascript' })));
    const source = ctx.createMediaStreamSource(new MediaStream([audioTrack.mediaStreamTrack]));
    const detector = new AudioWorkletNode(ctx, 'tone-detector');
    detector.port.onmessage = () => tones.push(performance.now());
    source.connect(detector);

    const el = document.querySelector('video[data-kind="video"]');
    const canvas = document.createElement('canvas');
    canvas.width = 8;
    canvas.height = 8;
    const g = canvas.getContext('2d', { willReadFrequently: true });
    let bright = false;
    const onFrame = (now, meta) => {
      g.drawImage(el, 0, 0, 8, 8);
      const d = g.getImageData(0, 0, 8, 8).data;
      let sum = 0;
      for (let i = 0; i < d.length; i += 4) sum += d[i];
      const white = sum / 64 > 128;
      if (white !== bright) {
        bright = white;
        if (white) flashes.push(meta.expectedDisplayTime ?? now);
      }
      el.requestVideoFrameCallback(onFrame);
    };
    el.requestVideoFrameCallback(onFrame);

    const stats = async () => {
      const summarize = async (track) => {
        const report = await track.getRTCStatsReport();
        const out = {};
        report?.forEach((s) => {
          if (s.type === 'inbound-rtp') {
            out.playout = s.estimatedPlayoutTimestamp ?? null;
            out.jitterBufferDelay = s.jitterBufferDelay;
            out.jitterBufferTargetDelay = s.jitterBufferTargetDelay;
            out.jitterBufferMinimumDelay = s.jitterBufferMinimumDelay;
            out.jitterBufferEmittedCount = s.jitterBufferEmittedCount;
            out.framesDropped = s.framesDropped ?? null;
            out.freezeCount = s.freezeCount ?? null;
          }
          if (s.type === 'remote-outbound-rtp') {
            out.reportsSent = s.reportsSent;
            out.remoteTimestamp = s.remoteTimestamp;
          }
        });
        return out;
      };
      return { audio: await summarize(audioTrack), video: await summarize(videoTrack) };
    };

    window.avResult = () => ({ flashes: flashes.slice(), tones: tones.slice() });
    window.avStats = stats;
    return true;
  };
})();
