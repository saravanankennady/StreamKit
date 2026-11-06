// LLM and speech adapters against local fixtures replaying each provider's documented wire format.
// PROTOCOL ONLY: these say nothing about the real services. See docs/VOICE_AI_PROVIDER_MATRIX.md.
import assert from 'node:assert/strict';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { describe, it } from 'node:test';
import { AnthropicLLM, APIError, AudioFrame, CartesiaTTS, type ChatChunk, ChatContext, ControlPlaneCredentials, DeepgramTTS, ElevenLabsTTS, CredentialError, Credentials, EnvironmentCredentials, functionTool, GoogleLLM, ModelChoice, ModelSelector, OpenAICompatibleLLM, OpenAICompatibleSTT, OpenAICompatibleTTS, SentenceTokenizer } from '../src/index.ts';

const KEY = 'sk-fixture-secret-key';

interface Seen {
  method: string;
  url: string;
  headers: IncomingMessage['headers'];
  body: Buffer;
}

async function fixture(handler: (seen: Seen, res: ServerResponse) => void, run: (url: string, requests: Seen[]) => Promise<void>): Promise<void> {
  const requests: Seen[] = [];
  const server: Server = createServer((req, res) => {
    const parts: Buffer[] = [];
    req.on('data', (d) => parts.push(d));
    req.on('end', () => {
      const seen = { method: req.method ?? '', url: req.url ?? '', headers: req.headers, body: Buffer.concat(parts) };
      requests.push(seen);
      handler(seen, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address() as { port: number };
  try {
    await run(`http://127.0.0.1:${address.port}`, requests);
  } finally {
    server.closeAllConnections();
    server.close();
  }
}

function sse(res: ServerResponse, events: unknown[]): void {
  for (const e of events) res.write(`data: ${typeof e === 'string' ? e : JSON.stringify(e)}\n\n`);
  res.end();
}

function namedSse(res: ServerResponse, events: Array<{ type: string }>): void {
  for (const e of events) res.write(`event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`);
  res.end();
}

async function collect(stream: AsyncIterable<ChatChunk>): Promise<ChatChunk[]> {
  const out: ChatChunk[] = [];
  for await (const c of stream) out.push(c);
  return out;
}

function context(): ChatContext {
  const ctx = new ChatContext();
  ctx.addMessage('system', 'Be brief.');
  ctx.addMessage('user', 'Weather in Paris?');
  ctx.add({ type: 'function_call', id: 'a', callId: 'c1', name: 'get_weather', arguments: '{"city":"Paris"}' });
  ctx.add({ type: 'function_call', id: 'b', callId: 'c2', name: 'get_weather', arguments: '{"city":"Rome"}' });
  ctx.add({ type: 'function_call_output', id: 'c', callId: 'c1', name: 'get_weather', output: 'sunny', isError: false });
  ctx.add({ type: 'function_call_output', id: 'd', callId: 'c2', name: 'get_weather', output: 'rain', isError: false });
  ctx.addMessage('assistant', 'Sunny in Paris.');
  ctx.addMessage('user', 'Thanks.');
  return ctx;
}

const weather = functionTool({
  name: 'get_weather',
  description: 'Gets the weather.',
  parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] },
  execute: () => 'fine',
});

describe('protocol fixtures', () => {
  it('OpenAI chat: streaming, tool call fragments and usage', async () => {
    await fixture(
      (_seen, res) =>
        sse(res, [
          { choices: [{ index: 0, delta: { role: 'assistant', content: '' } }] },
          { choices: [{ index: 0, delta: { content: 'Hel' } }] },
          { choices: [{ index: 0, delta: { content: 'lo' } }] },
          { choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: 'call_a', function: { name: 'get_', arguments: '' } }] } }] },
          { choices: [{ index: 0, delta: { tool_calls: [{ index: 0, function: { name: 'weather', arguments: '{"ci' } }] } }] },
          { choices: [{ index: 0, delta: { tool_calls: [{ index: 0, function: { arguments: 'ty":"Paris"}' } }] } }] },
          { choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] },
          { choices: [], usage: { prompt_tokens: 11, completion_tokens: 7, prompt_tokens_details: { cached_tokens: 3 }, cost: 0.5 } },
          '[DONE]',
        ]),
      async (url, requests) => {
        const llm = new OpenAICompatibleLLM('gpt-x', { baseUrl: `${url}/v1`, apiKey: KEY, provider: 'openai' });
        const chunks = await collect(llm.chat(context(), { tools: [weather], toolChoice: 'auto' }));
        const request = requests[0];
        assert.equal(request.url, '/v1/chat/completions');
        assert.equal(request.headers.authorization, `Bearer ${KEY}`);
        const body = JSON.parse(request.body.toString());
        assert.equal(body.stream, true);
        assert.deepEqual(body.stream_options, { include_usage: true });
        assert.equal(body.tools[0].function.name, 'get_weather');
        assert.deepEqual(body.messages.map((m: { role: string }) => m.role), ['system', 'user', 'assistant', 'tool', 'tool', 'assistant', 'user']);
        assert.equal(body.messages[2].tool_calls.length, 2, 'parallel calls belong to one message');
        assert.equal(chunks.map((c) => c.text ?? '').join(''), 'Hello');
        const calls = chunks.flatMap((c) => c.toolCalls ?? []);
        assert.deepEqual(calls.map((c) => [c.callId, c.name, c.arguments]), [['call_a', 'get_weather', '{"city":"Paris"}']]);
        const usage = chunks.find((c) => c.usage)!.usage!;
        assert.deepEqual([usage.promptTokens, usage.completionTokens, usage.cachedTokens, usage.reportedCost], [11, 7, 3, 0.5]);
      },
    );
  });

  it('retries before output, never after, and keeps keys out of errors', async () => {
    let attempts = 0;
    await fixture(
      (_seen, res) => {
        attempts += 1;
        if (attempts === 1) {
          res.statusCode = 503;
          res.end('busy');
        } else {
          sse(res, [{ choices: [{ delta: { content: 'ok' } }] }, '[DONE]']);
        }
      },
      async (url) => {
        const llm = new OpenAICompatibleLLM('m', { baseUrl: url, apiKey: KEY });
        llm.retryIntervalMs = 1;
        const chunks = await collect(llm.chat(context()));
        assert.equal(chunks.map((c) => c.text ?? '').join(''), 'ok');
        assert.equal(attempts, 2);
      },
    );
    attempts = 0;
    await fixture(
      (_seen, res) => {
        attempts += 1;
        res.statusCode = 401;
        res.end('{"error":"bad key"}');
      },
      async (url) => {
        const llm = new OpenAICompatibleLLM('m', { baseUrl: url, apiKey: KEY });
        await assert.rejects(collect(llm.chat(context())), (e: APIError) => e.status === 401 && !e.message.includes(KEY));
        assert.equal(attempts, 1);
      },
    );
    attempts = 0;
    await fixture(
      (_seen, res) => {
        attempts += 1;
        sse(res, [{ choices: [{ delta: { content: 'half' } }] }, { error: { message: 'boom' } }]);
      },
      async (url) => {
        const llm = new OpenAICompatibleLLM('m', { baseUrl: url, apiKey: KEY });
        llm.retryIntervalMs = 1;
        const got: string[] = [];
        await assert.rejects(async () => {
          for await (const c of llm.chat(context())) if (c.text) got.push(c.text);
        });
        assert.equal(attempts, 1);
        assert.deepEqual(got, ['half']);
      },
    );
  });

  it('Anthropic messages', async () => {
    await fixture(
      (_seen, res) =>
        namedSse(res, [
          { type: 'message_start', message: { usage: { input_tokens: 25, output_tokens: 1, cache_read_input_tokens: 4 } } },
          { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } },
          { type: 'ping' },
          { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'Hello' } },
          { type: 'content_block_stop', index: 0 },
          { type: 'content_block_start', index: 1, content_block: { type: 'tool_use', id: 'toolu_1', name: 'get_weather', input: {} } },
          { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: '{"city": "Pa' } },
          { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: 'ris"}' } },
          { type: 'content_block_stop', index: 1 },
          { type: 'message_delta', delta: { stop_reason: 'tool_use' }, usage: { output_tokens: 15 } },
          { type: 'message_stop' },
        ] as Array<{ type: string }>),
      async (url, requests) => {
        const llm = new AnthropicLLM('claude-x', { apiKey: KEY, baseUrl: url });
        const chunks = await collect(llm.chat(context(), { tools: [weather], toolChoice: 'required' }));
        const request = requests[0];
        assert.equal(request.url, '/v1/messages');
        assert.equal(request.headers['x-api-key'], KEY);
        assert.equal(request.headers['anthropic-version'], '2026-10-21');
        const body = JSON.parse(request.body.toString());
        assert.equal(body.system, 'Be brief.');
        assert.deepEqual(body.tool_choice, { type: 'any' });
        assert.deepEqual(body.messages.map((m: { role: string }) => m.role), ['user', 'assistant', 'user', 'assistant', 'user']);
        assert.equal(chunks.map((c) => c.text ?? '').join(''), 'Hello');
        const calls = chunks.flatMap((c) => c.toolCalls ?? []);
        assert.deepEqual(JSON.parse(calls[0].arguments), { city: 'Paris' });
        const usage = chunks.find((c) => c.usage)!.usage!;
        assert.deepEqual([usage.promptTokens, usage.completionTokens, usage.cachedTokens], [25, 15, 4]);
      },
    );
  });

  it('Google streamGenerateContent', async () => {
    await fixture(
      (_seen, res) =>
        sse(res, [
          { candidates: [{ content: { role: 'model', parts: [{ text: 'Hel' }] } }] },
          {
            candidates: [{ content: { parts: [{ text: 'lo' }, { functionCall: { id: 'fc1', name: 'get_weather', args: { city: 'Paris' } } }] }, finishReason: 'STOP' }],
            usageMetadata: { promptTokenCount: 9, candidatesTokenCount: 4 },
          },
        ]),
      async (url, requests) => {
        const llm = new GoogleLLM('gemini-x', { apiKey: KEY, baseUrl: url });
        const chunks = await collect(llm.chat(context(), { tools: [weather] }));
        const request = requests[0];
        assert.equal(request.url, '/v1beta/models/gemini-x:streamGenerateContent?alt=sse');
        assert.equal(request.headers['x-goog-api-key'], KEY);
        const body = JSON.parse(request.body.toString());
        assert.deepEqual(body.systemInstruction, { parts: [{ text: 'Be brief.' }] });
        assert.deepEqual(body.contents.map((c: { role: string }) => c.role), ['user', 'model', 'user', 'model', 'user']);
        assert.equal(body.tools[0].functionDeclarations[0].name, 'get_weather');
        assert.equal(chunks.map((c) => c.text ?? '').join(''), 'Hello');
        assert.equal(chunks.flatMap((c) => c.toolCalls ?? [])[0].callId, 'fc1');
        assert.equal(chunks.find((c) => c.usage)!.usage!.promptTokens, 9);
      },
    );
  });

  it('OpenAI-shaped transcription and speech', async () => {
    const pcm = Buffer.alloc(9600);
    for (let i = 0; i < pcm.length / 2; i++) pcm.writeInt16LE(i % 1000, i * 2);
    await fixture(
      (seen, res) => {
        if (seen.url.endsWith('/audio/transcriptions')) {
          res.setHeader('content-type', 'application/json');
          res.end(JSON.stringify({ text: '  hello there ' }));
        } else {
          // Network chunks fall anywhere, including inside a sample.
          res.write(pcm.subarray(0, 1001));
          res.end(pcm.subarray(1001));
        }
      },
      async (url, requests) => {
        const stt = new OpenAICompatibleSTT('gpt-transcribe', { baseUrl: `${url}/v1`, apiKey: KEY, language: 'en', provider: 'openai' });
        const event = await stt.recognize(new AudioFrame(new Int16Array(16000), 16000));
        assert.equal(event.text, 'hello there');
        assert.equal(event.usage?.audioSeconds, 1);
        assert.match(String(requests[0].headers['content-type']), /multipart\/form-data/);
        assert.ok(requests[0].body.includes('RIFF') && requests[0].body.includes('gpt-transcribe'));

        const tts = new OpenAICompatibleTTS('gpt-4o-mini-tts', { baseUrl: `${url}/v1`, apiKey: KEY, voice: 'marin', provider: 'openai' });
        const frames: AudioFrame[] = [];
        for await (const f of tts.synthesize('Hello')) frames.push(f);
        assert.deepEqual(JSON.parse(requests[1].body.toString()), { model: 'gpt-4o-mini-tts', input: 'Hello', voice: 'marin', response_format: 'pcm' });
        assert.ok(AudioFrame.concat(frames).toBuffer().equals(pcm));
        assert.equal(tts.lastUsage?.characters, 5);
      },
    );
  });

  it('Cartesia, ElevenLabs and Deepgram speech', async () => {
    const pcm = Buffer.alloc(4800, 7);
    const wavHeader = Buffer.concat([Buffer.from('RIFF\0\0\0\0WAVEfmt \x10\0\0\0', 'latin1'), Buffer.alloc(16), Buffer.from('data\0\0\0\0', 'latin1')]);
    await fixture(
      (seen, res) => {
        if (seen.url.startsWith('/v1/speak')) res.write(wavHeader.subarray(0, 9));
        if (seen.url.startsWith('/v1/speak')) res.write(wavHeader.subarray(9));
        res.end(pcm);
      },
      async (url, requests) => {
        const play = async (tts: { synthesize: (t: string) => AsyncGenerator<AudioFrame> }) => {
          const frames: AudioFrame[] = [];
          for await (const f of tts.synthesize('Hello')) frames.push(f);
          assert.ok(AudioFrame.concat(frames).toBuffer().equals(pcm));
        };
        await play(new CartesiaTTS('sonic-3.6', { voice: 'v', apiKey: KEY, baseUrl: url }));
        assert.equal(requests[0].url, '/tts/bytes');
        assert.equal(requests[0].headers.authorization, `Bearer ${KEY}`);
        assert.equal(requests[0].headers['cartesia-version'], '2026-08-14');
        const cartesia = JSON.parse(requests[0].body.toString());
        assert.deepEqual(cartesia.output_format, { container: 'raw', encoding: 'pcm_s16le', sample_rate: 24000 });
        assert.deepEqual(cartesia.voice, { id: 'v' });

        await play(new ElevenLabsTTS('eleven_multilingual_v2', { voice: 'abc', apiKey: KEY, baseUrl: url }));
        assert.equal(requests[1].url, '/v1/text-to-speech/abc/stream?output_format=pcm_24000');
        assert.equal(requests[1].headers['xi-api-key'], KEY);

        await play(new DeepgramTTS('aura-2-thalia-en', { apiKey: KEY, baseUrl: url }));
        assert.equal(requests[2].url, '/v1/speak?model=aura-2-thalia-en&encoding=linear16&sample_rate=24000');
        assert.equal(requests[2].headers.authorization, `Token ${KEY}`);
      },
    );
  });

  it('credentials from the control plane', async () => {
    await fixture(
      (seen, res) => {
        const provider = JSON.parse(seen.body.toString()).provider;
        res.statusCode = provider === 'missing' ? 404 : provider === 'refused' ? 403 : 200;
        res.end(JSON.stringify(provider === 'deepgram' ? { secret: KEY } : {}));
      },
      async (url, requests) => {
        const source = new ControlPlaneCredentials(url, 'worker-token');
        const credentials = new Credentials([new EnvironmentCredentials(), source]);
        assert.equal(await credentials.require('deepgram'), KEY);
        assert.equal(await source.get('deepgram'), KEY);
        assert.equal(requests.length, 1, 'the key is cached, not fetched per call');
        assert.equal(requests[0].url, '/api/v1/agent-credentials/resolve');
        assert.equal(requests[0].headers.authorization, 'Bearer worker-token');
        await assert.rejects(credentials.require('missing'), CredentialError);
        await assert.rejects(source.get('refused'), (e: Error) => e instanceof CredentialError && !e.message.includes('worker-token'));
      },
    );
  });

  it('choosing models by name', async () => {
    process.env.ANTHROPIC_API_KEY = KEY;
    process.env.OPENAI_API_KEY = KEY;
    try {
      const selector = new ModelSelector();
      assert.equal((await selector.llm('anthropic/claude-haiku-5-5')).label, 'anthropic/claude-haiku-5-5');
      assert.equal((await selector.tts('openai/gpt-4o-mini-tts:marin')).voice, 'marin');
      assert.equal((await selector.stt('openai/gpt-transcribe')).label, 'openai/gpt-transcribe');
      await assert.rejects(selector.llm('nonsense/model'));
      await assert.rejects(selector.llm('no-slash'));
      delete process.env.ANTHROPIC_API_KEY;
      await assert.rejects(selector.llm('anthropic/x'), CredentialError);
    } finally {
      delete process.env.ANTHROPIC_API_KEY;
      delete process.env.OPENAI_API_KEY;
    }
    assert.deepEqual({ ...ModelChoice.fromJobMetadata('not json') }, { stt: undefined, llm: undefined, tts: undefined });
    const chosen = ModelChoice.fromJobMetadata('{"models": {"llm": "anthropic/x"}}');
    assert.equal(chosen.llm, 'anthropic/x');
    assert.equal(new ModelChoice({ llm: 'a/b', stt: 'c/d' }).overlay(chosen).stt, 'c/d');
  });

  it('sentence tokenizer matches the Python one', () => {
    const t = new SentenceTokenizer();
    assert.deepEqual(t.push('Hello there, this is a test. Another '), ['Hello there, this is a test.']);
    assert.deepEqual(t.push('one is coming! Short. '), ['Another one is coming!']);
    assert.deepEqual(t.push('Dr. Smith paid 3.5 dollars today. '), ['Short. Dr. Smith paid 3.5 dollars today.']);
    assert.equal(t.flush(), '');
    const pieces = new SentenceTokenizer({ maxChars: 100 }).push('word '.repeat(80));
    assert.ok(pieces.length > 0 && pieces.every((p) => p.length <= 100));
  });
});
