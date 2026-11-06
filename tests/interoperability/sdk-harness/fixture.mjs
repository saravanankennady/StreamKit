// Serves the harness pages and mints access tokens for them. Unlike the oracle's fixture, tokens here can
// carry custom grants and lifetimes, because several tests are about exactly that.
import { createHmac } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { dirname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const b64 = (v) => Buffer.from(v).toString('base64url');

export function mintToken({ apiKey, apiSecret, identity, room, grants = {}, ttl = 3600, name }) {
  const now = Math.floor(Date.now() / 1000);
  const head = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64(
    JSON.stringify({
      iss: apiKey,
      sub: identity,
      nbf: now - 5,
      exp: now + ttl,
      name: name ?? identity,
      video: { roomJoin: true, room, canPublish: true, canSubscribe: true, canPublishData: true, canUpdateOwnMetadata: true, ...grants },
    }),
  );
  const sig = createHmac('sha256', apiSecret).update(`${head}.${body}`).digest('base64url');
  return `${head}.${body}.${sig}`;
}

const pages = {
  '/streamkit.html': 'page.html?script=/dist/client.js',
  '/official.html': 'page.html?script=/dist/official.js',
  '/react.html': 'page.html?script=/dist/react-client.js',
};
const types = { '.html': 'text/html', '.js': 'text/javascript', '.map': 'application/json' };

export function startFixtureServer({ apiKey = 'devkey', apiSecret = 'secret' } = {}) {
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    if (url.pathname === '/token') {
      const q = url.searchParams;
      res.end(
        mintToken({
          apiKey,
          apiSecret,
          identity: q.get('identity'),
          room: q.get('room'),
          grants: q.get('grants') ? JSON.parse(q.get('grants')) : {},
          ttl: q.get('ttl') ? Number(q.get('ttl')) : 3600,
        }),
      );
      return;
    }
    if (pages[url.pathname]) {
      const script = pages[url.pathname].split('script=')[1];
      res.setHeader('content-type', 'text/html');
      res.end(`<!doctype html>
<meta charset="utf-8" />
<title>SDK test client</title>
<div id="state" data-state="loading"></div>
<div id="media"></div>
<div id="root"></div>
<script type="module" src="${script}"></script>`);
      return;
    }
    const rel = normalize(url.pathname).replace(/^[/\\.]+/, '');
    try {
      const data = await readFile(join(root, rel));
      res.setHeader('content-type', types[rel.slice(rel.lastIndexOf('.'))] ?? 'application/octet-stream');
      res.end(data);
    } catch {
      res.statusCode = 404;
      res.end('not found');
    }
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port })));
}
