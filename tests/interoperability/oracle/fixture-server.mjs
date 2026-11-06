// Serves the test page and mints HS256 access tokens for the interop test client.
import { createHmac } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { dirname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const b64 = (v) => Buffer.from(v).toString('base64url');

export function mintToken({ apiKey, apiSecret, identity, room, grants = {} }) {
  const now = Math.floor(Date.now() / 1000);
  const head = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64(
    JSON.stringify({
      iss: apiKey,
      sub: identity,
      nbf: now - 5,
      exp: now + 3600,
      name: identity,
      video: { roomJoin: true, room, canPublish: true, canSubscribe: true, canPublishData: true, ...grants },
    }),
  );
  const sig = createHmac('sha256', apiSecret).update(`${head}.${body}`).digest('base64url');
  return `${head}.${body}.${sig}`;
}

export function startFixtureServer({ port = 0, apiKey = 'devkey', apiSecret = 'secret' } = {}) {
  const types = { '.html': 'text/html', '.js': 'text/javascript', '.map': 'application/json' };
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    if (url.pathname === '/token') {
      res.end(mintToken({ apiKey, apiSecret, identity: url.searchParams.get('identity'), room: url.searchParams.get('room') }));
      return;
    }
    const rel = url.pathname === '/' ? 'index.html' : normalize(url.pathname).replace(/^[/\\.]+/, '');
    try {
      const data = await readFile(join(root, rel));
      res.setHeader('content-type', types[rel.slice(rel.lastIndexOf('.'))] ?? 'application/octet-stream');
      res.end(data);
    } catch {
      res.statusCode = 404;
      res.end('not found');
    }
  });
  return new Promise((resolve) => server.listen(port, '127.0.0.1', () => resolve({ server, port: server.address().port })));
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const { port } = await startFixtureServer({ port: Number(process.env.PORT ?? 7900) });
  console.log(`fixture server on http://127.0.0.1:${port}`);
}
