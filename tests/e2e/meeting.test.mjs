// End-to-end tests of share links and the meeting page: real Chrome with fake capture devices, the real server, a real
// database and real media. Nothing is mocked. Video assertions read decoded frames and changing pixels, and sound
// assertions measure the level coming out of the received track.
//
//   cd tests/e2e && npm install && node --test --test-timeout=600000 meeting.test.mjs
//
// Tests share one account and run in order, like someone working through the product. See lib/stack.mjs.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import dgram from 'node:dgram';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join as joinPath } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { promisify } from 'node:util';
import { createKey, eventually, horizontalOverflow, launch, pageApi, register, sleep, useNewProject, verifyJwt } from './lib/helpers.mjs';
import { BACKEND, DASH, shotsDir, sql, startStack, stopStack, tmpDir, WS_URL, workersRunning } from './lib/stack.mjs';

const ROOM = 'standup';
const SCREEN_ARGS = ['--auto-select-desktop-capture-source=Entire screen', '--enable-usermedia-screen-capturing'];
// Reaches this machine under a name browsers do not treat as secure, the way another computer on a network would.
const INSECURE_ARGS = ['--host-resolver-rules=MAP meet.test 127.0.0.1'];
// Several fake devices, so choosing one means something. Of three cameras, Chrome's second is in a format it can show and
// cannot encode, so it produces no frames to send. It is left out of the choices below.
const MANY_DEVICES_ARGS = ['--use-fake-device-for-media-stream=device-count=3', ...SCREEN_ARGS];
const SENDABLE_CAMERAS = (cameras) => [cameras[0], cameras[2]];
// Where the test server takes media. See streamkit-dash.yaml.
const RTC_UDP_PORT = 17882;

const run = promisify(execFile);

/**
 * A bad network, for real: a UDP relay on one browser's way to the server, which drops a share of the media going up.
 * What is lost is lost, so whatever the server scores comes from packets that never arrived. It runs over loopback, and
 * says nothing about any real network. It leaves connectivity checks and encryption setup alone, which lets the
 * connection come up, as it would on a link that only loses some of what it carries.
 */
async function startLossyLink(lossRate) {
  const front = dgram.createSocket('udp4');
  const backs = new Map();
  front.on('message', (packet, from) => {
    const key = `${from.address}:${from.port}`;
    let back = backs.get(key);
    if (!back) {
      back = dgram.createSocket('udp4');
      back.on('message', (reply) => front.send(reply, from.port, from.address));
      backs.set(key, back);
    }
    // RTP has version 2 in its first byte, and RTCP sits in this range of its second.
    const isMedia = (packet[0] & 0xc0) === 0x80 && !(packet[1] >= 192 && packet[1] <= 223);
    if (isMedia && Math.random() < lossRate) return;
    back.send(packet, RTC_UDP_PORT, '127.0.0.1');
  });
  await new Promise((resolve) => front.bind(0, '127.0.0.1', resolve));
  return {
    port: front.address().port,
    close() {
      for (const back of backs.values()) back.close();
      front.close();
    },
  };
}

/**
 * Runs in a page, to make a relay the only way its media can go. The server's address in what it offers is swapped for
 * the relay's. The server does not get to learn this browser's address either, because it would then open a direct path
 * around the relay, and ICE would take it.
 */
function routeMediaThrough(port) {
  const P = RTCPeerConnection.prototype;
  const swap = (line) => line.replace(/^(a=)?(candidate:\S+ \d+ udp \d+) \S+ \d+ typ/i, `$1$2 127.0.0.1 ${port} typ`);
  const setRemote = P.setRemoteDescription;
  P.setRemoteDescription = function (d) {
    if (!d?.sdp) return setRemote.call(this, d);
    const lines = d.sdp.split('\r\n').filter((l) => !/^a=candidate:\S+ \d+ tcp /i.test(l));
    return setRemote.call(this, { type: d.type, sdp: lines.map(swap).join('\r\n') });
  };
  const add = P.addIceCandidate;
  P.addIceCandidate = function (c) {
    if (!c?.candidate) return add.call(this, c);
    if (/ tcp /i.test(c.candidate)) return Promise.resolve();
    return add.call(this, { candidate: swap(c.candidate), sdpMid: c.sdpMid, sdpMLineIndex: c.sdpMLineIndex, usernameFragment: c.usernameFragment });
  };
  const strip = (d) => ({ type: d.type, sdp: d.sdp.split('\r\n').filter((l) => !/^a=candidate:/i.test(l)).join('\r\n') });
  for (const name of ['createOffer', 'createAnswer']) {
    const real = P[name];
    P[name] = function (...args) {
      return real.apply(this, args).then(strip);
    };
  }
  for (const name of ['localDescription', 'pendingLocalDescription']) {
    const real = Object.getOwnPropertyDescriptor(P, name);
    Object.defineProperty(P, name, {
      get() {
        const d = real.get.call(this);
        return d && strip(d);
      },
    });
  }
  const trickle = Object.getOwnPropertyDescriptor(P, 'onicecandidate');
  Object.defineProperty(P, 'onicecandidate', {
    get: trickle.get,
    set(fn) {
      trickle.set.call(this, fn && ((e) => (e.candidate ? undefined : fn(e))));
    },
  });
}

let browser;
let insecureBrowser;
let owner;
let project;
let key;
let linkUrl;
const contexts = [];
const everyGuest = [];

async function openGuest(url, { from = browser, contextOptions } = {}) {
  const context = await from.newContext(contextOptions);
  contexts.push(context);
  const page = await context.newPage();
  const guest = { page, context, urls: [], joinBodies: [], errors: [] };
  everyGuest.push(guest);
  page.on('request', (r) => guest.urls.push(r.url()));
  page.on('pageerror', (e) => guest.errors.push(e.message));
  page.on('response', async (r) => {
    if (r.url().includes('/api/v1/join')) guest.joinBodies.push(await r.text().catch(() => ''));
  });
  await page.goto(url);
  return guest;
}

async function join(guest, name) {
  await guest.page.getByTestId('display-name').fill(name);
  await guest.page.getByTestId('join-button').click();
  await guest.page.locator('[data-testid="meeting-room"][data-state="connected"]').waitFor({ timeout: 30000 });
}

function tile(page, name) {
  const label = new RegExp(`^${name}( \\(you\\))?$`);
  return page.getByTestId('tile').filter({ has: page.getByTestId('tile-name').filter({ hasText: label }) });
}

/** A video element shows moving pictures: frames decode, and two looks a second apart differ. */
async function assertPlaying(video, what) {
  const look = () =>
    video.evaluate((v) => {
      const canvas = document.createElement('canvas');
      canvas.width = 64;
      canvas.height = 36;
      const g = canvas.getContext('2d');
      g.drawImage(v, 0, 0, 64, 36);
      const data = g.getImageData(0, 0, 64, 36).data;
      let hash = 0;
      for (const byte of data) hash = (hash * 31 + byte) | 0;
      return { width: v.videoWidth, frames: v.getVideoPlaybackQuality().totalVideoFrames, hash };
    });
  await eventually(
    what,
    async () => {
      const first = await look();
      await new Promise((r) => setTimeout(r, 1000));
      const second = await look();
      return first.width > 0 && second.frames > first.frames && second.hash !== first.hash;
    },
    { timeout: 30000, interval: 250 },
  );
}

/** Measures what the page's received audio elements actually carry, over a few seconds. */
function receivedSoundLevel(page) {
  return page.evaluate(async () => {
    const elements = [...document.querySelectorAll('[data-streamkit-room-audio] audio')].filter((a) => a.srcObject);
    if (elements.length === 0) return 0;
    const ctx = new AudioContext();
    await ctx.resume();
    const analysers = elements.map((a) => {
      const an = ctx.createAnalyser();
      an.fftSize = 2048;
      ctx.createMediaStreamSource(a.srcObject).connect(an);
      return an;
    });
    const samples = new Float32Array(2048);
    let peak = 0;
    const end = performance.now() + 4000;
    while (performance.now() < end) {
      for (const an of analysers) {
        an.getFloatTimeDomainData(samples);
        for (const s of samples) peak = Math.max(peak, Math.abs(s));
      }
      await new Promise((r) => setTimeout(r, 40));
    }
    await ctx.close();
    return peak;
  });
}

const api = (method, path, body) => pageApi(owner, method, path, { project, body });

async function participants(room = ROOM) {
  const res = await api('GET', `/participants?room=${room}`);
  assert.equal(res.status, 200, res.text);
  return res.json.items.filter((p) => p.state !== 'DISCONNECTED');
}

async function newLink(body) {
  const res = await api('POST', '/join-links', body);
  assert.equal(res.status, 201, res.text);
  return res.json;
}

before(async () => {
  await startStack();
  mkdirSync(shotsDir, { recursive: true });
  browser = await launch(SCREEN_ARGS);
  insecureBrowser = await launch(INSECURE_ARGS);
  const context = await browser.newContext();
  contexts.push(context);
  owner = await context.newPage();
  await register(owner, { email: `meeting-${Date.now()}@example.com` });
  project = (await useNewProject(owner, 'Meetings')).id;
  key = await createKey(owner, project);
});

after(async () => {
  for (const c of contexts) await c.close().catch(() => {});
  await browser?.close();
  await insecureBrowser?.close();
  await stopStack();
});

describe('share links in the dashboard', () => {
  it('makes a link for a room that is not open yet, and says when it only works on this computer', async () => {
    await owner.goto(`${DASH}/rooms`);
    await owner.getByTestId('share-new').click();
    await owner.getByTestId('share-room').fill(ROOM);
    await owner.getByTestId('share-label').fill('Daily standup');
    await owner.getByTestId('share-create').click();
    const field = owner.getByTestId('new-share-link');
    await field.waitFor();
    linkUrl = await field.inputValue();
    assert.ok(linkUrl.startsWith(`${DASH}/meet/join#`), linkUrl);
    assert.ok(linkUrl.split('#')[1].length >= 20, 'a code that cannot be guessed');
    await owner.getByText('This link only works on this computer').waitFor();
    await owner.screenshot({ path: `${shotsDir}/share-dialog.png` });
    await owner.getByRole('button', { name: 'Done' }).click();
  });

  it('offers a link from an empty room, and says the connection address is not for browsers', async () => {
    const created = await api('POST', '/rooms', { name: 'empty-room' });
    assert.ok(created.status < 300, created.text);
    await owner.goto(`${DASH}/rooms/empty-room`);
    await owner.getByText('It does not open in a browser').waitFor();
    for (const button of ['room-empty-share', 'share-links-empty-create']) {
      await owner.getByTestId(button).click();
      assert.equal(await owner.getByTestId('share-room').inputValue(), 'empty-room');
      await owner.getByRole('button', { name: 'Cancel' }).click();
      await owner.getByTestId('share-room').waitFor({ state: 'detached' });
    }
    const closed = await api('DELETE', '/rooms/empty-room');
    assert.ok(closed.status < 300, closed.text);
  });

  it('asks for an API key first, rather than handing out a link that cannot work', async () => {
    const other = await api('POST', '/projects', { name: 'no keys here', environment: 'development' });
    assert.equal(other.status, 201, other.text);
    const res = await pageApi(owner, 'POST', '/join-links', { project: other.json.id, body: { room: 'x' } });
    assert.equal(res.status, 409);
    assert.equal(res.json.error.code, 'no_api_key');
  });
});

