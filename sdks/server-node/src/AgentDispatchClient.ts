import { create, fromJson, toJson, type JsonValue } from '@bufbuild/protobuf';
import { Transport, type ClientOptions } from './http.js';
import {
  AgentDispatchSchema,
  CreateAgentDispatchRequestSchema,
  DeleteAgentDispatchRequestSchema,
  ListAgentDispatchRequestSchema,
  ListAgentDispatchResponseSchema,
  type AgentDispatch,
} from './proto/livekit_agent_dispatch_pb.js';

const SERVICE = 'AgentDispatchService';
const parse = { ignoreUnknownFields: true };

/**
 * Asks for agents to be sent to rooms, over the same Twirp service the reference server has. This server does not have
 * that service yet, so each call throws NotSupportedError here.
 */
export class AgentDispatchClient {
  private readonly transport: Transport;

  constructor(url: string, apiKey?: string, apiSecret?: string, options?: ClientOptions) {
    this.transport = new Transport(url, apiKey, apiSecret, options);
  }

  private call(method: string, room: string, body: JsonValue): Promise<JsonValue> {
    return this.transport.twirp(SERVICE, method, { roomAdmin: true, room }, body) as Promise<JsonValue>;
  }

  /** Sends an agent to a room. */
  async createDispatch(room: string, agentName: string, options: { metadata?: string } = {}): Promise<AgentDispatch> {
    const body = toJson(CreateAgentDispatchRequestSchema, create(CreateAgentDispatchRequestSchema, { room, agentName, metadata: options.metadata }));
    return fromJson(AgentDispatchSchema, await this.call('CreateDispatch', room, body), parse);
  }

  /** Cancels a dispatch. */
  async deleteDispatch(dispatchId: string, room: string): Promise<void> {
    const body = toJson(DeleteAgentDispatchRequestSchema, create(DeleteAgentDispatchRequestSchema, { dispatchId, room }));
    await this.call('DeleteDispatch', room, body);
  }

  /** Lists a room's dispatches. */
  async listDispatch(room: string): Promise<AgentDispatch[]> {
    const body = toJson(ListAgentDispatchRequestSchema, create(ListAgentDispatchRequestSchema, { room }));
    return fromJson(ListAgentDispatchResponseSchema, await this.call('ListDispatch', room, body), parse).agentDispatches;
  }
}
