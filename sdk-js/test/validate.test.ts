import { describe, expect, it, vi } from 'vitest';
import { ConnectionErrorReason } from '../src/errors.js';
import { errorFromValidateResponse, explainFailedUpgrade } from '../src/signal/validate.js';

describe('errorFromValidateResponse', () => {
  it('a missing room is a refusal', () => {
    const e = errorFromValidateResponse(404, 'requested room does not exist');
    expect(e.reason).toBe(ConnectionErrorReason.NotAllowed);
    expect(e.status).toBe(404);
  });
  it('any other 404 means this is not a signalling server', () => {
    const e = errorFromValidateResponse(404, 'page not found');
    expect(e.reason).toBe(ConnectionErrorReason.ServiceNotFound);
  });
  it('401 and 403 carry the message from the server', () => {
    for (const status of [401, 403]) {
      const e = errorFromValidateResponse(status, 'invalid token\n');
      expect(e.reason).toBe(ConnectionErrorReason.NotAllowed);
      expect(e.message).toBe('invalid token');
      expect(e.status).toBe(status);
    }
  });
  it('a 200 means the upgrade itself failed', () => {
    const e = errorFromValidateResponse(200, 'success');
    expect(e.reason).toBe(ConnectionErrorReason.InternalError);
    expect(e.message).toMatch(/WebSocket failed/);
  });
  it('anything else is internal, with the status', () => {
    const e = errorFromValidateResponse(500, 'boom');
    expect(e.reason).toBe(ConnectionErrorReason.InternalError);
    expect(e.status).toBe(500);
    expect(e.message).toContain('boom');
  });
});

describe('explainFailedUpgrade', () => {
  it('reports an unreachable server when the request itself fails', async () => {
    const e = await explainFailedUpgrade('http://x', vi.fn().mockRejectedValue(new TypeError('failed to fetch')));
    expect(e.reason).toBe(ConnectionErrorReason.ServerUnreachable);
    expect(e.message).toContain('failed to fetch');
  });
  it('gives up after its deadline', async () => {
    vi.useFakeTimers();
    const hanging = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_, reject) => {
      init.signal!.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
    }));
    const result = explainFailedUpgrade('http://x', hanging as unknown as typeof fetch);
    await vi.advanceTimersByTimeAsync(3000);
    expect((await result).reason).toBe(ConnectionErrorReason.ServerUnreachable);
    vi.useRealTimers();
  });
  it('classifies what comes back', async () => {
    const e = await explainFailedUpgrade('http://x', vi.fn().mockResolvedValue(new Response('nope', { status: 401 })));
    expect(e.reason).toBe(ConnectionErrorReason.NotAllowed);
  });
});