describe('the meeting page', () => {
  let asha;
  let ben;

  it('shows the meeting and the guest\'s own camera and microphone before they join', async () => {
    asha = await openGuest(linkUrl);
    const { page } = asha;
    await page.getByTestId('prejoin').waitFor();
    assert.equal(await page.getByTestId('meeting-title').innerText(), 'Daily standup');
    await assertPlaying(page.getByTestId('preview-video'), 'the camera preview moves');
    await eventually('the microphone meter moves', async () => Number(await page.getByTestId('mic-level').getAttribute('data-level')) > 0.01);
    await eventually('devices are listed by name', async () => (await page.getByTestId('device-camera').locator('option').count()) > 0);
    await page.getByTestId('test-speaker').click();
    await eventually('the speaker test finishes', () => page.getByTestId('test-speaker').isEnabled());
    assert.equal(await page.getByTestId('prejoin').getByRole('alert').count(), 0, 'no device complaints');
    await page.screenshot({ path: `${shotsDir}/prejoin.png` });
  });

  it('lets two guests meet, with real video and sound in both directions', async () => {
    await join(asha, 'Asha');
    ben = await openGuest(linkUrl);
    await ben.page.getByTestId('prejoin').waitFor();
    await join(ben, 'Ben');

    await eventually('both see two people', async () => (await asha.page.getByTestId('people-count').innerText()) === '2' && (await ben.page.getByTestId('people-count').innerText()) === '2');
    await assertPlaying(tile(asha.page, 'Ben').locator('video'), "Asha sees Ben's video");
    await assertPlaying(tile(ben.page, 'Asha').locator('video'), "Ben sees Asha's video");
    await eventually('Asha hears Ben', async () => (await receivedSoundLevel(asha.page)) > 0.001, { timeout: 30000 });
    await eventually('Ben hears Asha', async () => (await receivedSoundLevel(ben.page)) > 0.001, { timeout: 30000 });
    await asha.page.screenshot({ path: `${shotsDir}/meeting.png` });
  });

  it('shows the server two guests, under the names they gave and identities it picked', async () => {
    const people = await participants();
    assert.equal(people.length, 2);
    assert.deepEqual(people.map((p) => p.name).sort(), ['Asha', 'Ben']);
    for (const p of people) {
      assert.match(p.identity, /^guest-/);
      assert.equal(p.tracks.length, 2, `${p.name} publishes camera and microphone`);
    }
    assert.notEqual(people[0].identity, people[1].identity);
  });

  it('carries muting and stopping the camera to the other person', async () => {
    const seen = tile(ben.page, 'Asha');
    await asha.page.getByTestId('control-mic').click();
    await eventually('Ben sees Asha muted', async () => (await seen.getAttribute('data-audio-muted')) === 'true');
    await asha.page.getByTestId('control-camera').click();
    await eventually('Ben sees the camera stop', async () => (await seen.locator('video').count()) === 0);

    await asha.page.getByTestId('control-mic').click();
    await asha.page.getByTestId('control-camera').click();
    await eventually('Ben sees Asha unmuted', async () => (await seen.getAttribute('data-audio-muted')) === 'false');
    await assertPlaying(seen.locator('video'), "Asha's video comes back");
  });

  it('shares a screen, and stops sharing', async () => {
    await asha.page.getByTestId('control-screen').click();
    const shared = ben.page.getByTestId('tile').filter({ has: ben.page.getByTestId('tile-name').filter({ hasText: "Asha's screen" }) });
    await shared.waitFor({ timeout: 30000 });
    await assertPlaying(shared.locator('video'), "Ben sees Asha's screen");
    await ben.page.screenshot({ path: `${shotsDir}/screen-share.png` });

    await asha.page.getByTestId('control-screen').click();
    await eventually('the shared screen goes away', async () => (await shared.count()) === 0);
    assert.equal(await asha.page.getByTestId('room-notice').count(), 0);
  });

  it('shows a summary on leaving, and the link gets you back in', async () => {
    await ben.page.getByTestId('control-leave').click();
    await ben.page.getByTestId('end-screen').waitFor();
    assert.match(await ben.page.getByTestId('end-screen').innerText(), /You left the meeting/);
    assert.equal(await ben.page.getByTestId('end-others').innerText(), '1');
    await eventually('Asha sees Ben go', async () => (await asha.page.getByTestId('people-count').innerText()) === '1');

    await ben.page.getByTestId('rejoin').click();
    await ben.page.getByTestId('prejoin').waitFor();
    assert.equal(await ben.page.getByTestId('display-name').inputValue(), 'Ben', 'the name is remembered');
    await join(ben, 'Ben');
    await eventually('Asha sees Ben return', async () => (await asha.page.getByTestId('people-count').innerText()) === '2');
    await assertPlaying(tile(asha.page, 'Ben').locator('video'), "Ben's video after rejoining");
  });

  it('keeps the code out of every URL a server sees, and every secret away from guests', async () => {
    const code = linkUrl.split('#')[1];
    for (const guest of [asha, ben]) {
      assert.ok(guest.urls.length > 0);
      for (const url of guest.urls) assert.ok(!new URL(url).pathname.includes(code) && !new URL(url).search.includes(code), `the code is in ${url}`);
      assert.ok(guest.joinBodies.length > 0);
      for (const body of guest.joinBodies) {
        assert.ok(!body.includes(key.secret), 'a guest received the API secret');
        assert.ok(!body.includes(project), 'a guest received the project ID');
      }
      assert.ok(!(await guest.page.content()).includes(key.secret));
    }
  });

  it('gives guests a token good for this room and nothing else', async () => {
    const body = ben.joinBodies.map((b) => JSON.parse(b)).find((b) => b.token);
    const claims = verifyJwt(body.token, key.secret);
    assert.ok(claims, 'signed with the project key');
    assert.equal(claims.video.room, ROOM);
    assert.equal(claims.video.roomJoin, true);
    for (const grant of ['roomAdmin', 'roomCreate', 'roomList', 'roomRecord', 'hidden']) assert.ok(!claims.video[grant], `grant ${grant}`);
    assert.ok(claims.exp - Date.now() / 1000 <= 600, 'short-lived');

    const asGuest = (method, path, sent) =>
      fetch(`${BACKEND}/api/v1${path}`, { method, headers: { Authorization: `Bearer ${body.token}`, 'Content-Type': 'application/json' }, body: sent && JSON.stringify(sent) });
    assert.equal((await asGuest('GET', '/rooms')).status, 403);
    assert.equal((await asGuest('GET', '/participants?room=standup')).status, 403);
    assert.equal((await asGuest('POST', '/tokens', { identity: 'x', room: ROOM })).status, 403);
    assert.equal((await asGuest('POST', '/join-links', { room: ROOM })).status, 403);
    assert.equal((await asGuest('GET', '/join-links')).status, 403);
  });

  it('turns off from the dashboard: nobody new gets in, and people in the room stay', async () => {
    await owner.goto(`${DASH}/rooms/${ROOM}`);
    const row = owner.getByTestId('share-link');
    await row.waitFor();
    assert.equal(await owner.getByTestId('share-link-url').inputValue(), linkUrl);
    assert.equal(await owner.getByTestId('share-link-uses').innerText(), '3');
    await owner.getByTestId('share-link-revoke').click();
    await owner.getByRole('alertdialog').getByRole('button', { name: 'Turn off' }).click();
    await eventually('the row goes', async () => (await row.count()) === 0);

    const late = await openGuest(linkUrl);
    await late.page.getByTestId('link-unusable').waitFor();
    assert.match(await late.page.getByTestId('link-unusable').innerText(), /turned off/);

    assert.equal(await asha.page.getByTestId('meeting-room').getAttribute('data-state'), 'connected');
    await assertPlaying(tile(asha.page, 'Ben').locator('video'), 'the meeting carries on');
  });

  it('tells someone removed from the meeting, and everyone when it closes', async () => {
    const people = await participants();
    const benIdentity = people.find((p) => p.name === 'Ben').identity;
    const removed = await api('DELETE', `/participants/${ROOM}/${benIdentity}`);
    assert.ok(removed.status < 300, removed.text);
    await ben.page.getByTestId('end-screen').waitFor({ timeout: 15000 });
    assert.match(await ben.page.getByTestId('end-screen').innerText(), /removed from the meeting/);

    const closed = await api('DELETE', `/rooms/${ROOM}`);
    assert.ok(closed.status < 300, closed.text);
    await asha.page.getByTestId('end-screen').waitFor({ timeout: 15000 });
    assert.match(await asha.page.getByTestId('end-screen').innerText(), /meeting was closed/);
  });
});

describe('other kinds of link', () => {
  it('makes a host link from the dashboard, and says what it is', async () => {
    await owner.goto(`${DASH}/rooms`);
    await owner.getByTestId('share-new').click();
    await owner.getByTestId('share-room').fill('hosted');
    await owner.getByLabel('People can', { exact: true }).click();
    await owner.getByRole('option', { name: 'Run the meeting, as a host', exact: true }).click();
    await owner.getByTestId('share-create').click();
    const field = owner.getByTestId('new-share-link');
    await field.waitFor();
    const url = await field.inputValue();
    await owner.getByRole('button', { name: 'Done' }).click();

    const host = await joinByLink(url, 'Dash host');
    assert.equal(await host.page.getByTestId('meeting-room').getAttribute('data-role'), 'host');
    assert.equal(new URL(host.page.url()).hash, '', 'the host code stayed in the address bar');
    await host.page.getByTestId('control-host').waitFor();
    // There is no guest link in this room yet, so there is nothing to offer for copying. The host link never is.
    await sleep(500);
    assert.equal(await host.page.getByTestId('copy-link').count(), 0);
    await host.page.getByTestId('control-leave').click();
    await host.page.getByTestId('end-screen').waitFor();

    // It is not in the address any more, so reloading has nothing to go on, and says where to find it.
    const reloading = await openGuest(url);
    await reloading.page.getByTestId('prejoin').waitFor();
    await eventually('the code to leave the address bar', async () => new URL(reloading.page.url()).hash === '');
    await reloading.page.reload();
    await reloading.page.getByTestId('link-unusable').waitFor();
    assert.match(await reloading.page.getByTestId('link-unusable').innerText(), /host link/);

    await owner.goto(`${DASH}/rooms/hosted`);
    await owner.getByTestId('share-link').getByText('Host link').waitFor();
  });

  it('lets a watch-only guest see and hear, with nothing to publish', async () => {
    const talk = await newLink({ room: 'webinar', label: 'Talk' });
    const watch = await newLink({ room: 'webinar', label: 'Talk', can_publish: false });
    assert.equal(watch.can_publish, false);

    const speaker = await openGuest(talk.url);
    await speaker.page.getByTestId('prejoin').waitFor();
    await join(speaker, 'Speaker');

    const viewer = await openGuest(watch.url);
    await viewer.page.getByTestId('watch-only').waitFor();
    assert.equal(await viewer.page.getByTestId('preview-video').count(), 0, 'no camera asked for');
    await join(viewer, 'Viewer');

    for (const control of ['control-mic', 'control-camera', 'control-screen']) assert.equal(await viewer.page.getByTestId(control).count(), 0, control);
    await assertPlaying(tile(viewer.page, 'Speaker').locator('video'), 'the viewer sees the speaker');
    await eventually('the viewer hears the speaker', async () => (await receivedSoundLevel(viewer.page)) > 0.001, { timeout: 30000 });

    const people = await participants('webinar');
    assert.equal(people.find((p) => p.name === 'Viewer').tracks.length, 0);
  });

  it('explains expired, wrong and cut-off links', async () => {
    const expired = await newLink({ room: 'old' });
    await sql(`UPDATE join_links SET expires_at = now() - interval '1 second' WHERE id = $1`, [expired.id]);
    const stale = await openGuest(expired.url);
    await stale.page.getByTestId('link-unusable').waitFor();
    assert.match(await stale.page.getByTestId('link-unusable').innerText(), /expired/);

    const wrong = await openGuest(`${DASH}/meet/join#not-a-real-code`);
    await wrong.page.getByTestId('link-unusable').waitFor();
    assert.match(await wrong.page.getByTestId('link-unusable').innerText(), /not valid/);

    const cut = await openGuest(`${DASH}/meet/join`);
    await cut.page.getByTestId('link-unusable').waitFor();
    assert.match(await cut.page.getByTestId('link-unusable').innerText(), /no meeting code/);
  });

  it('says so when the room is full', async () => {
    const created = await api('POST', '/rooms', { name: 'small', max_participants: 1 });
    assert.ok(created.status < 300, created.text);
    const { url } = await newLink({ room: 'small' });
    const first = await openGuest(url);
    await first.page.getByTestId('prejoin').waitFor();
    await join(first, 'First');

    const second = await openGuest(url);
    await second.page.getByTestId('display-name').fill('Second');
    await second.page.getByTestId('join-button').click();
    await second.page.getByTestId('join-failure').waitFor({ timeout: 30000 });
    assert.match(await second.page.getByTestId('join-failure').innerText(), /full/);
    assert.equal(await first.page.getByTestId('meeting-room').getAttribute('data-state'), 'connected');
  });

  it('still lets someone watch and listen from a page that is not secure', async () => {
    const { url } = await newLink({ room: 'lan' });
    const host = await openGuest(url);
    await host.page.getByTestId('prejoin').waitFor();
    await join(host, 'Host');

    const remote = await openGuest(url.replace('localhost', 'meet.test'), { from: insecureBrowser });
    await remote.page.getByTestId('insecure-page').waitFor();
    assert.equal(await remote.page.evaluate(() => window.isSecureContext), false);
    await join(remote, 'Remote');
    assert.equal(await remote.page.getByTestId('control-mic').count(), 0);
    await assertPlaying(tile(remote.page, 'Host').locator('video'), 'the remote guest sees the host');
  });

  it('fits a phone', async () => {
    const { url } = await newLink({ room: 'phone', label: 'A meeting with quite a long title, to see that it wraps' });
    const guest = await openGuest(url, { contextOptions: { viewport: { width: 390, height: 780 } } });
    await guest.page.getByTestId('prejoin').waitFor();
    assert.equal(await horizontalOverflow(guest.page), null);
    await guest.page.screenshot({ path: `${shotsDir}/prejoin-phone.png` });
    await join(guest, 'Phone');
    await tile(guest.page, 'Phone').waitFor();
    assert.equal(await horizontalOverflow(guest.page), null);
    await guest.page.screenshot({ path: `${shotsDir}/meeting-phone.png` });
  });
});

