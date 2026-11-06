// SDP edits a browser's API has no setting for. These are text edits on purpose: the changes are small,
// and a parse and reprint could disturb lines the other side depends on.

const EOL = '\r\n';

interface Section {
  lines: string[];
  kind: string;
  mid?: string;
}

function split(sdp: string): { head: string[]; sections: Section[] } {
  const head: string[] = [];
  const sections: Section[] = [];
  for (const line of sdp.split(/\r?\n/)) {
    if (line === '') continue;
    if (line.startsWith('m=')) {
      sections.push({ lines: [line], kind: line.slice(2).split(' ')[0] });
      continue;
    }
    const current = sections[sections.length - 1];
    if (!current) {
      head.push(line);
      continue;
    }
    current.lines.push(line);
    if (line.startsWith('a=mid:')) current.mid = line.slice(6);
  }
  return { head, sections };
}

function join(head: string[], sections: Section[]): string {
  return [...head, ...sections.flatMap((s) => s.lines)].join(EOL) + EOL;
}

/** Payload types in a section whose rtpmap names the codec, case-insensitively. */
function payloadTypes(section: Section, codec: string): string[] {
  const out: string[] = [];
  const name = codec.toLowerCase();
  for (const line of section.lines) {
    const m = line.match(/^a=rtpmap:(\d+) ([^/]+)\//);
    if (m && m[2].toLowerCase() === name) out.push(m[1]);
  }
  return out;
}

/** Adds a parameter to a payload type's fmtp line, creating the line if the codec has none. */
function addFmtp(section: Section, pt: string, param: string) {
  const prefix = `a=fmtp:${pt} `;
  const at = section.lines.findIndex((l) => l.startsWith(prefix));
  if (at >= 0) {
    const key = param.split('=')[0];
    const existing = section.lines[at].slice(prefix.length).split(';').map((p) => p.trim().split('=')[0]);
    if (!existing.includes(key)) section.lines[at] += `;${param}`;
    return;
  }
  // Fmtp lines belong after the codec's rtpmap and any rtcp-fb lines.
  let last = section.lines.findIndex((l) => l.startsWith(`a=rtpmap:${pt} `));
  if (last < 0) return;
  while (section.lines[last + 1]?.startsWith(`a=rtcp-fb:${pt} `)) last++;
  section.lines.splice(last + 1, 0, `${prefix}${param}`);
}

/**
 * Offers stereo Opus on every audio section. It only allows stereo: mono capture stays mono, and not
 * offering it would stop a stereo source being heard as stereo.
 */
export function enableOpusStereo(sdp: string): string {
  const { head, sections } = split(sdp);
  for (const s of sections) {
    if (s.kind !== 'audio') continue;
    for (const pt of payloadTypes(s, 'opus')) addFmtp(s, pt, 'stereo=1');
  }
  return join(head, sections);
}

/**
 * Tells the browser's encoder where to start. Without this it starts near 300 kbps and its first seconds
 * of video are visibly poor.
 */
export function setStartBitrate(sdp: string, trackId: string, codec: string, kbps: number): string {
  const { head, sections } = split(sdp);
  const section = sections.find((s) => s.kind === 'video' && s.lines.some((l) => l.startsWith('a=msid:') && l.includes(trackId)));
  if (!section) return sdp;
  const pts = payloadTypes(section, codec);
  if (pts.length === 0) return sdp;
  addFmtp(section, pts[0], `x-google-start-bitrate=${Math.round(kbps)}`);
  return join(head, sections);
}

const DEPENDENCY_DESCRIPTOR_URI = 'https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension';

/**
 * Asks for the dependency descriptor on a track's section, which browsers leave out unless told. It is what lets a server
 * give each viewer only the layers it wants from an SVC stream. Without it a VP9 server can still go by VP9's own headers,
 * at a cost, and an AV1 one cannot choose at all. Browsers then refuse to decode AV1 received without it.
 *
 * One URI has one ID across a bundle, so an ID already in use for it elsewhere wins. A new one goes above every ID in use,
 * because the browser assigns its own without knowing of this one, and steps over 15, which RFC 8285 reserves.
 */
export function addDependencyDescriptor(sdp: string, trackId: string): string {
  const { head, sections } = split(sdp);
  const section = sections.find((s) => s.kind === 'video' && s.lines.some((l) => l.startsWith('a=msid:') && l.includes(trackId)));
  if (!section || section.lines.some((l) => l.endsWith(` ${DEPENDENCY_DESCRIPTOR_URI}`))) return sdp;

  let id: number | undefined;
  let highest = 0;
  for (const s of sections) {
    for (const line of s.lines) {
      const m = line.match(/^a=extmap:(\d+)(?:\/\w+)? (.+)$/);
      if (!m) continue;
      highest = Math.max(highest, Number(m[1]));
      if (m[2] === DEPENDENCY_DESCRIPTOR_URI) id = Number(m[1]);
    }
  }
  if (id !== undefined && sections.some((s) => s.lines.some((l) => l.startsWith(`a=extmap:${id} `) && !l.endsWith(` ${DEPENDENCY_DESCRIPTOR_URI}`)))) {
    // The browser would reject a bundle that used this ID for two things, and its half of the map is not this code's to change.
    return sdp;
  }
  id ??= highest + 1 === 15 ? 16 : highest + 1;

  let at = -1;
  section.lines.forEach((l, i) => {
    if (l.startsWith('a=extmap:')) at = i;
  });
  if (at < 0) at = section.lines.findIndex((l) => l.startsWith('a=mid:'));
  section.lines.splice(at + 1, 0, `a=extmap:${id} ${DEPENDENCY_DESCRIPTOR_URI}`);
  return join(head, sections);
}

export interface OfferedAudioFeatures {
  stereoMids: Set<string>;
  nackMids: Set<string>;
}

/** What the server offered that an answer has to ask for explicitly to receive. */
export function readOfferedAudioFeatures(offer: string): OfferedAudioFeatures {
  const out: OfferedAudioFeatures = { stereoMids: new Set(), nackMids: new Set() };
  for (const s of split(offer).sections) {
    if (s.kind !== 'audio' || !s.mid) continue;
    for (const pt of payloadTypes(s, 'opus')) {
      if (s.lines.some((l) => l.startsWith(`a=fmtp:${pt} `) && l.includes('sprop-stereo=1'))) s.mid && out.stereoMids.add(s.mid);
      if (s.lines.includes(`a=rtcp-fb:${pt} nack`)) s.mid && out.nackMids.add(s.mid);
    }
  }
  return out;
}

export function applyAudioFeaturesToAnswer(answer: string, features: OfferedAudioFeatures): string {
  const { head, sections } = split(answer);
  for (const s of sections) {
    if (s.kind !== 'audio' || !s.mid) continue;
    for (const pt of payloadTypes(s, 'opus')) {
      if (features.stereoMids.has(s.mid)) addFmtp(s, pt, 'stereo=1');
      if (features.nackMids.has(s.mid) && !s.lines.includes(`a=rtcp-fb:${pt} nack`)) {
        const rtpmap = s.lines.findIndex((l) => l.startsWith(`a=rtpmap:${pt} `));
        s.lines.splice(rtpmap + 1, 0, `a=rtcp-fb:${pt} nack`);
      }
    }
  }
  return join(head, sections);
}

/** The ICE username of a description, which changes when ICE restarts. */
export function iceUfrag(sdp: string | undefined): string | undefined {
  return sdp?.match(/^a=ice-ufrag:(.+?)\r?$/m)?.[1];
}
