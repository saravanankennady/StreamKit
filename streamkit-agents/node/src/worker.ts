// A worker holds a WebSocket open to the server's /agent path, accepts jobs, and runs them, as tasks in this process.
import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { WebSocket } from 'ws';
import {
  AvailabilityResponseSchema,
  JobStatus,
  JobType,
  MigrateJobRequestSchema,
  RegisterWorkerRequestSchema,
  ServerMessageSchema,
  UpdateJobStatusSchema,
  UpdateWorkerStatusSchema,
  WorkerMessageSchema,
  WorkerStatus,
  type Job,
  type JobAssignment,
  type WorkerMessage,
} from './proto/livekit_agent_pb.js';
import type { Participant } from './rtc/room.ts';
import { Room } from './rtc/room.ts';
import { workerToken } from './token.ts';

const STATUS_INTERVAL_MS = 1500;
const ASSIGNMENT_TIMEOUT_MS = 7500;
const REQUEST_TIMEOUT_MS = 8000;
const MAX_RETRY_DELAY_S = 10;
const ATTR_AGENT_NAME = 'lk.agent.name';

export class JobContext {
  readonly job: Job;
  readonly url: string;
  readonly token: string;
  readonly room = new Room();
  private readonly worker: Worker;
  private readonly callbacks: Array<() => unknown> = [];
  private resolveShutdown!: () => void;
  readonly shutdownPromise: Promise<void>;
  shutdownReason = '';

  constructor(worker: Worker, job: Job, url: string, token: string) {
    this.worker = worker;
    this.job = job;
    this.url = url;
    this.token = token;
    this.shutdownPromise = new Promise((resolve) => (this.resolveShutdown = resolve));
  }

  get jobId(): string {
    return this.job.id;
  }

  get metadata(): string {
    return this.job.metadata;
  }

  async connect(): Promise<void> {
    await this.room.connect(this.url, this.token);
    this.room.on('disconnected', () => this.shutdown('disconnected from the room'));
    this.worker.reportJob(this.job.id, JobStatus.JS_RUNNING);
  }

  addShutdownCallback(callback: () => unknown): void {
    this.callbacks.push(callback);
  }

  shutdown(reason = ''): void {
    if (!this.shutdownReason) this.shutdownReason = reason || 'shut down';
    this.resolveShutdown();
  }

  async runCallbacks(): Promise<void> {
    for (const callback of this.callbacks) {
      try {
        await callback();
      } catch (e) {
        console.error('a shutdown callback failed', e);
      }
    }
  }

  /** Waits for someone other than an agent to be in the room. */
  waitForParticipant(identity?: string): Promise<Participant> {
    const match = (p: Participant) => (identity ? p.identity === identity : !p.isAgent);
    for (const p of this.room.remoteParticipants.values()) if (match(p)) return Promise.resolve(p);
    return new Promise((resolve) => {
      const onConnected = (p: Participant) => {
        if (match(p)) {
          this.room.off('participantConnected', onConnected);
          resolve(p);
        }
      };
      this.room.on('participantConnected', onConnected);
    });
  }
}

export class JobRequest {
  readonly job: Job;
  answer: WorkerMessage | null = null;

  constructor(job: Job) {
    this.job = job;
  }

  accept(options: { identity?: string; name?: string; metadata?: string; attributes?: Record<string, string> } = {}): void {
    this.answer ??= create(WorkerMessageSchema, {
      message: {
        case: 'availability',
        value: create(AvailabilityResponseSchema, {
          jobId: this.job.id,
          available: true,
          participantIdentity: options.identity || `agent-${this.job.id}`,
          participantName: options.name ?? '',
          participantMetadata: options.metadata ?? '',
          participantAttributes: options.attributes ?? {},
        }),
      },
    });
  }

  reject(options: { terminate?: boolean } = {}): void {
    this.answer ??= create(WorkerMessageSchema, {
      message: { case: 'availability', value: create(AvailabilityResponseSchema, { jobId: this.job.id, available: false, terminate: options.terminate ?? false }) },
    });
  }
}

export interface WorkerOptions {
  entrypoint: (ctx: JobContext) => Promise<void>;
  wsUrl?: string;
  apiKey?: string;
  apiSecret?: string;
  agentName?: string;
  jobType?: JobType;
  requestFn?: (request: JobRequest) => Promise<void> | void;
  loadFn?: () => number;
  loadThreshold?: number;
  version?: string;
}

export class Worker {
  readonly agentName: string;
  readonly wsUrl: string;
  readonly jobs = new Map<string, JobContext>();
  workerId = '';
  private readonly options: WorkerOptions;
  private readonly apiKey: string;
  private readonly apiSecret: string;
  private ws: WebSocket | null = null;
  private closed = false;
  private retries = 0;
  private statusTimer: NodeJS.Timeout | null = null;
  private readonly assignments = new Map<string, (a: JobAssignment) => void>();
  private registeredResolve: (() => void) | null = null;
  private registered: Promise<void>;

  constructor(options: WorkerOptions) {
    this.options = options;
    this.wsUrl = options.wsUrl ?? process.env.STREAMKIT_URL ?? '';
    this.apiKey = options.apiKey ?? process.env.STREAMKIT_API_KEY ?? '';
    this.apiSecret = options.apiSecret ?? process.env.STREAMKIT_API_SECRET ?? '';
    if (!this.wsUrl || !this.apiKey || !this.apiSecret) throw new Error('a worker needs a URL, an API key and an API secret');
    this.agentName = options.agentName ?? '';
    this.registered = new Promise((resolve) => (this.registeredResolve = resolve));
  }