// The page's own Room, read from React's tree rather than through anything the production app exposes for tests.
const FIND_ROOM = `(() => {
  const el = document.querySelector('[data-testid="meeting-room"]');
  const key = Object.keys(el).find((k) => k.startsWith('__reactFiber$'));
  for (let f = el[key]; f; f = f.return) {
    const v = f.memoizedProps?.value;
    if (v?.simulateScenario) return v;
  }
  throw new Error('no room on this page');
})()`;

/** Runs code in a page with its real Room in scope, as `room`. */
function withRoom(page, body, arg) {
  return page.evaluate(new Function('arg', `const room = ${FIND_ROOM}; return (async () => { ${body} })();`), arg);
}

const identityOf = (page) => withRoom(page, 'return room.localParticipant.identity;');
const tokenOf = (page) => withRoom(page, 'return room.accessToken;');

/** The loudest sound arriving from one person's track, measured over a few seconds of what this page received. */
function soundFrom(page, identity, source = 'microphone') {
  return withRoom(
    page,
    `const p = room.remoteParticipants.get(arg.identity);
     const pub = p && p.getTrackPublications().find((x) => x.source === arg.source);
     const track = pub?.track?.mediaStreamTrack;
     if (!track) return 0;
     const ctx = new AudioContext();
     await ctx.resume();
     const an = ctx.createAnalyser();
     an.fftSize = 2048;
     ctx.createMediaStreamSource(new MediaStream([track])).connect(an);
     const samples = new Float32Array(2048);
     let peak = 0;
     const end = performance.now() + 4000;
     while (performance.now() < end) {
       an.getFloatTimeDomainData(samples);
       for (const s of samples) peak = Math.max(peak, Math.abs(s));
       await new Promise((r) => setTimeout(r, 40));
     }
     await ctx.close();
     return peak;`,
    { identity, source },
  );
}

const HEARD = 0.001;
const assertHears = (page, identity, what) => eventually(what, async () => (await soundFrom(page, identity)) > HEARD, { timeout: 30000 });

/** Says nothing is arriving, once anything already on its way has had time to play out. */
async function assertSilent(page, identity, what) {
  await sleep(1000);
  const level = await soundFrom(page, identity);
  assert.ok(level < HEARD, `${what}: level ${level}`);
}

/** Calls the API from inside a page, the way a guest trying their luck would, with their real token and no cookie. */
function callFromPage(page, method, path, token, body) {
  return page.evaluate(
    async ({ method, path, token, body }) => {
      const headers = { 'Content-Type': 'application/json' };
      if (token) headers.Authorization = `Bearer ${token}`;
      const res = await fetch(`/api/v1${path}`, { method, credentials: 'omit', headers, body: body === undefined ? undefined : JSON.stringify(body) });
      return { status: res.status, text: await res.text() };
    },
    { method, path, token, body },
  );
}

/** Tries a token on the signalling endpoint from a browser, and reports whether a session came of it. */
function trySignal(page, token) {
  return page.evaluate(
    ({ url, token }) =>
      new Promise((resolve) => {
        const ws = new WebSocket(`${url}/rtc?access_token=${encodeURIComponent(token)}&auto_subscribe=1&protocol=16`);
        ws.onopen = () => {
          ws.close();
          resolve('connected');
        };
        ws.onerror = () => resolve('refused');
      }),
    { url: WS_URL, token },
  );
}

/** What the server says when asked why, which is what the SDK shows people. */
async function whyRefused(token) {
  const res = await fetch(`${BACKEND}/rtc/validate?access_token=${encodeURIComponent(token)}`);
  return { status: res.status, text: (await res.text()).trim() };
}

async function assertTokenRefused(page, token, reason) {
  const why = await whyRefused(token);
  assert.equal(why.status, 401, why.text);
  assert.match(why.text, reason);
  assert.equal(await trySignal(page, token), 'refused');
}

async function openPanel(page, name) {
  await page.getByTestId(`control-${name}`).click();
  await page.getByTestId(`panel-${name}`).waitFor();
}

const closePanel = (page) => page.getByTestId('panel-close').click();

const person = (page, name) => page.locator(`[data-testid="person"][data-name="${name}"]`);

async function everyoneSees(pages, count) {
  await eventually(`everyone sees ${count} people`, async () => {
    for (const p of pages) {
      if ((await p.getByTestId('people-count').innerText()) !== String(count)) return false;
    }
    return true;
  });
}

async function joinByLink(url, name, options) {
  const guest = await openGuest(url, options);
  await guest.page.getByTestId('prejoin').waitFor();
  await join(guest, name);
  return guest;
}

/** Every button on the page that a screen reader would have no name for. */
const unnamedButtons = (page) =>
  page.locator('button, [role="switch"]').evaluateAll((els) =>
    els
      .filter((e) => {
        const label = e.getAttribute('aria-label') || e.textContent?.trim() || e.getAttribute('title');
        return !label;
      })
      .map((e) => e.outerHTML.slice(0, 120)),
  );

describe('starting a meeting', () => {
  it('does not let someone with no account start one', async () => {
    const stranger = await openGuest(`${DASH}/meet`);
    await stranger.page.getByTestId('start-signed-out').waitFor();
    assert.equal(await stranger.page.getByTestId('start-button').count(), 0);
    assert.equal(await stranger.page.getByTestId('start-sign-in').getAttribute('href'), '/login?next=/meet');
    // Nor by asking directly. This is the endpoint that would otherwise mint rooms for anyone.
    const direct = await callFromPage(stranger.page, 'POST', '/meetings', null, { title: 'free rooms' });
    assert.equal(direct.status, 401);
  });

  it('says what is missing when the project has no API key', async () => {
    const page = await owner.context().newPage();
    await page.goto(`${DASH}/meet`);
    await page.getByTestId('start-project').waitFor();
    assert.equal(await page.getByTestId('start-project').inputValue(), project, 'starts from the project the dashboard has open');
    await page.getByTestId('start-project').selectOption({ label: 'no keys here' });
    await page.getByTestId('start-button').click();
    await page.getByTestId('start-error').waitFor();
    assert.match(await page.getByTestId('start-error').innerText(), /API key/);
    await page.close();
  });
});

