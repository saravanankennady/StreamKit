// Takes a screenshot of every page in both themes at desktop and phone width, against real data: two participants
// with fake cameras in a real room. Saved under tests/e2e/.tmp/shots. Also reports any page wider than the screen.
import { mkdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { createKey, eventually, horizontalOverflow, launch, Participants, pageApi, pushRtmp, register, sleep, startReceiver, useNewProject } from './lib/helpers.mjs';
import { DASH, shotsDir, startStack, stopStack, workersRunning } from './lib/stack.mjs';

const only = process.argv.slice(2);
await startStack();
rmSync(shotsDir, { recursive: true, force: true });
mkdirSync(shotsDir, { recursive: true });
const browser = await launch();
const participants = new Participants(browser);
const receiver = await startReceiver();

const VIEWPORTS = [
  { name: 'desktop', width: 1440, height: 900 },
  { name: 'phone', width: 390, height: 844 },
];

try {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const consoleErrors = [];
  page.on('pageerror', (e) => consoleErrors.push(`pageerror on ${page.url()}: ${e.message}`));
  page.on('console', (m) => {
    if (m.type() === 'error' && !/status of (404|401|403|409|412)/.test(m.text())) consoleErrors.push(`console on ${page.url()}: ${m.text()}`);
  });

  const stamp = Date.now();
  await register(page, { email: `shots-${stamp}@example.com`, name: 'Shot Taker', organization: 'Northwind Video' });
  await page.goto(`${DASH}/overview`);
  // First-run screen, before there is any project.
  await page.getByRole('heading', { name: 'Set up your first project' }).waitFor();
  await page.screenshot({ path: join(shotsDir, 'first-run-light-desktop.png') });

  const project = await useNewProject(page, 'production-web', 'production');
  const staging = await useNewProject(page, 'staging-web', 'staging');
  await page.evaluate((id) => localStorage.setItem('streamkit.project', JSON.stringify({ state: { projectId: id }, version: 0 })), project.id);
  const key = await createKey(page, project.id, 'production-backend');
  await createKey(page, project.id, 'ci');

  // Real activity: rooms, participants publishing and subscribing, a webhook with one failure and one success.
  await pageApi(page, 'POST', '/rooms', { project: project.id, body: { name: 'waiting-room', metadata: '{"topic":"standby"}', max_participants: 10 } });
  await pageApi(page, 'POST', '/rooms', { project: project.id, body: { name: 'finished-standup' } });
  await pageApi(page, 'DELETE', '/rooms/finished-standup', { project: project.id });
  await participants.join({ apiKey: key.api_key, apiSecret: key.secret, identity: 'alice', room: 'design-review' });
  await participants.join({ apiKey: key.api_key, apiSecret: key.secret, identity: 'bob', room: 'design-review' });
  const hook = await pageApi(page, 'POST', '/webhooks', { project: project.id, body: { url: receiver.url, api_key_id: key.id } });
  receiver.respondWith = 400;
  await pageApi(page, 'POST', `/webhooks/${hook.json.id}/test`, { project: project.id });
  await sleep(1500);
  receiver.respondWith = 200;
  await pageApi(page, 'POST', `/webhooks/${hook.json.id}/test`, { project: project.id });

  // A real recording, a real failure to start one, and a real stream through an ingress.
  let ingressId = null;
  let ffmpeg = null;
  if (workersRunning()) {
    await eventually('workers', async () => {
      const kinds = (await pageApi(page, 'GET', '/servers')).json.items.map((n) => n.stats.kind);
      return kinds.includes('egress_worker') && kinds.includes('ingress_worker');
    }, { timeout: 30000 });
    const job = await pageApi(page, 'POST', '/egress', { project: project.id, body: { type: 'room_composite', room: 'design-review', layout: 'grid', output: { kind: 'file', format: 'mp4' } } });
    if (job.status === 201) {
      await eventually('the job to start', async () => (await pageApi(page, 'GET', `/egress/${job.json.id}`, { project: project.id })).json.status === 'active', { timeout: 60000 });
      await sleep(5000);
      await pageApi(page, 'POST', `/egress/${job.json.id}/stop`, { project: project.id });
      await eventually('the job to finish', async () => (await pageApi(page, 'GET', `/egress/${job.json.id}`, { project: project.id })).json.status === 'complete', { timeout: 60000 });
    }
    await pageApi(page, 'POST', '/egress', { project: project.id, body: { type: 'participant', room: 'design-review', participant: 'bob', output: { kind: 'file', format: 'mp4' }, stream_urls: ['rtmp://127.0.0.1:1/live/not-a-real-destination'] } });
    const ingress = await pageApi(page, 'POST', '/ingress', { project: project.id, body: { name: 'Main stage', input_type: 'rtmp', room: 'design-review', participant_identity: 'stage', participant_name: 'Main stage' } });
    await pageApi(page, 'POST', '/ingress', { project: project.id, body: { name: 'Backup camera', input_type: 'whip', room: 'design-review', participant_identity: 'backup' } });
    if (ingress.status === 201) {
      ingressId = ingress.json.id;
      ffmpeg = pushRtmp(`${ingress.json.publish_url}/${ingress.json.stream_key}`);
      await eventually('the stream to arrive', async () => (await pageApi(page, 'GET', `/ingress/${ingressId}`, { project: project.id })).json.status === 'publishing', { timeout: 45000 }).catch(() => {});
    }
  }

  // Usage is written every 10 seconds, and the rolling charts need a little history.
  await eventually('usage to be recorded', async () => (await pageApi(page, 'GET', '/analytics/overview', { project: project.id })).json.totals.participant_minutes > 0, { timeout: 30000 });
  await page.goto(`${DASH}/monitor`);
  await sleep(20000);

  const pages = [
    ['overview', '/overview'],
    ['monitor', '/monitor'],
    ['rooms', '/rooms'],
    ['rooms-history', '/rooms?tab=history'],
    ['room-detail', '/rooms/design-review'],
    ['participants', '/participants'],
    ['participant-detail', '/participants/design-review/alice'],
    ['projects', '/projects'],
    ['project-detail', `/projects/${project.id}`],
    ['developer-keys', '/developer'],
    ['developer-tokens', '/developer?tab=tokens'],
    ['developer-settings', '/developer?tab=settings'],
    ['webhooks', '/webhooks'],
    ['webhook-detail', `/webhooks/${hook.json.id}`],
    ['recordings', '/recordings'],
    ['egress', '/egress'],
    ['ingress', '/ingress'],
    ...(ingressId ? [['ingress-detail', `/ingress/${ingressId}`]] : []),
    ['analytics', '/analytics'],
    ['servers', '/servers'],
    ['logs', '/logs'],
    ['logs-audit', '/logs?tab=audit'],
    ['account', '/account'],
    ['docs', '/docs'],
    ['docs-getting-started', '/docs/getting-started'],
    ['docs-api', '/docs/api-reference'],
  ];

  const problems = [];
  for (const theme of ['light', 'dark']) {
    await page.evaluate((t) => localStorage.setItem('streamkit.theme', t), theme);
    for (const vp of VIEWPORTS) {
      await page.setViewportSize({ width: vp.width, height: vp.height });
      for (const [name, path] of pages) {
        if (only.length && !only.includes(name)) continue;
        await page.goto(`${DASH}${path}`);
        await page.waitForLoadState('networkidle', { timeout: 3000 }).catch(() => {});
        await sleep(name === 'monitor' || name === 'overview' ? 2500 : 900);
        const overflow = await horizontalOverflow(page);
        if (overflow) problems.push(`${name} ${theme} ${vp.name} overflows: ${JSON.stringify(overflow)}`);
        await page.screenshot({ path: join(shotsDir, `${name}-${theme}-${vp.name}.png`), fullPage: true });
      }
    }
  }
  ffmpeg?.kill('SIGKILL');

  // Everything that only appears when asked for: dialogs, menus, the secret reveal, playback.
  if (!only.length) {
    const shot = (name, theme, opts = {}) => page.screenshot({ path: join(shotsDir, `${name}-${theme}.png`), ...opts });
    const closeTop = () => page.keyboard.press('Escape');
    for (const theme of ['light', 'dark']) {
      await page.evaluate((t) => localStorage.setItem('streamkit.theme', t), theme);
      await page.setViewportSize({ width: 1280, height: 800 });
      await page.goto(`${DASH}/developer`);
      await page.getByRole('button', { name: 'Create API key' }).click();
      await page.getByRole('dialog').getByLabel('Name').fill('from-the-screenshot-script');
      await page.getByRole('dialog').getByRole('button', { name: 'Create key' }).click();
      await page.getByTestId('reveal-secret').waitFor();
      await shot('overlay-secret-reveal', theme);
      await page.getByRole('dialog').getByLabel('I have saved the secret').click();
      await page.getByRole('dialog').getByRole('button', { name: 'Done' }).click();
      await page.getByTestId('key-row').first().getByRole('button', { name: 'Rotate' }).click();
      await page.getByRole('dialog').waitFor();
      await shot('overlay-rotate', theme);
      await closeTop();

      await page.goto(`${DASH}/rooms`);
      await page.getByRole('button', { name: 'Create room' }).click();
      await page.getByRole('dialog').waitFor();
      await shot('overlay-create-room', theme);
      await closeTop();
      await page.getByRole('button', { name: /Actions for design-review/ }).click();
      await page.getByRole('menuitem', { name: 'Close room' }).waitFor();
      await shot('overlay-room-menu', theme);
      await page.getByRole('menuitem', { name: 'Close room' }).click();
      await page.getByRole('alertdialog').waitFor();
      await shot('overlay-confirm-close', theme);
      await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click();

      await page.keyboard.press('Control+k');
      await page.getByPlaceholder('Page or project name').waitFor();
      await shot('overlay-command-palette', theme);
      await closeTop();
      await page.getByTestId('project-switcher').click();
      await page.getByRole('menuitem', { name: 'Manage projects' }).waitFor();
      await shot('overlay-project-switcher', theme);
      await closeTop();

      await page.goto(`${DASH}/projects/${project.id}`);
      await page.getByRole('button', { name: 'Delete project' }).click();
      await page.getByRole('alertdialog').waitFor();
      await shot('overlay-confirm-delete', theme);
      await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click();

      await page.goto(`${DASH}/webhooks`);
      await page.getByRole('button', { name: 'Add endpoint' }).click();
      await page.getByRole('dialog').getByLabel('All events').click();
      await page.getByRole('dialog').waitFor();
      await shot('overlay-webhook-form', theme);
      await closeTop();

      await page.goto(`${DASH}/webhooks/${hook.json.id}`);
      await page.getByTestId('delivery-row').first().locator('button').first().click();
      await shot('overlay-delivery-expanded', theme, { fullPage: true });

      await page.goto(`${DASH}/monitor`);
      await page.getByRole('button', { name: 'alice', exact: true }).click();
      await page.getByRole('button', { name: 'Show Bitrate as a table' }).click();
      await sleep(1500);
      await shot('overlay-monitor-expanded', theme, { fullPage: true });

      if (workersRunning()) {
        await page.goto(`${DASH}/recordings`);
        const play = page.getByTestId('recording-play').first();
        if (await play.count()) {
          await play.click();
          await page.getByTestId('recording-video').evaluate((v) => new Promise((resolve) => {
            if (v.readyState >= 2) resolve();
            else v.onloadeddata = resolve;
          }));
          await sleep(1000);
          await shot('overlay-video-player', theme);
          await closeTop();
        }
      }
      await page.goto(`${DASH}/developer?tab=tokens`);
      await page.getByLabel('Identity').fill('alice');
      await page.getByLabel('Room', { exact: true }).fill('design-review');
      await page.getByRole('button', { name: 'Generate token' }).click();
      await page.getByTestId('token-result').waitFor();
      await shot('token-result', theme, { fullPage: true });

      // Empty states: a project with nothing in it.
      await page.evaluate((id) => localStorage.setItem('streamkit.project', JSON.stringify({ state: { projectId: id }, version: 0 })), staging.id);
      for (const [name, path] of [['overview', '/overview'], ['monitor', '/monitor'], ['rooms', '/rooms'], ['participants', '/participants'], ['developer', '/developer'], ['webhooks', '/webhooks'], ['recordings', '/recordings'], ['egress', '/egress'], ['ingress', '/ingress'], ['analytics', '/analytics']]) {
        await page.goto(`${DASH}${path}`);
        await sleep(1200);
        await shot(`empty-${name}`, theme, { fullPage: true });
      }
      await page.setViewportSize({ width: 390, height: 844 });
      await page.goto(`${DASH}/rooms`);
      await sleep(900);
      await shot('empty-rooms-phone', theme, { fullPage: true });
      await page.goto(`${DASH}/developer?tab=tokens`);
      await sleep(600);
      await shot('developer-tokens-phone', theme, { fullPage: true });
      await page.setViewportSize({ width: 1280, height: 800 });

      // What a failed request and a slow one look like.
      await page.route('**/api/v1/rooms', (route) => route.fulfill({ status: 500, contentType: 'text/plain', body: 'Internal Server Error' }));
      await page.goto(`${DASH}/rooms`);
      await page.getByText('The server is not responding').first().waitFor({ timeout: 15000 });
      await shot('state-error', theme);
      await page.unroute('**/api/v1/rooms');
      await page.route('**/api/v1/rooms/history*', async (route) => {
        await sleep(4000);
        await route.continue().catch(() => {});
      });
      await page.goto(`${DASH}/rooms?tab=history`);
      await sleep(900);
      await shot('state-loading', theme);
      await page.unroute('**/api/v1/rooms/history*');
      await page.route('**/api/v1/me', (route) => route.fulfill({ status: 500, contentType: 'text/plain', body: 'Internal Server Error' }));
      await page.goto(`${DASH}/overview`);
      await page.getByRole('heading', { name: 'The server is not responding' }).waitFor({ timeout: 20000 });
      await shot('state-server-down', theme);
      await page.unroute('**/api/v1/me');
      await page.evaluate((id) => localStorage.setItem('streamkit.project', JSON.stringify({ state: { projectId: id }, version: 0 })), project.id);
    }
  }

  // Signed-out pages.
  const anon = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const anonPage = await anon.newPage();
  for (const theme of ['light', 'dark']) {
    await anonPage.goto(`${DASH}/login`);
    await anonPage.evaluate((t) => localStorage.setItem('streamkit.theme', t), theme);
    for (const vp of VIEWPORTS) {
      await anonPage.setViewportSize({ width: vp.width, height: vp.height });
      for (const [name, path] of [['login', '/login'], ['register', '/register'], ['forgot', '/forgot-password'], ['reset', '/reset-password?token=abc'], ['verify', '/verify-email?token=abc']]) {
        if (only.length && !only.includes(name)) continue;
        await anonPage.goto(`${DASH}${path}`);
        await sleep(400);
        const overflow = await horizontalOverflow(anonPage);
        if (overflow) problems.push(`${name} ${theme} ${vp.name} overflows: ${JSON.stringify(overflow)}`);
        await anonPage.screenshot({ path: join(shotsDir, `${name}-${theme}-${vp.name}.png`), fullPage: true });
      }
    }
  }

  console.log(problems.length ? `Layout problems:\n${problems.join('\n')}` : 'No page is wider than its screen.');
  console.log(consoleErrors.length ? `Console errors:\n${[...new Set(consoleErrors)].join('\n')}` : 'No console errors.');
} finally {
  await participants.closeAll();
  receiver.close();
  await browser.close();
  await stopStack();
}
