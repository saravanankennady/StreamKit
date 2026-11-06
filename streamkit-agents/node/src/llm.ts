// Conversation history, tools and the LLM interface, in the same shape as the Python framework.
import { randomUUID } from 'node:crypto';

export type Role = 'system' | 'user' | 'assistant';

export interface ChatMessage {
  type: 'message';
  id: string;
  role: Role;
  content: string;
  interrupted: boolean;
}

export interface FunctionCall {
  type: 'function_call';
  id: string;
  callId: string;
  name: string;
  arguments: string;
}

export interface FunctionCallOutput {
  type: 'function_call_output';
  id: string;
  callId: string;
  name: string;
  output: string;
  isError: boolean;
}

export interface AgentHandoff {
  type: 'agent_handoff';
  id: string;
  oldAgent: string;
  newAgent: string;
}

export type ChatItem = ChatMessage | FunctionCall | FunctionCallOutput | AgentHandoff;

export const newId = (): string => randomUUID().replaceAll('-', '').slice(0, 16);

export class ChatContext {
  items: ChatItem[];

  constructor(items: ChatItem[] = []) {
    this.items = [...items];
  }

  addMessage(role: Role, content: string, interrupted = false): ChatMessage {
    const message: ChatMessage = { type: 'message', id: newId(), role, content, interrupted };
    this.items.push(message);
    return message;
  }

  add<T extends ChatItem>(item: T): T {
    this.items.push(item);
    return item;
  }

  copy(): ChatContext {
    return new ChatContext(this.items);
  }

  messages(): ChatMessage[] {
    return this.items.filter((i): i is ChatMessage => i.type === 'message');
  }

  setInstructions(instructions: string): void {
    const index = this.items.findIndex((i) => i.type === 'message' && i.role === 'system');
    const message: ChatMessage = { type: 'message', id: newId(), role: 'system', content: instructions, interrupted: false };
    if (index >= 0) this.items[index] = message;
    else this.items.unshift(message);
  }

  toOpenAI(): Array<Record<string, unknown>> {
    const out: Array<Record<string, unknown>> = [];
    for (const item of this.items) {
      if (item.type === 'message') {
        out.push({ role: item.role, content: item.content });
      } else if (item.type === 'function_call') {
        const call = { id: item.callId, type: 'function', function: { name: item.name, arguments: item.arguments } };
        const last = out.at(-1);
        // Parallel calls belong to one assistant message.
        if (last && last.role === 'assistant' && last.tool_calls) (last.tool_calls as unknown[]).push(call);
        else if (last && last.role === 'assistant' && last.content !== null) last.tool_calls = [call];
        else out.push({ role: 'assistant', content: null, tool_calls: [call] });
      } else if (item.type === 'function_call_output') {
        out.push({ role: 'tool', tool_call_id: item.callId, content: item.output });
      }
    }
    return out;
  }

  /** Returns the system prompt and messages. Roles must alternate and start with the user, so neighbours merge. */
  toAnthropic(): { system: string; messages: Array<{ role: string; content: unknown[] }> } {
    const system: string[] = [];
    const messages: Array<{ role: string; content: unknown[] }> = [];
    const add = (role: string, block: unknown) => {
      const last = messages.at(-1);
      if (last && last.role === role) last.content.push(block);
      else messages.push({ role, content: [block] });
    };
    for (const item of this.items) {
      if (item.type === 'message') {
        if (item.role === 'system') system.push(item.content);
        else if (item.content) add(item.role, { type: 'text', text: item.content });
      } else if (item.type === 'function_call') {
        add('assistant', { type: 'tool_use', id: item.callId, name: item.name, input: parseArgs(item.arguments) });
      } else if (item.type === 'function_call_output') {
        add('user', { type: 'tool_result', tool_use_id: item.callId, content: item.output, ...(item.isError ? { is_error: true } : {}) });
      }
    }
    if (messages[0] && messages[0].role !== 'user') messages.unshift({ role: 'user', content: [{ type: 'text', text: '(the conversation continues)' }] });
    return { system: system.join('\n\n'), messages };
  }

  /** Returns the system instruction and contents. */
  toGoogle(): { system: string; contents: Array<{ role: string; parts: unknown[] }> } {
    const system: string[] = [];
    const contents: Array<{ role: string; parts: unknown[] }> = [];
    const add = (role: string, part: unknown) => {
      const last = contents.at(-1);
      if (last && last.role === role) last.parts.push(part);
      else contents.push({ role, parts: [part] });
    };
    for (const item of this.items) {
      if (item.type === 'message') {
        if (item.role === 'system') system.push(item.content);
        else if (item.content) add(item.role === 'assistant' ? 'model' : 'user', { text: item.content });
      } else if (item.type === 'function_call') {
        add('model', { functionCall: { id: item.callId, name: item.name, args: parseArgs(item.arguments) } });
      } else if (item.type === 'function_call_output') {
        add('user', { functionResponse: { id: item.callId, name: item.name, response: { [item.isError ? 'error' : 'output']: item.output } } });
      }
    }
    return { system: system.join('\n\n'), contents };
  }
}