describe('running a meeting', () => {
  let host;
  let asha;
  let ben;
  let inviteUrl;
  let hostUrl;
  let meetingRoom;
  let hostIdentity;
  let ashaIdentity;
  let benIdentity;
  const stamped = () => [host.page, asha.page, ben.page];

  it('starts from the meeting app, signed in, and gives its starter a host link', async () => {
    // This replaces what the address was allowed, so camera and microphone go in the list: revoking them stops a capture.
    await owner.context().grantPermissions(['clipboard-read', 'clipboard-write', 'camera', 'microphone'], { origin: DASH });
    const page = await owner.context().newPage();
    host = { page, context: owner.context(), urls: [], joinBodies: [], errors: [] };
    everyGuest.push(host);
    page.on('pageerror', (e) => host.errors.push(e.message));
    page.on('request', (r) => host.urls.push(r.url()));
    page.on('response', async (r) => {
      if (r.url().includes('/api/v1/join') || r.url().includes('/api/v1/meetings')) host.joinBodies.push(await r.text().catch(() => ''));
    });
    await page.goto(`${DASH}/meet`);
    await page.getByTestId('start-title').fill('Planning');
    await page.getByTestId('start-button').click();
    await page.getByTestId('prejoin').waitFor();
    assert.equal(await page.getByTestId('meeting-title').innerText(), 'Planning');
    await page.getByTestId('prejoin-host').waitFor();

    hostUrl = JSON.parse(host.joinBodies.find((b) => b.includes('host_url'))).host_url;
    const hostCode = hostUrl.split('#')[1];
    assert.ok(hostCode.length >= 20, 'a code that cannot be guessed');
    for (const url of host.urls) assert.ok(!url.split('#')[0].includes(hostCode), `the host code is in ${url}`);
    // It came in the address, and has no business staying there, where it can be copied, or seen when the screen is shared.
    await eventually('the host code to leave the address bar', async () => new URL(page.url()).hash === '');

    await join(host, 'Hana');
    assert.equal(await page.getByTestId('meeting-room').getAttribute('data-role'), 'host');
    assert.equal(new URL(page.url()).hash, '', 'the host code came back to the address bar');
    meetingRoom = await withRoom(page, 'return room.name;');
    assert.match(meetingRoom, /^meet-/);
    hostIdentity = await identityOf(page);
    assert.match(hostIdentity, /^host-/);
  });

  it('hands out a guest link, never the host link, and holds the codes as hash and seal only', async () => {
    const { page } = host;
    await page.getByTestId('copy-link').click();
    await page.getByRole('button', { name: 'Copied' }).waitFor();
    inviteUrl = await page.evaluate(() => navigator.clipboard.readText());
    assert.ok(inviteUrl.startsWith(`${DASH}/meet/join#`), inviteUrl);
    assert.notEqual(inviteUrl, hostUrl, 'the host link must not be what gets shared');

    const preview = await callFromPage(page, 'POST', '/join/preview', null, { code: inviteUrl.split('#')[1] });
    assert.equal(JSON.parse(preview.text).role, 'guest');

    const rows = await sql(`SELECT j::text AS row FROM join_links j WHERE room = $1`, [meetingRoom]);
    assert.equal(rows.rows.length, 2);
    const hostCode = hostUrl.split('#')[1];
    for (const { row } of rows.rows) {
      assert.ok(!row.includes(hostCode) && !row.includes(inviteUrl.split('#')[1]), 'a code is readable in the database');
    }
  });

  it('never shows a host the API secret, and gives them no admin grant to leak', async () => {
    const token = await tokenOf(host.page);
    const claims = verifyJwt(token, key.secret);
    assert.ok(claims, 'signed with the project key');
    assert.equal(claims.attributes['meeting.role'], 'host');
    for (const grant of ['roomAdmin', 'roomCreate', 'roomList', 'roomRecord', 'hidden', 'ingressAdmin']) assert.ok(!claims.video[grant], `grant ${grant}`);
    for (const body of host.joinBodies) assert.ok(!body.includes(key.secret), 'a response carried the API secret');
    assert.ok(!(await host.page.content()).includes(key.secret));
    // A host token is good for running this meeting, and for nothing else on the API.
    for (const [method, path] of [['GET', '/rooms'], ['GET', '/recordings'], ['GET', '/egress'], ['POST', '/tokens'], ['POST', '/join-links'], ['GET', `/participants?room=${meetingRoom}`]]) {
      const res = await callFromPage(host.page, method, path, token, method === 'GET' ? undefined : {});
      assert.equal(res.status, 403, `${method} ${path}: ${res.text}`);
    }
  });

  it('brings guests in with real media, with no controls that are not theirs', async () => {
    asha = await joinByLink(inviteUrl, 'Asha');
    ben = await joinByLink(inviteUrl, 'Ben');
    await everyoneSees(stamped(), 3);
    ashaIdentity = await identityOf(asha.page);
    benIdentity = await identityOf(ben.page);
    assert.match(ashaIdentity, /^guest-/);
    assert.equal(await asha.page.getByTestId('meeting-room').getAttribute('data-role'), 'guest');
    assert.equal(await asha.page.getByTestId('control-host').count(), 0);
    await assertPlaying(tile(asha.page, 'Hana').locator('video'), 'Asha sees the host');
    await assertPlaying(tile(host.page, 'Ben').locator('video'), 'the host sees Ben');
    await assertHears(asha.page, benIdentity, 'Asha hears Ben');
    await assertHears(asha.page, hostIdentity, 'Asha hears the host');

    await openPanel(asha.page, 'people');
    await person(asha.page, 'Hana').getByTestId('person-role-name').waitFor();
    assert.match(await person(asha.page, 'Hana').getByTestId('person-role-name').innerText(), /Host/);
    assert.equal(await asha.page.getByTestId('person-mute').count(), 0, 'a guest has no mute buttons');
    assert.equal(await asha.page.getByTestId('person-remove').count(), 0, 'a guest has no remove buttons');
    await closePanel(asha.page);
  });

  it('lets a guest pass on the link they came in with, from the button or the address bar', async () => {
    const { page, context } = asha;
    assert.equal(page.url(), inviteUrl, 'a guest link is there to be passed on, and it stays in the address');
    // This replaces what the address was allowed, so camera and microphone go in the list: revoking them stops a capture.
    await context.grantPermissions(['clipboard-read', 'clipboard-write', 'camera', 'microphone'], { origin: DASH });
    await page.getByTestId('copy-link').click();
    await page.getByRole('button', { name: 'Copied' }).waitFor();
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), inviteUrl);
  });

  it('refuses a guest who calls the API directly from their own page', async () => {
    const token = await tokenOf(asha.page);
    const room = meetingRoom;
    const attempts = [
      ['POST', `/meetings/${room}/mute-all`, {}],
      ['POST', `/meetings/${room}/participants/${benIdentity}/mute`, {}],
      ['POST', `/meetings/${room}/participants/${benIdentity}/remove`, {}],
      ['POST', `/meetings/${room}/participants/${hostIdentity}/remove`, {}],
      ['POST', `/meetings/${room}/participants/${benIdentity}/lower-hand`, {}],
      ['PATCH', `/meetings/${room}/participants/${ashaIdentity}`, { role: 'moderator' }],
      ['PATCH', `/meetings/${room}`, { locked: true }],
      ['POST', `/meetings/${room}/end`, {}],
      ['POST', `/meetings/${room}/waiting/guest-x`, { admit: true }],
      // The general API, too.
      ['DELETE', `/participants/${room}/${benIdentity}`, {}],
      ['PATCH', `/participants/${room}/${benIdentity}`, { name: 'Hacked' }],
      ['DELETE', `/rooms/${room}`, {}],
      ['PATCH', `/rooms/${room}`, { metadata: '{"locked":true}' }],
      ['POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }],
    ];
    const tracks = await api('GET', `/tracks?room=${room}`);
    assert.equal(tracks.status, 200, tracks.text);
    const benMic = tracks.json.items.find((t) => t.identity === benIdentity && t.source === 'MICROPHONE');
    assert.ok(benMic, 'Ben has a microphone track to aim at');
    attempts.push(['POST', `/tracks/${room}/${benIdentity}/${benMic.sid}/mute`, { muted: true }]);

    for (const [method, path, body] of attempts) {
      const res = await callFromPage(asha.page, method, path, token, body);
      assert.equal(res.status, 403, `${method} ${path} returned ${res.status}: ${res.text}`);
    }
    // And nothing changed.
    await everyoneSees(stamped(), 3);
    assert.equal(await ben.page.getByTestId('control-mic').getAttribute('aria-pressed'), 'true');
    await assertHears(asha.page, benIdentity, 'Ben is still heard');
    assert.equal(await asha.page.getByTestId('meeting-room').getAttribute('data-role'), 'guest');

    // Without a token, no route says more than to sign in.
    const none = await callFromPage(asha.page, 'POST', `/meetings/${room}/mute-all`, null, {});
    assert.equal(none.status, 401);
  });

  it('lets a host mute one person: the sound stops at the viewer, and they can unmute', async () => {
    await openPanel(host.page, 'people');
    await person(host.page, 'Ben').getByTestId('person-mute').click();

    await ben.page.getByTestId('muted-by-host').waitFor();
    await eventually('Ben\'s own button follows', async () => (await ben.page.getByTestId('control-mic').getAttribute('aria-pressed')) === 'false');
    assert.match(await ben.page.getByTestId('control-mic').innerText(), /Unmute/);
    await eventually('Asha sees Ben muted', async () => (await tile(asha.page, 'Ben').getAttribute('data-audio-muted')) === 'true');
    await tile(asha.page, 'Ben').getByTestId('tile-muted').waitFor();
    await assertSilent(asha.page, benIdentity, 'Ben still reaches Asha after being muted');
    // The measurement itself works: the host is on the same page, and still comes through.
    await assertHears(asha.page, hostIdentity, 'Asha still hears the host');

    // Changing another device must not bring the microphone back. They are applied together.
    await ben.page.getByTestId('control-camera').click();
    await eventually('Ben\'s camera is off', async () => (await ben.page.getByTestId('control-camera').getAttribute('aria-pressed')) === 'false');
    await ben.page.getByTestId('control-camera').click();
    await eventually('Ben\'s camera is back', async () => (await ben.page.getByTestId('control-camera').getAttribute('aria-pressed')) === 'true');
    await sleep(1000);
    assert.equal(await ben.page.getByTestId('control-mic').getAttribute('aria-pressed'), 'false');
    await assertSilent(asha.page, benIdentity, 'Ben came back by changing his camera');

    await ben.page.getByTestId('control-mic').click();
    await eventually('Asha sees Ben unmute', async () => (await tile(asha.page, 'Ben').getAttribute('data-audio-muted')) === 'false');
    assert.equal(await ben.page.getByTestId('muted-by-host').count(), 0);
    await assertHears(asha.page, benIdentity, 'Asha hears Ben again');
    assert.equal(await tile(asha.page, 'Ben').getByTestId('tile-muted').count(), 0);
  });

  it('stops a client that ignores being muted, at the server', async () => {
    // This browser's microphone cannot be turned off from the page: it keeps sending whatever the app tells it.
    const stubborn = await openGuest(inviteUrl, { contextOptions: {} });
    await stubborn.page.addInitScript(() => {
      const enabled = Object.getOwnPropertyDescriptor(MediaStreamTrack.prototype, 'enabled');
      Object.defineProperty(MediaStreamTrack.prototype, 'enabled', { get: enabled.get, set() {} });
    });
    await stubborn.page.reload();
    await stubborn.page.getByTestId('prejoin').waitFor();
    await join(stubborn, 'Stubborn');
    const stubbornIdentity = await identityOf(stubborn.page);
    await assertHears(asha.page, stubbornIdentity, 'Asha hears the stubborn client');

    await person(host.page, 'Stubborn').getByTestId('person-mute').click();
    await eventually('Asha sees them muted', async () => (await tile(asha.page, 'Stubborn').getAttribute('data-audio-muted')) === 'true');

    const sent = () =>
      withRoom(
        stubborn.page,
        `const stats = await room.engine.publisher.pc.getStats();
         let packets = 0;
         stats.forEach((s) => { if (s.type === 'outbound-rtp' && s.kind === 'audio') packets += s.packetsSent; });
         const track = room.localParticipant.getTrackPublication('microphone').track.mediaStreamTrack;
         return { packets, enabled: track.enabled };`,
      );
    const before = await sent();
    await assertSilent(asha.page, stubbornIdentity, 'a client that ignored the mute was still heard');
    const later = await sent();
    assert.equal(later.enabled, true, 'the test client should still be sending');
    assert.ok(later.packets > before.packets + 100, `it should still be sending: ${before.packets} then ${later.packets}`);

    await stubborn.page.getByTestId('control-leave').click();
    await stubborn.page.getByTestId('end-screen').waitFor();
    await everyoneSees(stamped(), 3);
  });

  it('mutes everyone but the host at once, through the server', async () => {
    await closePanel(host.page);
    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-mute-all').click();
    await host.page.getByTestId('host-muted-count').waitFor();
    assert.match(await host.page.getByTestId('host-muted-count').innerText(), /Muted 2 people/);

    await asha.page.getByTestId('muted-by-host').waitFor();
    await ben.page.getByTestId('muted-by-host').waitFor();
    await eventually('the server has everyone muted but the host', async () => {
      const tracks = await api('GET', `/tracks?room=${meetingRoom}`);
      const mics = tracks.json.items.filter((x) => x.source === 'MICROPHONE');
      return mics.length === 3 && mics.every((t) => t.muted === (t.identity !== hostIdentity));
    });
    await assertSilent(asha.page, benIdentity, 'Ben is heard after mute all');
    await assertSilent(ben.page, ashaIdentity, 'Asha is heard after mute all');
    await assertHears(asha.page, hostIdentity, 'the host is not muted');

    // Pressing it again finds nobody left to mute.
    await host.page.getByTestId('host-mute-all').click();
    await eventually('nobody left', async () => /Nobody had a microphone on/.test(await host.page.getByTestId('host-muted-count').innerText()));

    await asha.page.getByTestId('control-mic').click();
    await ben.page.getByTestId('control-mic').click();
    await assertHears(asha.page, benIdentity, 'Asha hears Ben after he unmutes');
    await closePanel(host.page);
  });

  it('carries chat between two browsers, as text, from who really sent it', async () => {
    const other = await launch(SCREEN_ARGS);
    try {
      const carol = await openGuest(inviteUrl, { from: other });
      await carol.page.getByTestId('prejoin').waitFor();
      await join(carol, 'Carol');
      await everyoneSees([asha.page, carol.page], 4);

      await openPanel(asha.page, 'chat');
      const hostile = '<img src=x onerror="window.__owned=1"> hello';
      await asha.page.getByTestId('chat-input').fill(hostile);
      await asha.page.getByTestId('chat-send').click();
      await asha.page.getByTestId('chat-text').filter({ hasText: 'hello' }).waitFor();
      await eventually('sending clears the box', async () => (await asha.page.getByTestId('chat-input').inputValue()) === '');

      // Someone without the panel open is told there is something to read.
      await eventually('Carol has an unread badge', async () => (await carol.page.getByTestId('control-chat').getByTestId('control-badge').count()) === 1);
      await openPanel(carol.page, 'chat');
      const received = carol.page.getByTestId('chat-message').first();
      await received.waitFor();
      assert.equal(await received.getByTestId('chat-from').innerText(), 'Asha');
      assert.equal(await received.getByTestId('chat-text').innerText(), hostile);
      assert.equal(await carol.page.evaluate(() => window.__owned), undefined, 'a message ran as code');
      // It clears just after the panel draws, and a read can land in between.
      await eventually('reading it clears the badge', async () => (await carol.page.getByTestId('control-chat').getByTestId('control-badge').count()) === 0, { timeout: 5000 });

      // Back the other way, across browsers.
      await carol.page.getByTestId('chat-input').fill('and back');
      await carol.page.keyboard.press('Enter');
      await asha.page.getByTestId('chat-text').filter({ hasText: 'and back' }).waitFor();

      // A client that claims to be someone else in the message body gets its real name. The server stamps the sender.
      await withRoom(carol.page, `await room.localParticipant.publishData(new TextEncoder().encode(JSON.stringify({ text: 'forged', from: 'Hana' })), { reliable: true, topic: 'meeting.chat' });`);
      const forged = asha.page.getByTestId('chat-message').filter({ hasText: 'forged' });
      await forged.waitFor();
      assert.equal(await forged.getByTestId('chat-from').innerText(), 'Carol');
      // Rubbish on the topic shows nothing.
      await withRoom(carol.page, `await room.localParticipant.publishData(new TextEncoder().encode('not json'), { reliable: true, topic: 'meeting.chat' });`);
      await sleep(500);
      assert.equal(await asha.page.getByTestId('chat-message').filter({ hasText: 'not json' }).count(), 0);
      // A flood of messages reaches the page, and it keeps only the latest. It has no way of holding back the sender.
      await withRoom(carol.page, `for (let i = 0; i < 600; i++) room.localParticipant.publishData(new TextEncoder().encode(JSON.stringify({ text: 'flood ' + i })), { reliable: true, topic: 'meeting.chat' });`);
      await asha.page.getByTestId('chat-text').filter({ hasText: /^flood 599$/ }).waitFor({ timeout: 20000 });
      const shown = await asha.page.getByTestId('chat-message').count();
      assert.ok(shown <= 500, `${shown} messages on the page`);
      assert.equal(await asha.page.getByTestId('chat-text').filter({ hasText: /^flood 0$/ }).count(), 0, 'the oldest of them went');
      await closePanel(asha.page);

      // Reactions travel the same way, between the same two browsers.
      await asha.page.getByTestId('control-react').click();
      await asha.page.locator('[data-testid="reaction-send"][data-emoji="👏"]').click();
      const seen = carol.page.locator('[data-testid="reaction"][data-emoji="👏"]');
      await seen.waitFor();
      assert.equal(await seen.getAttribute('data-from'), 'Asha');
      await asha.page.locator('[data-testid="reaction"][data-emoji="👏"][data-from="You"]').waitFor();
      await eventually('the reaction goes away by itself', async () => (await seen.count()) === 0, { timeout: 8000 });
      // Only what the picker offers. A reaction cannot put text over someone's video.
      await withRoom(carol.page, `await room.localParticipant.publishData(new TextEncoder().encode(JSON.stringify({ emoji: 'BUY NOW' })), { reliable: true, topic: 'meeting.reaction' });`);
      await sleep(500);
      assert.equal(await asha.page.getByTestId('reaction').count(), 0);
      await asha.page.getByTestId('control-react').click();
      assert.equal(await asha.page.getByTestId('reaction-picker').count(), 0);

      await carol.page.getByTestId('control-leave').click();
      await carol.page.getByTestId('end-screen').waitFor();
      await everyoneSees(stamped(), 3);
    } finally {
      await other.close();
    }
  });

  it('shows a raised hand to everyone, including someone who joins later, and a host can lower it', async () => {
    await asha.page.getByTestId('control-hand').click();
    const onHost = tile(host.page, 'Asha').getByTestId('tile-hand');
    await onHost.waitFor();
    assert.match(await onHost.innerText(), /Hand raised/);
    await tile(ben.page, 'Asha').getByTestId('tile-hand').waitFor();
    await eventually('Asha\'s own button follows the server', async () => (await asha.page.getByTestId('control-hand').getAttribute('aria-pressed')) === 'true');
    assert.match(await asha.page.getByTestId('control-hand').innerText(), /Lower hand/);

    // It is state, not a message, so someone arriving now sees it.
    const late = await joinByLink(inviteUrl, 'Late');
    await tile(late.page, 'Asha').getByTestId('tile-hand').waitFor();
    await late.page.getByTestId('control-leave').click();
    await late.page.getByTestId('end-screen').waitFor();

    // Asha lowers her own.
    await asha.page.getByTestId('control-hand').click();
    await eventually('the hand comes down for everyone', async () => (await onHost.count()) === 0 && (await tile(ben.page, 'Asha').getByTestId('tile-hand').count()) === 0);

    // A guest cannot lower someone else's, or raise it for them: there is no route for the second at all.
    await ben.page.getByTestId('control-hand').click();
    await tile(asha.page, 'Ben').getByTestId('tile-hand').waitFor();
    const refused = await callFromPage(asha.page, 'POST', `/meetings/${meetingRoom}/participants/${benIdentity}/lower-hand`, await tokenOf(asha.page), {});
    assert.equal(refused.status, 403);

    // A host can, and the list puts raised hands first.
    await openPanel(host.page, 'people');
    assert.equal(await host.page.getByTestId('person').first().getAttribute('data-name'), 'Ben');
    await person(host.page, 'Ben').getByTestId('person-hand').waitFor();
    await person(host.page, 'Ben').getByTestId('person-lower-hand').click();
    await eventually('Ben\'s hand comes down', async () => (await ben.page.getByTestId('control-hand').getAttribute('aria-pressed')) === 'false');
    await closePanel(host.page);
  });

  it('lists who is here, with their role, microphone and connection', async () => {
    await openPanel(host.page, 'people');
    assert.equal(await host.page.getByTestId('person').count(), 3);
    const row = person(host.page, 'Asha');
    await row.waitFor();
    assert.match(await row.getByTestId('person-mic-state').innerText(), /Mic on/);
    assert.equal(await row.getAttribute('data-mic'), 'on');
    assert.match(await person(host.page, 'Hana').getByTestId('person-role-name').innerText(), /Host/);
    assert.equal(await person(host.page, 'Hana').getByTestId('person-remove').count(), 0, 'a host has no button to remove themselves');
    await closePanel(host.page);
  });

  it('shows the connection quality the server measures, and never makes one up', async () => {
    // The server scores everyone every few seconds. Until its first score, the page says it has not measured yet.
    const shown = tile(asha.page, 'Ben').getByTestId('tile-quality');
    await eventually('a score arrives', async () => (await shown.getAttribute('data-quality')) !== 'unknown', { timeout: 30000 });
    await eventually('the page and the server agree', async () => {
      const res = await api('GET', `/participants?room=${meetingRoom}`);
      const onServer = res.json.items.find((p) => p.identity === benIdentity).quality;
      return onServer.toLowerCase() === (await shown.getAttribute('data-quality'));
    }, { timeout: 30000 });

    // Real loss on one person's uplink, and the server's verdict about it reaches everyone else.
    const link = await startLossyLink(0.3);
    let shaky;
    try {
      shaky = await openGuest(inviteUrl);
      await shaky.page.addInitScript(routeMediaThrough, link.port);
      await shaky.page.reload();
      await shaky.page.getByTestId('prejoin').waitFor();
      await join(shaky, 'Shaky');
      await assertPlaying(tile(asha.page, 'Shaky').locator('video'), 'media still gets through the lossy link');
      const weak = tile(asha.page, 'Shaky').getByTestId('tile-quality');
      await eventually('the server scores the lossy connection as weak', async () => (await weak.getAttribute('data-quality')) === 'poor', { timeout: 60000 });
      assert.match(await weak.innerText(), /Weak/, 'said in words, not only in colour');
      const shakyId = await identityOf(shaky.page);
      await eventually('the server says the same, and counts what it lost', async () => {
        const people = await api('GET', `/participants?room=${meetingRoom}`);
        const mine = people.json.items.find((p) => p.identity === shakyId);
        return mine.quality === 'POOR' && mine.tracks.some((t) => t.packets_lost > 0);
      }, { timeout: 30000 });
      // Everyone else is watching this person, and it is one person's link that is bad. Give the server another round of
      // scoring, so this is not a reading from before the loss could have counted.
      await sleep(6000);
      await eventually('those watching read as fine, with none of that loss counted against them', async () => {
        const res = await api('GET', `/participants?room=${meetingRoom}`);
        const watching = res.json.items.filter((p) => p.identity !== shakyId);
        assert.equal(watching.length, 3);
        for (const p of watching) assert.ok(p.loss_ratio < 0.02, `${p.name} has loss ${p.loss_ratio}`);
        for (const name of ['Asha', 'Ben', 'Hana']) {
          if ((await tile(asha.page, name).getByTestId('tile-quality').getAttribute('data-quality')) === 'poor') return false;
        }
        return true;
      }, { timeout: 20000 });
    } finally {
      await shaky?.page.close();
      link.close();
    }
    await everyoneSees(stamped(), 3);
  });

  it('follows the active speaker in speaker view, from what the server reports', async () => {
    // Fake microphones beep too briefly for the server to call it speech, which it is right about: 1 look in 40 caught
    // it. So these two have a sound file for a microphone, with syllables, and go through the real audio path.
    const speech = joinPath(tmpDir, 'speech.wav');
    const syllables = '(0.5+0.5*sin(2*PI*4*t))';
    await run('ffmpeg', ['-y', '-hide_banner', '-loglevel', 'error', '-f', 'lavfi', '-i', `aevalsrc='(0.5*sin(2*PI*140*t)+0.3*sin(2*PI*280*t)+0.2*sin(2*PI*560*t)+0.1*sin(2*PI*1120*t))*${syllables}':s=48000:d=20`, '-ac', '1', '-c:a', 'pcm_s16le', speech]);
    const talking = await launch([`--use-file-for-fake-audio-capture=${speech}`, ...SCREEN_ARGS]);
    try {
      const { url } = await newLink({ room: 'speakers' });
      const watcher = await joinByLink(url, 'Watcher');
      await watcher.page.getByTestId('control-mic').click();
      const pat = await joinByLink(url, 'Pat', { from: talking });
      const quinn = await joinByLink(url, 'Quinn', { from: talking });
      await everyoneSees([watcher.page, pat.page, quinn.page], 3);
      const patIdentity = await identityOf(pat.page);
      const quinnIdentity = await identityOf(quinn.page);

      // Only one person at a time has their microphone on, so there is no doubt who the server should name.
      await pat.page.getByTestId('control-mic').click();
      await quinn.page.getByTestId('control-mic').click();
      await watcher.page.getByTestId('control-layout').click();
      const stage = watcher.page.getByTestId('stage');
      await eventually('speaker view', async () => (await stage.getAttribute('data-layout')) === 'speaker');
      assert.match(await watcher.page.getByTestId('control-layout').innerText(), /Grid view/);
      const featured = watcher.page.locator('[data-testid="tile"][data-featured="true"]');
      await featured.locator('video').first().waitFor();
      assert.equal(await watcher.page.getByTestId('stage-strip').getByTestId('tile').count(), 2, 'the others are in a strip');

      // Quinn first: with nobody having spoken, the view would show Pat anyway, and that would prove nothing.
      await quinn.page.getByTestId('control-mic').click();
      await eventually('Quinn becomes the featured speaker', async () => (await featured.getAttribute('data-identity')) === quinnIdentity, { timeout: 30000 });
      await assertPlaying(featured.locator('video').first(), 'the featured speaker\'s video');

      await quinn.page.getByTestId('control-mic').click();
      await pat.page.getByTestId('control-mic').click();
      await eventually('Pat becomes the featured speaker', async () => (await featured.getAttribute('data-identity')) === patIdentity, { timeout: 30000 });
      const mark = tile(quinn.page, 'Pat').getByTestId('tile-speaking');
      await eventually('Pat is marked as speaking, with more than colour', async () => (await mark.count()) === 1, { timeout: 30000 });
      assert.equal(await mark.getAttribute('aria-label'), 'Speaking');
      assert.equal(await tile(watcher.page, 'Pat').getAttribute('data-speaking'), 'true');
      assert.equal(await tile(watcher.page, 'Quinn').getAttribute('data-speaking'), 'false');

      // Staying on the last speaker when nobody is talking: no flipping back and forth.
      await pat.page.getByTestId('control-mic').click();
      await eventually('the mark comes off Pat', async () => (await mark.count()) === 0, { timeout: 15000 });
      await eventually('nobody is marked as speaking', async () => (await withRoom(watcher.page, 'return room.activeSpeakers.length;')) === 0, { timeout: 15000 });
      await sleep(2000);
      assert.equal(await featured.getAttribute('data-identity'), patIdentity);

      await watcher.page.getByTestId('control-layout').click();
      await eventually('grid view', async () => (await stage.getAttribute('data-layout')) === 'grid');
      await eventually('a grid of everyone', async () => (await watcher.page.getByTestId('tile').count()) === 3);
      for (const guest of [watcher, pat, quinn]) {
        await guest.page.getByTestId('control-leave').click();
        await guest.page.getByTestId('end-screen').waitFor();
      }
    } finally {
      await talking.close();
    }
  });

  it('shares a screen with its audio, heard by the viewer apart from the microphone', async () => {
    await asha.page.getByTestId('control-mic').click();
    await eventually('Ben sees Asha muted', async () => (await tile(ben.page, 'Asha').getAttribute('data-audio-muted')) === 'true');
    await asha.page.getByTestId('control-screen').click();
    const shared = ben.page.getByTestId('tile').filter({ has: ben.page.getByTestId('tile-name').filter({ hasText: "Asha's screen" }) });
    await shared.waitFor({ timeout: 30000 });
    await assertPlaying(shared.locator('video'), "Ben sees Asha's screen");
    await eventually('Asha is told it has sound', async () => (await asha.page.getByTestId('screen-audio-state').getAttribute('data-sharing')) === 'true');

    await eventually('the server has the screen and its audio as tracks of their own', async () => {
      const tracks = await api('GET', `/tracks?room=${meetingRoom}`);
      const sources = tracks.json.items.filter((t) => t.identity === ashaIdentity).map((t) => t.source).sort();
      return sources.join() === 'CAMERA,MICROPHONE,SCREEN_SHARE,SCREEN_SHARE_AUDIO';
    });
    await eventually('Ben hears the screen', async () => (await soundFrom(ben.page, ashaIdentity, 'screen_share_audio')) > HEARD, { timeout: 30000 });
    await assertSilent(ben.page, ashaIdentity, 'muting the microphone did not stop only that');

    await asha.page.getByTestId('control-screen').click();
    await eventually('the shared screen goes away', async () => (await shared.count()) === 0);
    await eventually('its audio ends with it', async () => {
      const after = await api('GET', `/tracks?room=${meetingRoom}`);
      return !after.json.items.some((t) => t.source === 'SCREEN_SHARE_AUDIO');
    });
    await asha.page.getByTestId('control-mic').click();
    await assertHears(ben.page, ashaIdentity, 'Ben hears Asha again');
  });

  it('makes someone a moderator: they can keep order, and cannot run the meeting', async () => {
    await openPanel(host.page, 'people');
    await person(host.page, 'Asha').getByTestId('person-role').click();
    await eventually('Asha\'s page follows', async () => (await asha.page.getByTestId('meeting-room').getAttribute('data-role')) === 'moderator');
    await tile(ben.page, 'Asha').getByTestId('tile-role').waitFor();
    assert.equal(await tile(ben.page, 'Asha').getByTestId('tile-role').getAttribute('aria-label'), 'Moderator');

    await openPanel(asha.page, 'host');
    await asha.page.getByTestId('host-mute-all').waitFor();
    for (const hostOnly of ['host-record', 'host-lock', 'host-waiting', 'host-end', 'host-title']) {
      assert.equal(await asha.page.getByTestId(hostOnly).count(), 0, `a moderator sees ${hostOnly}`);
    }
    await closePanel(asha.page);
    await openPanel(asha.page, 'people');
    assert.equal(await person(asha.page, 'Hana').getByTestId('person-remove').count(), 0, 'no button to remove a host');
    await person(asha.page, 'Ben').getByTestId('person-mute').click();
    await ben.page.getByTestId('muted-by-host').waitFor();
    await ben.page.getByTestId('control-mic').click();
    await closePanel(asha.page);

    const token = await tokenOf(asha.page);
    for (const [method, path, body] of [
      ['PATCH', `/meetings/${meetingRoom}`, { locked: true }],
      ['POST', `/meetings/${meetingRoom}/end`, {}],
      ['POST', `/meetings/${meetingRoom}/participants/${hostIdentity}/remove`, {}],
      ['PATCH', `/meetings/${meetingRoom}/participants/${benIdentity}`, { role: 'moderator' }],
      ['POST', '/egress', { type: 'room_composite', room: meetingRoom, output: { kind: 'file', format: 'mp4' } }],
    ]) {
      const res = await callFromPage(asha.page, method, path, token, body);
      assert.equal(res.status, 403, `a moderator called ${method} ${path}: ${res.text}`);
    }

    // Taken away, and it stops at once, with the same token.
    await person(host.page, 'Asha').getByTestId('person-role').click();
    await eventually('Asha is a guest again', async () => (await asha.page.getByTestId('meeting-room').getAttribute('data-role')) === 'guest');
    const after = await callFromPage(asha.page, 'POST', `/meetings/${meetingRoom}/mute-all`, token, {});
    assert.equal(after.status, 403);
    await closePanel(host.page);
  });

  it('records for real: the host starts and stops it, everyone sees it, and a file comes out', async (t) => {
    if (!workersRunning()) return t.skip('the workers did not build');
    await eventually('a recording worker reports in', async () => {
      const res = await pageApi(owner, 'GET', '/servers');
      return res.json.items.some((n) => n.stats.kind === 'egress_worker');
    }, { timeout: 30000 });

    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-record').click();
    // Everyone is told, from what the server says: a recorder really in the room.
    await asha.page.getByTestId('recording-indicator').waitFor({ timeout: 60000 });
    await ben.page.getByTestId('recording-indicator').waitFor({ timeout: 60000 });
    await host.page.getByTestId('recording-indicator').waitFor({ timeout: 60000 });
    assert.match(await asha.page.getByTestId('recording-indicator').innerText(), /Recording/);
    assert.equal(await withRoom(ben.page, 'return room.isRecording;'), true);
    await everyoneSees(stamped(), 3); // The recorder is not one of the people in the meeting.

    // Someone joining during it is told straight away.
    const late = await joinByLink(inviteUrl, 'Late too');
    await late.page.getByTestId('recording-indicator').waitFor();
    await late.page.getByTestId('control-leave').click();
    await late.page.getByTestId('end-screen').waitFor();
    assert.equal(await late.page.getByTestId('end-recorded').getAttribute('data-recorded'), 'true');

    await sleep(6000);
    const stoppable = host.page.getByRole('button', { name: 'Stop recording' });
    await eventually('the host can stop it', () => stoppable.isEnabled());

    // A guest cannot stop it, even knowing which job it is. They would not, but they could guess.
    const list = await api('GET', '/recordings');
    assert.equal(list.status, 200, list.text);
    const job = list.json.items.find((r) => r.room_name === meetingRoom);
    assert.ok(job, 'the job is on the project\'s recordings');
    const refused = await callFromPage(asha.page, 'POST', `/egress/${job.id}/stop`, await tokenOf(asha.page), {});
    assert.equal(refused.status, 403);
    // Nor list the project's recordings, or read the file, as the host either.
    const hostToken = await tokenOf(host.page);
    assert.equal((await callFromPage(host.page, 'GET', '/recordings', hostToken)).status, 403);
    assert.equal((await callFromPage(host.page, 'GET', `/recordings/${job.id}/file`, hostToken)).status, 403);

    await stoppable.click();
    await eventually('everyone sees it stop', async () => (await asha.page.getByTestId('recording-indicator').count()) === 0 && (await ben.page.getByTestId('recording-indicator').count()) === 0, { timeout: 60000 });
    await asha.page.getByTestId('recording-news').waitFor();

    const done = await eventually('the job to finish', async () => {
      const res = await api('GET', `/recordings/${job.id}`);
      return res.json.status === 'complete' ? res.json : null;
    }, { timeout: 60000 });
    assert.ok(done.file_size > 10000, `${done.file_size} bytes`);

    const file = await owner.context().request.get(`${DASH}/api/v1/recordings/${job.id}/file?project_id=${project}`);
    assert.equal(file.status(), 200);
    const path = joinPath(tmpDir, 'meeting-recording.mp4');
    writeFileSync(path, await file.body());
    const { stdout } = await run('ffprobe', ['-v', 'error', '-print_format', 'json', '-show_format', '-show_streams', path]);
    const probe = JSON.parse(stdout);
    assert.ok(probe.streams.some((s) => s.codec_type === 'video'), 'it has video');
    assert.ok(probe.streams.some((s) => s.codec_type === 'audio'), 'it has audio');
    assert.ok(Number(probe.format.duration) > 3, `it lasts ${probe.format.duration}s`);

    await eventually('the host can record again', async () => /Start recording/.test(await host.page.getByTestId('host-record').innerText()), { timeout: 30000 });
    await closePanel(host.page);
  });

  it('shows the meeting\'s details, and a new title reaches everyone without a reload', async () => {
    await openPanel(asha.page, 'settings');
    assert.equal(await asha.page.getByTestId('info-title').innerText(), 'Planning');
    assert.equal(await asha.page.getByTestId('info-room').innerText(), meetingRoom);
    assert.equal(await asha.page.getByTestId('info-role').innerText(), 'A guest');
    assert.equal(await asha.page.getByTestId('info-people').innerText(), '3');

    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-title').fill('Planning, part two');
    await host.page.getByTestId('host-title-save').click();
    await eventually('the title changes for a guest', async () => (await asha.page.getByTestId('room-title').innerText()) === 'Planning, part two');
    assert.equal(await asha.page.getByTestId('info-title').innerText(), 'Planning, part two');
    await closePanel(asha.page);

    // It is the room's metadata, and new arrivals see it before they join.
    const onServer = await api('GET', `/rooms/${meetingRoom}`);
    assert.equal(JSON.parse(onServer.json.metadata).title, 'Planning, part two');
    const preview = await callFromPage(asha.page, 'POST', '/join/preview', null, { code: inviteUrl.split('#')[1] });
    assert.equal(JSON.parse(preview.text).title, 'Planning, part two');
    await closePanel(host.page);
  });

  it('recovers from a dropped connection, and says so while it does', async () => {
    const seen = () => ben.page.evaluate(() => window.__states ?? []);
    await ben.page.evaluate(() => {
      const el = document.querySelector('[data-testid="meeting-room"]');
      window.__states = [];
      new MutationObserver(() => window.__states.push(el.dataset.state)).observe(el, { attributes: true, attributeFilter: ['data-state'] });
    });

    // A real drop of the signal connection. Everything sent while it is down has to wait.
    await ben.context.setOffline(true);
    await withRoom(ben.page, `room.simulateScenario('signal-reconnect');`);
    await ben.page.getByTestId('reconnecting').waitFor({ timeout: 30000 });
    assert.match(await ben.page.getByTestId('reconnecting').innerText(), /Trying to reconnect/);
    assert.equal(await ben.page.getByTestId('meeting-room').getAttribute('data-state'), 'signalReconnecting');
    await sleep(2000);
    assert.equal(await ben.page.getByTestId('reconnecting').count(), 1, 'it stays while the network is down');
    await ben.context.setOffline(false);
    await ben.page.getByTestId('reconnected').waitFor({ timeout: 60000 });
    assert.equal(await ben.page.getByTestId('meeting-room').getAttribute('data-state'), 'connected');
    assert.equal(await ben.page.getByTestId('reconnecting').count(), 0);
    assert.deepEqual((await seen()).slice(-2), ['signalReconnecting', 'connected']);
    await everyoneSees(stamped(), 3);
    await assertPlaying(tile(asha.page, 'Ben').locator('video'), "Ben's video after a dropped signal connection");
    await assertPlaying(tile(ben.page, 'Asha').locator('video'), 'Asha seen by Ben after a dropped signal connection');
    await assertHears(asha.page, benIdentity, 'Ben is heard after a dropped signal connection');

    // The whole session lost, so a new one under the same identity.
    await withRoom(ben.page, `room.simulateScenario('full-reconnect');`);
    await eventually('Ben goes through reconnecting', async () => (await seen()).includes('reconnecting'), { timeout: 30000 });
    await eventually('Ben is back', async () => (await ben.page.getByTestId('meeting-room').getAttribute('data-state')) === 'connected', { timeout: 60000 });
    assert.equal(await identityOf(ben.page), benIdentity);
    await everyoneSees(stamped(), 3);
    await assertPlaying(tile(asha.page, 'Ben').locator('video'), "Ben's video after a full reconnect");
    await assertHears(asha.page, benIdentity, 'Ben is heard after a full reconnect');
  });

  it('removes someone: they are disconnected, and their token is no good any more', async () => {
    const dan = await joinByLink(inviteUrl, 'Dan');
    await everyoneSees([host.page, asha.page], 4);
    const danIdentity = await identityOf(dan.page);
    const refreshed = await tokenOf(dan.page);
    const original = dan.joinBodies.map((b) => JSON.parse(b)).find((b) => b.token).token;
    // The token a browser holds in a session is the server's refresh of it, good for hours. That is the one that matters.
    assert.ok(verifyJwt(refreshed, key.secret).exp - Date.now() / 1000 > 3600, 'the page holds a long refreshed token');

    await openPanel(host.page, 'people');
    await person(host.page, 'Dan').getByTestId('person-remove').click();
    await person(host.page, 'Dan').getByTestId('person-remove-confirm').click();

    await dan.page.getByTestId('end-screen').waitFor({ timeout: 15000 });
    assert.match(await dan.page.getByTestId('end-screen').innerText(), /removed from the meeting/);
    assert.equal(await dan.page.getByTestId('rejoin').count(), 0, 'no invitation back');
    await everyoneSees([host.page, asha.page], 3);
    const people = await participants(meetingRoom);
    assert.ok(!people.some((p) => p.identity === danIdentity));

    for (const token of [refreshed, original]) await assertTokenRefused(dan.page, token, /removed/);
    const res = await callFromPage(dan.page, 'POST', `/meetings/${meetingRoom}/hand`, refreshed, { raised: true });
    assert.equal(res.status, 403, 'nor can they use the API');
    await everyoneSees([host.page, asha.page], 3);

    // Removing someone from the dashboard shuts their token out the same way.
    const eve = await joinByLink(inviteUrl, 'Eve');
    const eveToken = await tokenOf(eve.page);
    const eveIdentity = await identityOf(eve.page);
    const removed = await api('DELETE', `/participants/${meetingRoom}/${eveIdentity}`);
    assert.ok(removed.status < 300, removed.text);
    await eve.page.getByTestId('end-screen').waitFor({ timeout: 15000 });
    await assertTokenRefused(eve.page, eveToken, /removed/);
    await closePanel(host.page);
  });

  it('locks the meeting: nobody new, whether by link or by a token they already hold', async () => {
    // Someone who got a token before the lock, and has not used it yet.
    const early = await openGuest(inviteUrl);
    await early.page.getByTestId('prejoin').waitFor();
    const minted = JSON.parse((await callFromPage(early.page, 'POST', '/join', null, { code: inviteUrl.split('#')[1], name: 'Early' })).text);
    assert.ok(minted.token);

    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-lock').click();
    await asha.page.getByTestId('locked-indicator').waitFor();
    await ben.page.getByTestId('locked-indicator').waitFor();
    assert.equal(await host.page.getByTestId('host-lock').getAttribute('aria-checked'), 'true');
    assert.match(await host.page.getByTestId('host-lock').innerText(), /On/, 'said in words, not only in colour');

    // By link: the page says so before anyone tries, and the server refuses when they do.
    const late = await openGuest(inviteUrl);
    await late.page.getByTestId('prejoin-locked').waitFor();
    await late.page.getByTestId('display-name').fill('Too late');
    await late.page.getByTestId('join-button').click();
    await late.page.getByTestId('join-error').waitFor();
    assert.match(await late.page.getByTestId('join-error').innerText(), /locked/);

    // With a token from before: no session. Asking why changes nothing.
    await assertTokenRefused(early.page, minted.token, /locked/);
    await assertTokenRefused(early.page, minted.token, /locked/);
    await everyoneSees([host.page, asha.page, ben.page], 3);

    // Someone already in can drop and come back, and a host can always join.
    await withRoom(asha.page, `room.simulateScenario('full-reconnect');`);
    await eventually('Asha reconnects through the lock', async () => (await asha.page.getByTestId('meeting-room').getAttribute('data-state')) === 'connected' && (await withRoom(asha.page, 'return room.remoteParticipants.size;')) === 2, { timeout: 60000 });
    await everyoneSees([host.page, ben.page], 3);
    const secondHost = await joinByLink(hostUrl, 'Second host');
    assert.equal(await secondHost.page.getByTestId('meeting-room').getAttribute('data-role'), 'host');
    await secondHost.page.getByTestId('control-leave').click();
    await secondHost.page.getByTestId('end-screen').waitFor();
    await everyoneSees([host.page, ben.page], 3);

    await host.page.getByTestId('host-lock').click();
    await eventually('it unlocks for everyone', async () => (await asha.page.getByTestId('locked-indicator').count()) === 0);
    const after = await joinByLink(inviteUrl, 'After the lock');
    await after.page.getByTestId('control-leave').click();
    await after.page.getByTestId('end-screen').waitFor();
    await closePanel(host.page);
  });

  it('keeps people in a waiting room until someone lets them in, with no token until then', async () => {
    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-waiting').click();
    await eventually('it is on', async () => (await host.page.getByTestId('host-waiting').getAttribute('aria-checked')) === 'true');
    await closePanel(host.page);

    const guest = await openGuest(inviteUrl);
    await guest.page.getByTestId('prejoin-waiting').waitFor();
    assert.match(await guest.page.getByTestId('join-button').innerText(), /Ask to join/);
    await guest.page.getByTestId('display-name').fill('Farah');
    await guest.page.getByTestId('join-button').click();
    await guest.page.getByTestId('waiting-room').waitFor();
    assert.ok(!guest.joinBodies.some((b) => b.includes('"token"')), 'a token before being let in');

    // A guest in the meeting cannot see who is at the door. The host can, without opening anything.
    const badge = host.page.getByTestId('control-people').getByTestId('control-badge');
    await eventually('the host is told', async () => (await badge.count()) === 1 && (await badge.innerText()) === '1', { timeout: 15000 });
    const ashaView = JSON.parse((await callFromPage(asha.page, 'GET', `/meetings/${meetingRoom}`, await tokenOf(asha.page))).text);
    assert.equal(ashaView.waiting, undefined);

    await openPanel(host.page, 'people');
    await host.page.locator('[data-testid="waiting-row"][data-name="Farah"]').getByTestId('waiting-admit').click();
    await guest.page.locator('[data-testid="meeting-room"][data-state="connected"]').waitFor({ timeout: 30000 });
    await tile(host.page, 'Farah').waitFor();
    await assertPlaying(tile(host.page, 'Farah').locator('video'), "the host sees Farah's video");
    await eventually('nobody is waiting now', async () => (await badge.count()) === 0);
    await guest.page.getByTestId('control-leave').click();
    await guest.page.getByTestId('end-screen').waitFor();

    // Turned away.
    const other = await openGuest(inviteUrl);
    await other.page.getByTestId('prejoin-waiting').waitFor();
    await other.page.getByTestId('display-name').fill('Gus');
    await other.page.getByTestId('join-button').click();
    await other.page.getByTestId('waiting-room').waitFor();
    const row = host.page.locator('[data-testid="waiting-row"][data-name="Gus"]');
    await row.waitFor({ timeout: 15000 });
    await row.getByTestId('waiting-deny').click();
    await other.page.getByTestId('link-unusable').waitFor({ timeout: 15000 });
    assert.match(await other.page.getByTestId('link-unusable').innerText(), /did not let you in/);

    // Someone waiting who gives up.
    const third = await openGuest(inviteUrl);
    await third.page.getByTestId('prejoin-waiting').waitFor();
    await third.page.getByTestId('display-name').fill('Hal');
    await third.page.getByTestId('join-button').click();
    await third.page.getByTestId('waiting-room').waitFor();
    await host.page.locator('[data-testid="waiting-row"][data-name="Hal"]').waitFor({ timeout: 15000 });
    await third.page.getByTestId('waiting-leave').click();
    await third.page.getByTestId('prejoin').waitFor();
    await eventually('Hal leaves the list', async () => (await host.page.locator('[data-testid="waiting-row"][data-name="Hal"]').count()) === 0, { timeout: 15000 });

    // A waiting room full of strangers can be cleared at once.
    const crowd = [];
    for (const name of ['Jo', 'Kim']) {
      const person = await openGuest(inviteUrl);
      await person.page.getByTestId('prejoin-waiting').waitFor();
      await person.page.getByTestId('display-name').fill(name);
      await person.page.getByTestId('join-button').click();
      await person.page.getByTestId('waiting-room').waitFor();
      crowd.push(person);
    }
    await eventually('both are listed', async () => (await host.page.getByTestId('waiting-count').innerText().catch(() => '')) === '2', { timeout: 15000 });
    await host.page.getByTestId('waiting-deny-all').click();
    for (const person of crowd) {
      await person.page.getByTestId('link-unusable').waitFor({ timeout: 15000 });
      assert.match(await person.page.getByTestId('link-unusable').innerText(), /did not let you in/);
    }
    await eventually('nobody is listed', async () => (await host.page.getByTestId('waiting-row').count()) === 0, { timeout: 15000 });
    await closePanel(host.page);

    // Turning it off opens the door for whoever is at it.
    const fourth = await openGuest(inviteUrl);
    await fourth.page.getByTestId('prejoin-waiting').waitFor();
    await fourth.page.getByTestId('display-name').fill('Ivy');
    await fourth.page.getByTestId('join-button').click();
    await fourth.page.getByTestId('waiting-room').waitFor();
    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-waiting').click();
    await fourth.page.locator('[data-testid="meeting-room"][data-state="connected"]').waitFor({ timeout: 30000 });
    await fourth.page.getByTestId('control-leave').click();
    await fourth.page.getByTestId('end-screen').waitFor();
    await closePanel(host.page);
  });

  it('ends the meeting for everyone, records the summary, and old tokens stay dead', async () => {
    const guestToken = await tokenOf(asha.page);
    const oldHostToken = await tokenOf(host.page);
    await openPanel(host.page, 'host');
    await host.page.getByTestId('host-end').click();
    // Not on the first press.
    await sleep(500);
    assert.equal(await asha.page.getByTestId('meeting-room').getAttribute('data-state'), 'connected');
    await host.page.getByTestId('host-end-confirm').click();

    for (const p of [host.page, asha.page, ben.page]) await p.getByTestId('end-screen').waitFor({ timeout: 15000 });
    assert.match(await asha.page.getByTestId('end-screen').innerText(), /meeting was closed/);
    assert.equal(await asha.page.getByTestId('rejoin').count(), 0, 'a guest has nothing to go back to');

    // The summary: only what happened, from the server's record.
    const summary = asha.page.getByTestId('end-summary');
    await asha.page.getByTestId('end-meeting-duration').waitFor({ timeout: 15000 });
    await eventually('the full list of people', async () => /Hana/.test(await asha.page.getByTestId('end-people').innerText()) && /Ben/.test(await asha.page.getByTestId('end-people').innerText()), { timeout: 15000 });
    assert.match(await summary.innerText(), /Who was there/);
    assert.ok(!/\b(discussed|decided|action items|topics?)\b/i.test(await summary.innerText()), 'a summary must not invent what was said');
    if (workersRunning()) {
      assert.equal(await asha.page.getByTestId('end-recorded').getAttribute('data-recorded'), 'true');
      await host.page.getByTestId('end-recording-note').waitFor();
    }
    await asha.page.screenshot({ path: `${shotsDir}/meeting-summary.png` });

    // Tokens from before stop, the host's too. A room closed by a host must not reopen for someone holding one.
    await assertTokenRefused(asha.page, guestToken, /ended/);
    await assertTokenRefused(asha.page, oldHostToken, /ended/);
    assert.equal((await api('GET', `/rooms/${meetingRoom}`)).status, 404, 'the room stayed closed');

    // The guest link says so.
    const late = await openGuest(inviteUrl);
    await late.page.getByTestId('link-unusable').waitFor();
    assert.match(await late.page.getByTestId('link-unusable').innerText(), /has ended/);

    // A host can start it again, and then the guest link works.
    assert.match(await host.page.getByTestId('rejoin').innerText(), /Start the meeting again/);
    await host.page.getByTestId('rejoin').click();
    await host.page.getByTestId('prejoin').waitFor();
    await eventually('the page knows it had ended', async () => /starting this meeting again/.test(await host.page.getByTestId('prejoin-host').innerText()));
    await join(host, 'Hana');
    const back = await joinByLink(inviteUrl, 'Back again');
    await everyoneSees([host.page, back.page], 2);
    await assertPlaying(tile(back.page, 'Hana').locator('video'), 'the restarted meeting has real media');
    await assertTokenRefused(asha.page, guestToken, /ended/);

    await host.page.getByTestId('control-leave').click();
    await host.page.getByTestId('end-screen').waitFor();
    await back.page.getByTestId('control-leave').click();
    await back.page.getByTestId('end-screen').waitFor();
    await host.page.close();
  });

  it('finishes a recording as a whole file when the meeting ends during it', async (t) => {
    if (!workersRunning()) return t.skip('the workers did not build');
    const made = await api('POST', '/meetings', { title: 'Recorded to the end' });
    assert.equal(made.status, 201, made.text);
    const hana = await joinByLink(made.json.host_url, 'Hana');
    await openPanel(hana.page, 'host');
    await hana.page.getByTestId('host-record').click();
    await hana.page.getByTestId('recording-indicator').waitFor({ timeout: 60000 });
    await sleep(5000);
    await hana.page.getByTestId('host-end').click();
    await hana.page.getByTestId('host-end-confirm').click();
    await hana.page.getByTestId('end-screen').waitFor({ timeout: 15000 });

    const job = await eventually('the job to end', async () => {
      const res = await api('GET', '/recordings');
      const found = res.json.items.find((r) => r.room_name === made.json.room);
      return found && ['complete', 'failed', 'aborted'].includes(found.status) ? found : null;
    }, { timeout: 60000 });
    assert.equal(job.status, 'complete', job.error);
    assert.ok(job.file_size > 10000, `${job.file_size} bytes`);
    assert.equal(await hana.page.getByTestId('end-recorded').getAttribute('data-recorded'), 'true');
  });
});

