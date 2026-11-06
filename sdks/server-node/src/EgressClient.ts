import { NotSupportedError, Transport, camelCased, pageQuery, type ClientOptions } from './http.js';

export type EgressType = 'room_composite' | 'participant' | 'audio_only' | 'video_only';
export type EgressStatus = 'starting' | 'active' | 'ending' | 'complete' | 'failed' | 'aborted';

/** A recording or streaming job. Times are RFC 3339. */
export interface Egress {
  id: string;
  projectId: string;
  roomName: string;
  roomSid: string;
  type: EgressType;
  status: EgressStatus;
  format: string;
  outputUrl: string;
  streamUrls: string[];
  fileSize: number;
  durationMs: number;
  error: string;
  createdAt: string;
  startedAt: string | null;
  endedAt: string | null;
  retryOf: string | null;
}

/** Says where a job sends its media. Give a format to record a file, stream URLs to stream, or both. */
export interface EgressOutput {
  /** Format of the file. */
  format?: 'mp4' | 'webm';
  /** rtmp:// or rtmps:// addresses, at most five. */
  streamUrls?: string[];
  /** Layout of a composite. The default is grid. */
  layout?: 'grid' | 'speaker';
}

export interface ListOptions {
  limit?: number;
  cursor?: string;
}

export interface EgressPage {
  items: Egress[];
  /** Fetches the next page. Empty means this is the last. */
  nextCursor: string;
}

const RECORD = { roomRecord: true };

/** Whether a job has finished, one way or another. */
export function egressEnded(job: Egress): boolean {
  return job.status === 'complete' || job.status === 'failed' || job.status === 'aborted';
}

/**
 * Starts and manages recordings and stream outputs, over this server's REST API. The server needs a control plane and
 * an egress worker.
 */
export class EgressClient {
  private readonly transport: Transport;

  constructor(url: string, apiKey?: string, apiSecret?: string, options?: ClientOptions) {
    this.transport = new Transport(url, apiKey, apiSecret, options);
  }

  private async job(verb: string, path: string, body?: unknown): Promise<Egress> {
    return camelCased<Egress>(await (await this.transport.rest(verb, path, RECORD, body)).json());
  }

  private start(type: EgressType, room: string, identity: string | undefined, output: EgressOutput): Promise<Egress> {
    return this.job('POST', '/egress', {
      type,
      room,
      participant: identity || undefined,
      layout: output.layout,
      output: output.format ? { kind: 'file', format: output.format } : undefined,
      stream_urls: output.streamUrls,
    });
  }

  /** Records or streams a whole room as one picture. */
  startRoomCompositeEgress(room: string, output: EgressOutput): Promise<Egress> {
    return this.start('room_composite', room, undefined, output);
  }

  /** Records or streams one participant. */
  startParticipantEgress(room: string, identity: string, output: EgressOutput): Promise<Egress> {
    return this.start('participant', room, identity, output);
  }

  /** Records or streams a room's audio, or one participant's when identity is given. */
  startAudioOnlyEgress(room: string, output: EgressOutput, identity?: string): Promise<Egress> {
    return this.start('audio_only', room, identity, output);
  }

  /** Records or streams a room's video, or one participant's when identity is given. */
  startVideoOnlyEgress(room: string, output: EgressOutput, identity?: string): Promise<Egress> {
    return this.start('video_only', room, identity, output);
  }

  /** Not supported: this server only records rooms, not arbitrary pages. */
  async startWebEgress(): Promise<never> {
    throw new NotSupportedError('Web egress: this server does not do this.');
  }

  /** Not supported. Use startAudioOnlyEgress or startVideoOnlyEgress for one kind of media. */
  async startTrackCompositeEgress(): Promise<never> {
    throw new NotSupportedError('Track composite egress: this server does not do this.');
  }

  /** Not supported. */
  async startTrackEgress(): Promise<never> {
    throw new NotSupportedError('Track egress: this server does not do this.');
  }

  /** Not supported: a running job keeps the layout it started with. */
  async updateLayout(): Promise<never> {
    throw new NotSupportedError("Changing a running job's layout: this server does not do this.");
  }

  /** Not supported: a running job keeps the stream outputs it started with. */
  async updateStream(): Promise<never> {
    throw new NotSupportedError("Changing a running job's stream outputs: this server does not do this.");
  }

  private async page(path: string, options: ListOptions): Promise<EgressPage> {
    const res = await this.transport.rest('GET', path + pageQuery(options.limit, options.cursor), RECORD);
    return camelCased<EgressPage>(await res.json());
  }

  /** Lists jobs, newest first. */
  listEgress(options: ListOptions = {}): Promise<EgressPage> {
    return this.page('/egress', options);
  }

  /** Returns one job. */
  getEgress(id: string): Promise<Egress> {
    return this.job('GET', `/egress/${encodeURIComponent(id)}`);
  }

  /** Stops a running job. The file, if any, is complete once the job's status says so. */
  stopEgress(id: string): Promise<Egress> {
    return this.job('POST', `/egress/${encodeURIComponent(id)}/stop`);
  }

  /** Lists jobs with a file output. */
  listRecordings(options: ListOptions = {}): Promise<EgressPage> {
    return this.page('/recordings', options);
  }

  /** Returns one job with a file output. */
  getRecording(id: string): Promise<Egress> {
    return this.job('GET', `/recordings/${encodeURIComponent(id)}`);
  }

  /** Fetches a recording's file. Read the body of the response you get back. The client's timeout does not apply. */
  downloadRecording(id: string): Promise<Response> {
    return this.transport.rest('GET', `/recordings/${encodeURIComponent(id)}/file`, RECORD, undefined, { download: true });
  }

  /** Starts a new job with the same request as a finished one. */
  retryRecording(id: string): Promise<Egress> {
    return this.job('POST', `/recordings/${encodeURIComponent(id)}/retry`);
  }

  /** Deletes a finished job and its file. */
  async deleteRecording(id: string): Promise<void> {
    await this.transport.rest('DELETE', `/recordings/${encodeURIComponent(id)}`, RECORD);
  }
}