  start(): void {
    this.connect();
  }

  waitRegistered(): Promise<void> {
    return this.registered;
  }

  private connect(): void {
    const url = this.wsUrl.replace(/\/$/, '').replace(/^http/, 'ws') + '/agent';
    const ws = new WebSocket(url, { headers: { Authorization: `Bearer ${workerToken(this.apiKey, this.apiSecret)}` } });
    ws.binaryType = 'nodebuffer';
    this.ws = ws;
    ws.on('open', () => {
      this.send({
        case: 'register',
        value: create(RegisterWorkerRequestSchema, {
          type: this.options.jobType ?? JobType.JT_ROOM,
          agentName: this.agentName,
          version: this.options.version ?? '0.1.0',
        }),
      });
    });
    ws.on('message', (raw: Buffer) => this.onMessage(raw));
    ws.on('error', (e) => console.warn('worker connection failed:', (e as Error).name));
    ws.on('close', () => this.onClose());
  }

  private onClose(): void {
    if (this.statusTimer) clearInterval(this.statusTimer);
    this.statusTimer = null;
    this.ws = null;
    if (this.closed) return;
    this.retries += 1;
    setTimeout(() => this.connect(), Math.min(this.retries * 2, MAX_RETRY_DELAY_S) * 1000);
  }

  private send(message: WorkerMessage['message']): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(toBinary(WorkerMessageSchema, create(WorkerMessageSchema, { message })));
    }
  }

  reportJob(jobId: string, status: JobStatus, error = ''): void {
    this.send({ case: 'updateJob', value: create(UpdateJobStatusSchema, { jobId, status, error }) });
  }

  private onMessage(raw: Buffer): void {
    const m = fromBinary(ServerMessageSchema, raw).message;
    switch (m.case) {
      case 'register':
        this.workerId = m.value.workerId;
        this.retries = 0;
        this.registeredResolve?.();
        if (this.jobs.size > 0) {
          this.send({ case: 'migrateJob', value: create(MigrateJobRequestSchema, { jobIds: [...this.jobs.keys()] }) });
        }
        this.statusTimer = setInterval(() => this.reportStatus(), STATUS_INTERVAL_MS);
        break;
      case 'availability':
        void this.answerOffer(m.value.job!);
        break;
      case 'assignment': {
        const resolve = this.assignments.get(m.value.job?.id ?? '');
        if (resolve) resolve(m.value);
        break;
      }
      case 'termination':
        this.jobs.get(m.value.jobId)?.shutdown('the server terminated the job');
        break;
      default:
        break;
    }
  }

  private reportStatus(): void {
    const load = this.options.loadFn?.() ?? 0;
    const full = load >= (this.options.loadThreshold ?? 0.7);
    this.send({
      case: 'updateWorker',
      value: create(UpdateWorkerStatusSchema, { status: full ? WorkerStatus.WS_FULL : WorkerStatus.WS_AVAILABLE, load, jobCount: this.jobs.size }),
    });
  }

  private async answerOffer(job: Job): Promise<void> {
    const request = new JobRequest(job);
    try {
      const decide = this.options.requestFn ?? ((r: JobRequest) => r.accept());
      await Promise.race([decide(request), new Promise((resolve) => setTimeout(resolve, REQUEST_TIMEOUT_MS))]);
    } catch (e) {
      console.error('the request function failed', e);
    }
    if (!request.answer) request.reject();
    const answer = request.answer!;
    const value = answer.message.value as { available: boolean; participantAttributes: Record<string, string> };
    if (!value.available) {
      this.send(answer.message);
      return;
    }
    value.participantAttributes[ATTR_AGENT_NAME] = this.agentName;
    const assignment = await new Promise<JobAssignment | null>((resolve) => {
      const timer = setTimeout(() => {
        this.assignments.delete(job.id);
        resolve(null);
      }, ASSIGNMENT_TIMEOUT_MS);
      this.assignments.set(job.id, (a) => {
        clearTimeout(timer);
        this.assignments.delete(job.id);
        resolve(a);
      });
      this.send(answer.message);
    });
    if (assignment) void this.runJob(assignment);
  }

  private async runJob(assignment: JobAssignment): Promise<void> {
    const ctx = new JobContext(this, assignment.job!, assignment.url || this.wsUrl, assignment.token);
    this.jobs.set(ctx.jobId, ctx);
    let status = JobStatus.JS_SUCCESS;
    let error = '';
    try {
      await Promise.race([this.options.entrypoint(ctx).then(() => ctx.shutdownPromise), ctx.shutdownPromise]);
    } catch (e) {
      console.error(`job ${ctx.jobId} failed`, e);
      status = JobStatus.JS_FAILED;
      error = (e as Error).name;
    }
    await ctx.runCallbacks();
    await ctx.room.disconnect();
    this.jobs.delete(ctx.jobId);
    this.reportJob(ctx.jobId, status, error);
  }

  async close(): Promise<void> {
    this.closed = true;
    for (const ctx of this.jobs.values()) ctx.shutdown('the worker is closing');
    const deadline = Date.now() + 5000;
    while (this.jobs.size > 0 && Date.now() < deadline) await new Promise((r) => setTimeout(r, 50));
    this.ws?.close();
  }
}