describe('devices', () => {
  let many;
  let viewerUrl;
  let viewer;
  let guest;

  before(async () => {
    many = await launch(MANY_DEVICES_ARGS);
    const { url } = await newLink({ room: 'devices' });
    viewerUrl = url;
  });

  after(async () => {
    await many?.close();
  });

  const settingOf = (page, source) => withRoom(page, `return room.localParticipant.getTrackPublication(arg).track.mediaStreamTrack.getSettings().deviceId;`, source);

  it('uses the camera and microphone chosen before joining', async () => {
    guest = await openGuest(viewerUrl, { from: many });
    const { page } = guest;
    await page.getByTestId('prejoin').waitFor();
    await eventually('the cameras are listed', async () => (await page.getByTestId('device-camera').locator('option').count()) === 3);
    await eventually('the microphones are listed', async () => (await page.getByTestId('device-mic').locator('option').count()) >= 2);

    const cameras = await page.getByTestId('device-camera').locator('option').evaluateAll((o) => o.map((x) => x.value));
    const secondCamera = SENDABLE_CAMERAS(cameras)[1];
    await page.getByTestId('device-camera').selectOption(secondCamera);
    await eventually('the preview moves to that camera', () => page.getByTestId('preview-video').evaluate((v, id) => v.srcObject?.getVideoTracks()[0]?.getSettings().deviceId === id, secondCamera));
    await assertPlaying(page.getByTestId('preview-video'), 'the second camera shows pictures');

    const mics = await page.getByTestId('device-mic').locator('option').evaluateAll((o) => o.map((x) => x.value));
    const secondMic = mics.find((m) => m !== 'default');
    await page.getByTestId('device-mic').selectOption(secondMic);
    await eventually('the meter still moves on that microphone', async () => Number(await page.getByTestId('mic-level').getAttribute('data-level')) > 0.01);

    const outs = await page.getByTestId('device-speaker').locator('option').evaluateAll((o) => o.map((x) => x.value));
    const speaker = outs.find((o) => o !== 'default');
    await page.getByTestId('device-speaker').selectOption(speaker);

    await join(guest, 'Devices');
    await eventually('it published the chosen camera', async () => (await settingOf(page, 'camera')) === secondCamera);
    await eventually('it published the chosen microphone', async () => (await settingOf(page, 'microphone')) === secondMic);

    viewer = await joinByLink(viewerUrl, 'Viewer');
    await assertPlaying(tile(viewer.page, 'Devices').locator('video'), 'the chosen camera reaches someone else');
    // The speaker chosen before anyone else was here applies to whoever arrives.
    await eventually('someone arriving plays through the chosen speaker', () =>
      page.evaluate((id) => {
        const els = [...document.querySelectorAll('[data-streamkit-room-audio] audio')];
        return els.length > 0 && els.every((e) => e.sinkId === id);
      }, speaker),
    );
  });

  it('switches camera, microphone and speaker during a meeting', async () => {
    const { page } = guest;
    await openPanel(page, 'settings');
    // The lists fill a moment after the panel opens: asking the browser for its devices takes a round of the event loop.
    const select = page.getByTestId('settings-camera');
    await eventually('the cameras are listed', async () => (await select.locator('option').count()) === 3);
    const cameras = await select.locator('option').evaluateAll((o) => o.map((x) => x.value));
    const now = await settingOf(page, 'camera');
    const next = SENDABLE_CAMERAS(cameras).find((c) => c !== now);
    await select.selectOption(next);
    await eventually('the camera moves', async () => (await settingOf(page, 'camera')) === next);
    await assertPlaying(tile(viewer.page, 'Devices').locator('video'), 'the new camera reaches someone else, with no renegotiation');

    const micSelect = page.getByTestId('settings-mic');
    await eventually('the microphones are listed', async () => (await micSelect.locator('option').count()) >= 3);
    const mics = await micSelect.locator('option').evaluateAll((o) => o.map((x) => x.value));
    const micNow = await settingOf(page, 'microphone');
    const micNext = mics.find((m) => m !== micNow && m !== 'default');
    await micSelect.selectOption(micNext);
    await eventually('the microphone moves', async () => (await settingOf(page, 'microphone')) === micNext);
    await assertHears(viewer.page, await identityOf(page), 'the new microphone is heard');

    // What this page hears comes out of the chosen speaker.
    const speakers = page.getByTestId('settings-speaker');
    await eventually('the speakers are listed', async () => (await speakers.locator('option').count()) >= 2);
    const outs = await speakers.locator('option').evaluateAll((o) => o.map((x) => ({ value: x.value, label: x.textContent })));
    const out = outs.find((o) => o.value !== 'default');
    assert.ok(out, `no second speaker in ${JSON.stringify(outs)}`);
    await speakers.selectOption(out.value);
    await eventually('the received audio is on that speaker', () =>
      page.evaluate((id) => {
        const els = [...document.querySelectorAll('[data-streamkit-room-audio] audio')];
        return els.length > 0 && els.every((e) => e.sinkId === id);
      }, out.value),
    );
    await page.getByTestId('settings-test-speaker').click();
    await page.getByTestId('settings-tone-result').waitFor();
    assert.match(await page.getByTestId('settings-tone-result').innerText(), new RegExp(`tone on ${out.label}`));
    assert.equal(await page.getByTestId('panel-settings').getByRole('alert').count(), 0);
    await closePanel(page);
  });

  it('tests the chosen speaker with a real tone, and says what was played where', async () => {
    const check = await openGuest(viewerUrl, { from: many });
    const { page } = check;
    await page.getByTestId('prejoin').waitFor();
    await eventually('speakers are listed', async () => (await page.getByTestId('device-speaker').locator('option').count()) >= 2);
    const outs = await page.getByTestId('device-speaker').locator('option').evaluateAll((o) => o.map((x) => ({ value: x.value, label: x.textContent })));
    const out = outs.find((o) => o.value !== 'default');
    await page.getByTestId('device-speaker').selectOption(out.value);
    await page.getByTestId('test-speaker').click();
    const result = page.getByTestId('speaker-tone-result');
    await result.waitFor();
    assert.equal(await result.getAttribute('data-sink'), out.value, 'it played through setSinkId on the chosen device');
    assert.ok(Number(await result.getAttribute('data-peak')) > 0.05, 'there was a real signal');
    assert.ok((await result.innerText()).includes(out.label));
    assert.equal(await page.getByTestId('speaker-tone-error').count(), 0);
  });

  it('says so where a browser cannot choose a speaker, and still plays the tone', async () => {
    const check = await openGuest(viewerUrl, { from: many });
    await check.page.addInitScript(() => {
      delete HTMLMediaElement.prototype.setSinkId;
    });
    await check.page.reload();
    const { page } = check;
    await page.getByTestId('speaker-unsupported').waitFor();
    assert.equal(await page.getByTestId('device-speaker').count(), 0);
    await page.getByTestId('test-speaker').click();
    await page.getByTestId('speaker-tone-result').waitFor();
    assert.match(await page.getByTestId('speaker-tone-result').innerText(), /default/i);
    assert.ok(Number(await page.getByTestId('speaker-tone-result').getAttribute('data-peak')) > 0.05, 'there was a real signal');
  });

  it('shares a screen without sound where the browser refuses a request for it', async () => {
    const check = await openGuest(viewerUrl);
    await check.page.addInitScript(() => {
      const real = MediaDevices.prototype.getDisplayMedia;
      MediaDevices.prototype.getDisplayMedia = function (options) {
        if (options?.audio) return Promise.reject(new TypeError('audio is not supported'));
        return real.call(this, options);
      };
    });
    await check.page.reload();
    await check.page.getByTestId('prejoin').waitFor();
    await join(check, 'No sound');
    await check.page.getByTestId('control-screen').click();
    const shared = viewer.page.getByTestId('tile').filter({ has: viewer.page.getByTestId('tile-name').filter({ hasText: "No sound's screen" }) });
    await shared.waitFor({ timeout: 30000 });
    assert.equal(await check.page.getByTestId('room-notice').count(), 0, 'nothing to complain about');
    await check.page.getByTestId('control-screen').click();
    await eventually('it stops', async () => (await shared.count()) === 0);
  });
});