function parseArgs(raw: string): Record<string, unknown> {
  try {
    const value = JSON.parse(raw || '{}');
    return value && typeof value === 'object' && !Array.isArray(value) ? value : {};
  } catch {
    return {};
  }
}

/** Thrown by a tool to tell the LLM it failed, in words the LLM can act on. */
export class ToolError extends Error {}

/** Thrown by a tool or hook to end the turn with no spoken reply. */
export class StopResponse extends Error {}

export interface RunContext {
  session: unknown;
  speechHandle: unknown;
  functionCall: FunctionCall;
  userdata: unknown;
}

export interface FunctionTool {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
  execute: (args: Record<string, unknown>, ctx: RunContext) => unknown | Promise<unknown>;
}

/** Defines a tool. Node has no type hints to read at run time, so the JSON schema is written out. */
export function functionTool(tool: FunctionTool): FunctionTool {
  return tool;
}

export interface Usage {
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  audioSeconds: number;
  characters: number;
  reportedCost: number | null;
}

export const emptyUsage = (): Usage => ({ promptTokens: 0, completionTokens: 0, cachedTokens: 0, audioSeconds: 0, characters: 0, reportedCost: null });

export interface ChatChunk {
  text?: string;
  toolCalls?: FunctionCall[];
  usage?: Usage;
  finishReason?: string;
}

/** A provider call failed. The message must never contain a credential. */
export class APIError extends Error {
  readonly status: number | undefined;
  readonly retryable: boolean;

  constructor(message: string, options: { status?: number; retryable?: boolean } = {}) {
    super(message);
    this.status = options.status;
    this.retryable = options.retryable ?? true;
  }
}

export function errorForStatus(provider: string, status: number, body = ''): APIError {
  const retryable = status === 408 || status === 409 || status === 429 || status >= 500;
  return new APIError(`${provider} returned ${status}: ${body.slice(0, 300).replaceAll('\n', ' ')}`, { status, retryable });
}

export interface ChatOptions {
  tools?: FunctionTool[];
  toolChoice?: string;
  temperature?: number;
  maxTokens?: number;
  signal?: AbortSignal;
}

export abstract class LLM {
  provider = 'unknown';
  readonly model: string;
  maxRetries = 2;
  retryIntervalMs = 500;

  constructor(model: string) {
    this.model = model;
  }

  get label(): string {
    return `${this.provider}/${this.model}`;
  }

  async *chat(ctx: ChatContext, options: ChatOptions = {}): AsyncGenerator<ChatChunk> {
    let attempt = 0;
    while (true) {
      let sentOutput = false;
      try {
        for await (const chunk of this.stream(ctx, options)) {
          sentOutput ||= Boolean(chunk.text || chunk.toolCalls?.length);
          yield chunk;
        }
        return;
      } catch (e) {
        // Once words have gone out, a retry would make the agent say them twice.
        if (!(e instanceof APIError) || sentOutput || !e.retryable || attempt >= this.maxRetries) throw e;
        attempt += 1;
        await new Promise((r) => setTimeout(r, this.retryIntervalMs * attempt));
      }
    }
  }

  protected abstract stream(ctx: ChatContext, options: ChatOptions): AsyncGenerator<ChatChunk>;
}

/** Reads Server-Sent Events, yielding the event name and data. */
export async function* sseEvents(body: ReadableStream<Uint8Array>): AsyncGenerator<{ event: string | null; data: string }> {
  const decoder = new TextDecoder();
  let buffer = '';
  let event: string | null = null;
  let data: string[] = [];
  for await (const chunk of body as unknown as AsyncIterable<Uint8Array>) {
    buffer += decoder.decode(chunk, { stream: true });
    let newline: number;
    while ((newline = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, newline).replace(/\r$/, '');
      buffer = buffer.slice(newline + 1);
      if (line === '') {
        if (data.length > 0) yield { event, data: data.join('\n') };
        event = null;
        data = [];
      } else if (line.startsWith('event:')) {
        event = line.slice(6).trim();
      } else if (line.startsWith('data:')) {
        data.push(line.slice(5).replace(/^ /, ''));
      }
    }
  }
  if (data.length > 0) yield { event, data: data.join('\n') };
}