describe('every screen', () => {
  it('gives every button a name, keeps focus visible, and says state in more than colour', async () => {
    const res = await api('POST', '/meetings', { title: 'Accessibility' });
    assert.equal(res.status, 201, res.text);
    const host = await joinByLink(res.json.host_url, 'Host');
    const guest = await joinByLink(res.json.guest_url, 'Guest');
    const { page } = host;
    await everyoneSees([page, guest.page], 2);

    assert.deepEqual(await unnamedButtons(page), [], 'the meeting');
    for (const name of ['chat', 'people', 'settings', 'host']) {
      await openPanel(page, name);
      assert.deepEqual(await unnamedButtons(page), [], `the ${name} panel`);
      await closePanel(page);
    }
    await page.getByTestId('control-react').click();
    assert.deepEqual(await unnamedButtons(page), [], 'the reaction picker');
    await page.getByTestId('control-react').click();

    // Muted and unmuted read differently without colour: in words, and for a screen reader.
    assert.equal(await page.getByTestId('control-mic').getAttribute('aria-pressed'), 'true');
    assert.match(await page.getByTestId('control-mic').innerText(), /Mute/);
    await page.getByTestId('control-mic').click();
    assert.equal(await page.getByTestId('control-mic').getAttribute('aria-pressed'), 'false');
    assert.match(await page.getByTestId('control-mic').innerText(), /Unmute/);
    await page.getByTestId('control-mic').click();

    // A keyboard reaches the controls, and shows where it is.
    await page.locator('body').click({ position: { x: 1, y: 1 } });
    await page.keyboard.press('Tab');
    const focus = await page.evaluate(() => {
      const el = document.activeElement;
      const style = getComputedStyle(el);
      return { tag: el.tagName, outline: style.outlineStyle, width: parseFloat(style.outlineWidth) };
    });
    assert.ok(focus.tag !== 'BODY', 'Tab reached something');
    assert.ok(focus.outline !== 'none' && focus.width >= 2, `no visible focus: ${JSON.stringify(focus)}`);

    await page.getByTestId('control-leave').click();
    await page.getByTestId('end-screen').waitFor();
    assert.deepEqual(await unnamedButtons(page), [], 'the end screen');
  });

  it('fits a phone, with everything reachable', async () => {
    const res = await api('POST', '/meetings', { title: 'A meeting with quite a long title, to see that it wraps at phone width' });
    assert.equal(res.status, 201, res.text);
    const options = { contextOptions: { viewport: { width: 390, height: 780 }, isMobile: true, hasTouch: true } };
    const phone = await openGuest(res.json.host_url, options);
    const { page } = phone;
    await page.getByTestId('prejoin').waitFor();
    assert.equal(await horizontalOverflow(page), null, 'pre-join');
    await join(phone, 'Phone');
    const other = await joinByLink(res.json.guest_url, 'Other');
    await everyoneSees([page, other.page], 2);
    assert.equal(await horizontalOverflow(page), null, 'the meeting');
    await page.screenshot({ path: `${shotsDir}/meeting-controls-phone.png` });

    // Every control is on the screen, and big enough to press.
    const controls = page.locator('[data-testid^="control-"]:not([data-testid="control-badge"])');
    const count = await controls.count();
    assert.ok(count >= 10, `${count} controls`);
    for (let i = 0; i < count; i++) {
      const box = await controls.nth(i).boundingBox();
      const id = await controls.nth(i).getAttribute('data-testid');
      assert.ok(box && box.x >= 0 && box.x + box.width <= 390 && box.y + box.height <= 780, `${id} is off the screen: ${JSON.stringify(box)}`);
      assert.ok(box.height >= 40 && box.width >= 40, `${id} is too small to press: ${JSON.stringify(box)}`);
    }

    for (const name of ['chat', 'people', 'settings', 'host']) {
      await openPanel(page, name);
      assert.equal(await horizontalOverflow(page), null, `the ${name} panel`);
      const box = await page.getByTestId(`panel-${name}`).boundingBox();
      assert.equal(box.width, 390, `the ${name} panel takes the screen`);
      await page.screenshot({ path: `${shotsDir}/meeting-${name}-phone.png` });
      await closePanel(page);
    }
    await page.getByTestId('control-layout').click();
    assert.equal(await horizontalOverflow(page), null, 'speaker view');
    await page.getByTestId('control-leave').click();
    await page.getByTestId('end-screen').waitFor();
    assert.equal(await horizontalOverflow(page), null, 'the end screen');
  });
});

describe('pages', () => {
  it('threw no errors', () => {
    const errors = everyGuest.flatMap((g) => g.errors);
    assert.deepEqual(errors, []);
  });
});
