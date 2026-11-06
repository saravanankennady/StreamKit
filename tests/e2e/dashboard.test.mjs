// End-to-end tests of the dashboard: real Chrome, the real server, a real database and real media. Nothing here is
// mocked, with one marked exception: a section at the end answers some media routes with a stub of the documented
// contract, to reach states that are hard to cause for real, such as a failed job.
//
//   cd tests/e2e && npm install && npm test
//
// Tests share one account and run in order, like someone working through the product. See lib/stack.mjs.
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import {
  createKey,
  currentProject,
  eventually,
  horizontalOverflow,
  launch,
  mailLink,
  pageApi,
  Participants,
  PASSWORD,
  pushRtmp,
  register,
  sha256Base64,
  signIn,
  sleep,
  startReceiver,
  useNewProject,
  verifyJwt,
} from './lib/helpers.mjs';
import { BACKEND, DASH, sql, startStack, stopStack, tmpDir, workersRunning } from './lib/stack.mjs';

const stamp = Date.now();
const email = `owner-${stamp}@example.com`;
const NEW_PASSWORD = 'an-entirely-new-password';

let browser;
let ctx;
let page;
const state = { password: PASSWORD, secrets: {} };
const pageErrors = [];

const dialog = () => page.getByRole('dialog');
const alert = () => page.getByRole('alertdialog');

async function chooseOption(label, option, scope = page) {
  await scope.getByLabel(label, { exact: true }).click();
  await page.getByRole('option', { name: option, exact: true }).click();
}

async function selectProject(id) {
  await page.evaluate((id) => localStorage.setItem('streamkit.project', JSON.stringify({ state: { projectId: id }, version: 0 })), id);
}

async function freshProject(name) {
  const p = await useNewProject(page, `${name}-${Date.now()}`);
  await page.goto(`${DASH}/overview`);
  await page.getByRole('heading', { name: 'Overview' }).waitFor();
  return p;
}

before(async () => {
  await startStack();
  browser = await launch();
  ctx = await browser.newContext({ viewport: { width: 1360, height: 900 } });
  page = await ctx.newPage();
  page.on('pageerror', (e) => pageErrors.push(`${page.url()}: ${e.message}`));
});

after(async () => {
  await browser?.close();
  await stopStack();
});

describe('accounts', () => {
  it('registers, and the first account is a platform administrator with a first-run screen', async () => {
    await register(page, { email, name: 'Owner', organization: 'Northwind' });
    await page.getByRole('heading', { name: 'Set up your first project' }).waitFor();
    const me = await pageApi(page, 'GET', '/me');
    assert.equal(me.json.user.is_platform_admin, true);
    assert.equal(me.json.user.email_verified_at, null);
    assert.deepEqual(me.json.projects, []);
  });

  it('shows validation errors from the server on the right field', async () => {
    const other = await browser.newContext();
    const p = await other.newPage();
    await p.goto(`${DASH}/register`);
    await p.getByLabel('Your name').fill('Someone');
    await p.getByLabel('Email').fill('not-an-email');
    await p.getByLabel('Organization').fill('Org');
    await p.getByLabel('Password').fill('long-enough-password');
    await p.getByRole('button', { name: 'Create account' }).click();
    await p.getByText('Must be a valid email address.').waitFor();
    await other.close();
  });

  it('verifies the email address from the link in the mail directory', async () => {
    const link = await mailLink(email, '/verify-email');
    assert.ok(link.startsWith(`${DASH}/verify-email?token=`), link);
    await page.goto(link);
    await page.getByText('Your email address is confirmed').waitFor();
    const me = await pageApi(page, 'GET', '/me');
    assert.ok(me.json.user.email_verified_at, 'the server records it');
    // The link works once.
    await page.goto(link);
    await page.getByText('That link has expired or was already used').waitFor();
  });

  it('logs out, refuses a wrong password, and logs in', async () => {
    await page.goto(`${DASH}/overview`);
    await page.getByRole('button', { name: 'Sign out' }).click();
    await page.waitForURL('**/login');
    await page.goto(`${DASH}/overview`);
    await page.waitForURL('**/login?next=*');

    await page.getByLabel('Email').fill(email);
    await page.getByLabel('Password').fill('this-is-the-wrong-password');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await page.getByText('That email and password do not match.').waitFor();

    await signIn(page, { email });
    assert.equal((await pageApi(page, 'GET', '/me')).json.user.email, email);
  });

  it('resets a forgotten password, which ends sessions and retires the old password', async () => {
    const other = await browser.newContext();
    const p = await other.newPage();
    await p.goto(`${DASH}/forgot-password`);
    await p.getByLabel('Email').fill(email);
    await p.getByRole('button', { name: 'Send reset link' }).click();
    await p.getByText('Check your inbox').waitFor();
    const link = await mailLink(email, '/reset-password');

    await p.goto(link);
    await p.getByLabel('New password').fill(NEW_PASSWORD);
    await p.getByLabel('Type it again').fill('something-different-here');
    await p.getByRole('button', { name: 'Change password' }).click();
    await p.getByText('The two passwords do not match.').waitFor();
    await p.getByLabel('Type it again').fill(NEW_PASSWORD);
    await p.getByRole('button', { name: 'Change password' }).click();
    await p.getByRole('heading', { name: 'Password changed' }).waitFor();
    await other.close();

    // The session this test was already using has ended.
    assert.equal((await pageApi(page, 'GET', '/me')).status, 401);

    await page.goto(`${DASH}/login`);
    await page.getByLabel('Email').fill(email);
    await page.getByLabel('Password').fill(PASSWORD);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await page.getByText('That email and password do not match.').waitFor();

    await signIn(page, { email, password: NEW_PASSWORD });
    state.password = NEW_PASSWORD;
  });
});

describe('projects', () => {
  it('walks the first-run flow: creates a project and shows its real address', async () => {
    await page.goto(`${DASH}/overview`);
    await page.getByRole('heading', { name: 'Set up your first project' }).waitFor();
    await page.getByLabel('Name').fill('first-app');
    await page.getByRole('button', { name: 'Create project' }).click();
    await page.getByRole('heading', { name: /first-app.*created/ }).waitFor();
    await page.getByText('ws://localhost:17880').first().waitFor();

    await page.getByRole('button', { name: 'Create API key' }).click();
    await page.getByText('Copy the secret now').waitFor();
    await page.getByRole('button', { name: 'Go to the console' }).click();
    await page.getByRole('heading', { name: 'Overview' }).waitFor();

    const me = await pageApi(page, 'GET', '/me');
    assert.equal(me.json.projects.length, 1);
    assert.equal(me.json.projects[0].name, 'first-app');
    state.projectId = me.json.projects[0].id;
    assert.equal((await currentProject(page)), state.projectId);
  });

  it('creates, renames, switches to and deletes a project from the shell', async () => {
    await page.getByTestId('project-switcher').click();
    await page.getByRole('menuitem', { name: 'New project' }).click();
    await dialog().getByLabel('Name').fill('second-app');
    await chooseOption('Environment', 'Staging', dialog());
    await dialog().getByLabel('Region').fill('eu-west');
    await dialog().getByRole('button', { name: 'Create project' }).click();
    await page.getByRole('row').filter({ hasText: 'second-app' }).waitFor();
    await eventually('the switcher to change', async () => (await page.getByTestId('project-switcher').innerText()).includes('second-app'));

    const row = page.getByTestId('project-row').filter({ hasText: 'second-app' });
    assert.match(await row.innerText(), /eu-west/);
    assert.match(await row.innerText(), /staging/);

    await row.getByRole('link', { name: 'second-app' }).click();
    await page.getByRole('heading', { name: 'second-app' }).waitFor();
    await page.getByLabel('Name').fill('second-app-renamed');
    await page.getByRole('button', { name: 'Save changes' }).click();
    await page.getByText('Project saved').first().waitFor();
    await eventually('the rename to reach the switcher', async () => (await page.getByTestId('project-switcher').innerText()).includes('second-app-renamed'));
    const id = page.url().split('/').pop();
    assert.equal((await pageApi(page, 'GET', `/projects/${id}`)).json.name, 'second-app-renamed');

    await page.getByRole('button', { name: 'Suspend project' }).click();
    await alert().getByRole('button', { name: 'Suspend project' }).click();
    await page.getByText('second-app-renamed is suspended').waitFor();
    assert.equal((await pageApi(page, 'GET', `/projects/${id}`)).json.status, 'suspended');
    await page.getByRole('button', { name: 'Reactivate project' }).click();
    await page.getByText('Project reactivated').first().waitFor();

    await page.getByRole('button', { name: 'Delete project' }).click();
    assert.equal(await alert().getByRole('button', { name: 'Delete project' }).isDisabled(), true, 'needs the name typed');
    await alert().getByLabel(/Type/).fill('second-app-renamed');
    await alert().getByRole('button', { name: 'Delete project' }).click();
    await page.waitForURL('**/projects');
    await eventually('the project to be gone', async () => (await page.getByTestId('project-row').count()) === 1);
    assert.equal((await pageApi(page, 'GET', `/projects/${id}`)).status, 404);
    await eventually('the switcher to fall back', async () => (await page.getByTestId('project-switcher').innerText()).includes('first-app'));
  });
});

describe('developer settings', () => {
  it('shows an API key secret once, and it is gone after reload', async () => {
    await selectProject(state.projectId);
    await page.goto(`${DASH}/developer`);
    await page.getByRole('button', { name: 'Create API key' }).click();
    await dialog().getByLabel('Name').fill('backend');
    await dialog().getByRole('button', { name: 'Create key' }).click();

    const secretEl = page.getByTestId('reveal-secret');
    await secretEl.waitFor();
    const secret = await secretEl.innerText();
    const apiKey = await page.getByTestId('reveal-api-key').innerText();
    assert.ok(secret.length >= 32, 'a real secret');
    state.secrets[apiKey] = secret;
    state.apiKey = apiKey;

    // It cannot be dismissed by accident.
    assert.equal(await dialog().getByRole('button', { name: 'Done' }).isDisabled(), true);
    await page.keyboard.press('Escape');
    await secretEl.waitFor();
    await dialog().getByLabel('I have saved the secret').click();
    await dialog().getByRole('button', { name: 'Done' }).click();
    await secretEl.waitFor({ state: 'detached' });

    await page.getByTestId('key-row').filter({ hasText: apiKey }).waitFor();
    assert.ok(!(await page.content()).includes(secret), 'not in the page once dismissed');
    await page.reload();
    await page.getByTestId('key-row').filter({ hasText: apiKey }).waitFor();
    assert.ok(!(await page.content()).includes(secret), 'not in the page after reload');
    const listed = await pageApi(page, 'GET', '/keys', { project: state.projectId });
    assert.ok(!listed.text.includes(secret) && !listed.text.includes('"secret"'), 'not from the API');
  });

  it('rotates a key with overlap, and revokes one', async () => {
    const row = page.getByTestId('key-row').filter({ hasText: state.apiKey });
    await row.getByRole('button', { name: 'Rotate' }).click();
    await chooseOption('Old key keeps working for', 'One hour', dialog());
    await dialog().getByRole('button', { name: 'Rotate key' }).click();
    await page.getByRole('heading', { name: 'Your replacement API key' }).waitFor();
    const replacement = await page.getByTestId('reveal-api-key').innerText();
    const replacementSecret = await page.getByTestId('reveal-secret').innerText();
    assert.notEqual(replacement, state.apiKey);
    await dialog().getByLabel('I have saved the secret').click();
    await dialog().getByRole('button', { name: 'Done' }).click();
    state.secrets[replacement] = replacementSecret;

    await row.getByText(/Rotating, ends/).waitFor();
    const keys = (await pageApi(page, 'GET', '/keys', { project: state.projectId })).json.items;
    const old = keys.find((k) => k.api_key === state.apiKey);
    const remaining = Date.parse(old.expires_at) - Date.now();
    assert.ok(remaining > 50 * 60_000 && remaining <= 3_600_000, `overlap of about an hour, got ${remaining}`);

    await row.getByRole('button', { name: 'Revoke' }).click();
    await alert().getByRole('button', { name: 'Revoke key' }).click();
    await row.waitFor({ state: 'detached' });
    await page.getByRole('button', { name: /Show .* revoked or expired/ }).click();
    await row.getByText(/Revoked/).waitFor();
    assert.ok((await pageApi(page, 'GET', '/keys', { project: state.projectId })).json.items.find((k) => k.api_key === state.apiKey).revoked_at);
    state.apiKey = replacement;
  });

  it('generates a token that verifies, carries its grants and works against the API', async () => {
    await page.goto(`${DASH}/developer?tab=tokens`);
    await page.getByLabel('Identity').fill('alice');
    await page.getByLabel('Room', { exact: true }).fill('standup');
    await page.getByLabel('Display name').fill('Alice');
    await page.getByLabel('Lifetime in seconds').fill('600');
    await page.getByLabel('Send data').click();
    await page.getByLabel('List rooms').click();
    await page.getByRole('button', { name: 'Generate token' }).click();
    await page.getByTestId('token-result').waitFor();

    const token = await page.getByTestId('token-result').locator('pre code').first().innerText();
    const claims = verifyJwt(token, state.secrets[state.apiKey]);
    assert.ok(claims, 'signed with the newest key in this project');
    assert.equal(claims.iss, state.apiKey);
    assert.equal(claims.sub, 'alice');
    assert.equal(claims.name, 'Alice');
    assert.equal(claims.video.room, 'standup');
    assert.equal(claims.video.roomJoin, true);
    assert.equal(claims.video.canPublishData, false);
    assert.equal(claims.video.roomList, true);
    assert.ok(Math.abs(claims.exp - Date.now() / 1000 - 600) < 30, 'the lifetime asked for');

    const res = await fetch(`${BACKEND}/api/v1/rooms`, { headers: { Authorization: `Bearer ${token}` } });
    assert.equal(res.status, 200, 'the server accepts it');
  });

  it('explains what to do when a project has no key to sign with', async () => {
    const empty = await freshProject('no-keys');
    await page.goto(`${DASH}/developer?tab=tokens`);
    await page.getByLabel('Identity').fill('alice');
    await page.getByLabel('Room', { exact: true }).fill('standup');
    await page.getByRole('button', { name: 'Generate token' }).click();
    await page.getByText('This project has no API key to sign with').waitFor();
    await pageApi(page, 'DELETE', `/projects/${empty.id}`);
    await selectProject(state.projectId);
  });

  it('saves project settings without losing the other settings', async () => {
    await page.goto(`${DASH}/developer?tab=settings`);
    await page.getByLabel('Origins, one per line').fill('https://app.example.com');
    await page.getByRole('button', { name: 'Save' }).nth(1).click();
    await page.getByText('Allowed origins saved').first().waitFor();

    await page.getByLabel('Empty timeout').fill('120');
    await page.getByLabel('Max participants').fill('25');
    await page.getByRole('button', { name: 'Save' }).first().click();
    await page.getByText('Room defaults saved').first().waitFor();

    await page.getByRole('button', { name: 'Add a server' }).click();
    await page.getByLabel('URLs').fill('turn:turn.example.com:3478');
    await page.getByLabel('Username').fill('user');
    await page.getByLabel('Credential').fill('pass');
    await page.getByRole('button', { name: 'Save' }).last().click();
    await page.getByText('ICE servers saved').first().waitFor();

    const settings = (await pageApi(page, 'GET', `/projects/${state.projectId}`)).json.settings;
    assert.deepEqual(settings.allowed_origins, ['https://app.example.com']);
    assert.equal(settings.empty_timeout, 120);
    assert.equal(settings.max_participants, 25);
    assert.deepEqual(settings.ice_servers, [{ urls: ['turn:turn.example.com:3478'], username: 'user', credential: 'pass' }]);

    // A value the server rejects is reported, and not silently dropped.
    await page.getByLabel('Origins, one per line').fill('not a url');
    await page.getByRole('button', { name: 'Save' }).nth(1).click();
    await page.getByText('Each origin must look like https://app.example.com.').waitFor();

    const cleared = await pageApi(page, 'PATCH', `/projects/${state.projectId}`, { body: { settings: {} } });
    assert.equal(cleared.status, 200);
  });
});

describe('rooms', () => {
  it('shows a working snippet when there are no rooms, and running it creates one', async () => {
    const p = await freshProject('snippet');
    await createKey(page, p.id);
    await page.goto(`${DASH}/rooms`);
    await page.getByRole('heading', { name: 'No rooms are open' }).waitFor();
    assert.match(await page.locator('pre code').innerText(), /http:\/\/localhost:17880\/api\/v1\/rooms/, 'the real address of this project');
    await page.getByRole('button', { name: 'Fill in a token that lasts one hour' }).click();
    await eventually('the token to be filled in', async () => !(await page.locator('pre code').innerText()).includes('$STREAMKIT_TOKEN'));
    const snippet = await page.locator('pre code').innerText();
    const output = spawnSync('bash', ['-c', snippet], { encoding: 'utf8' });
    assert.equal(output.status, 0, output.stderr);
    assert.equal(JSON.parse(output.stdout).name, 'my-room');
    await page.getByTestId('room-row').filter({ hasText: 'my-room' }).waitFor();
    await pageApi(page, 'DELETE', `/projects/${p.id}`);
    await selectProject(state.projectId);
  });

  it('creates, updates and closes a room', async () => {
    await page.goto(`${DASH}/rooms`);
    await page.getByRole('button', { name: 'Create room' }).click();
    await dialog().getByLabel('Name').fill('design review');
    await dialog().getByLabel('Metadata').fill('{"topic":"logo"}');
    await dialog().getByLabel('Max participants').fill('7');
    await dialog().getByRole('button', { name: 'Create room' }).click();
    await page.waitForURL('**/rooms/design%20review');
    await page.getByTestId('room-metadata').getByText('{"topic":"logo"}').waitFor();
    assert.match(await page.getByTestId('room-participants').innerText(), /0 of 7/);
    const created = (await pageApi(page, 'GET', '/rooms/design%20review', { project: state.projectId })).json;
    assert.equal(created.max_participants, 7);

    await page.getByRole('button', { name: 'Edit metadata' }).click();
    await dialog().getByLabel('Metadata').fill('{"topic":"colours"}');
    await dialog().getByRole('button', { name: 'Save metadata' }).click();
    await page.getByTestId('room-metadata').getByText('{"topic":"colours"}').waitFor();
    assert.equal((await pageApi(page, 'GET', '/rooms/design%20review', { project: state.projectId })).json.metadata, '{"topic":"colours"}');

    await page.goto(`${DASH}/rooms`);
    const row = page.getByTestId('room-row').filter({ hasText: 'design review' });
    assert.match(await row.innerText(), new RegExp(created.state === 'creating' ? 'Waiting' : '.'));
    assert.match(await row.innerText(), /0 \/ 7/);
    await row.getByRole('button', { name: 'Actions for design review' }).click();
    await page.getByRole('menuitem', { name: 'Close room' }).click();
    await alert().getByRole('button', { name: 'Close room' }).click();
    await row.waitFor({ state: 'detached' });
    assert.equal((await pageApi(page, 'GET', '/rooms/design%20review', { project: state.projectId })).status, 404);

    await page.getByRole('tab', { name: 'History' }).click();
    const history = page.getByTestId('history-row').filter({ hasText: 'design review' });
    await eventually('the finished room in history', async () => (await history.count()) > 0 && !(await history.first().innerText()).includes('Still open'), { timeout: 15000 });
  });

  it('says plainly when a room has closed', async () => {
    await page.goto(`${DASH}/rooms/nothing-here`);
    await page.getByRole('heading', { name: 'This room is not open' }).waitFor();
  });
});

describe('webhooks', () => {
  let receiver;
  let hookUrlPath;

  before(async () => {
    receiver = await startReceiver();
  });
  after(() => receiver.close());

  it('creates an endpoint with a signing key and selected events', async () => {
    await page.goto(`${DASH}/webhooks`);
    await page.getByRole('heading', { name: 'No endpoints yet' }).waitFor();
    await page.getByRole('button', { name: 'Add endpoint' }).click();
    await dialog().getByLabel('URL').fill(receiver.url);
    await dialog().getByLabel('Signing key').click();
    await page.getByRole('option', { name: new RegExp(state.apiKey) }).click();
    await dialog().getByLabel('All events').click();
    await dialog().getByLabel('room_started').click();
    await dialog().getByRole('button', { name: 'Add endpoint' }).click();
    await page.waitForURL('**/webhooks/*');
    hookUrlPath = new URL(page.url()).pathname;
    await page.getByText('room_started').first().waitFor();
    const id = hookUrlPath.split('/').pop();
    const hook = (await pageApi(page, 'GET', `/webhooks/${id}`, { project: state.projectId })).json;
    assert.equal(hook.url, receiver.url);
    assert.equal(hook.api_key, state.apiKey);
    assert.deepEqual(hook.events, ['room_started']);
  });

  it('delivers a test event with a valid signature, and shows it delivered', async () => {
    await page.getByRole('button', { name: 'Send test event' }).first().click();
    await page.locator('[data-testid="delivery-row"][data-status="delivered"]').waitFor({ timeout: 20000 });
    assert.equal(receiver.received.length, 1);
    const { headers, body } = receiver.received[0];
    assert.equal(headers['content-type'], 'application/webhook+json');
    const claims = verifyJwt(headers.authorization, state.secrets[state.apiKey]);
    assert.ok(claims, 'the signature verifies with the secret of the signing key');
    assert.equal(claims.iss, state.apiKey);
    assert.equal(claims.sha256, sha256Base64(body), 'the signature covers the exact body');
    assert.equal(JSON.parse(body).event, 'room_started');
    assert.match(await page.getByTestId('delivery-response').first().innerText(), /200/);
    assert.match(await page.getByTestId('delivery-summary').innerText(), /1 delivered, 0 failed/);
  });

  it('delivers a real room event to the selected event only', async () => {
    const before = receiver.received.length;
    await pageApi(page, 'POST', '/rooms', { project: state.projectId, body: { name: 'hooked-room' } });
    await eventually('the event to arrive', () => receiver.received.length > before);
    const event = JSON.parse(receiver.received[receiver.received.length - 1].body);
    assert.equal(event.room.name, 'hooked-room');
    await pageApi(page, 'DELETE', '/rooms/hooked-room', { project: state.projectId });
    await sleep(1000);
    assert.equal(receiver.received.length, before + 1, 'room_finished was not selected');
  });

  it('shows a failure with its details, and a retry delivers it', async () => {
    receiver.respondWith = 400;
    const countBefore = receiver.received.length;
    await page.getByRole('button', { name: 'Send test event' }).first().click();
    const failed = page.locator('[data-testid="delivery-row"][data-status="failed"]');
    await failed.waitFor({ timeout: 20000 });
    assert.match(await failed.getByTestId('delivery-response').innerText(), /400/);
    assert.match(await failed.getByTestId('delivery-error').innerText(), /received HTTP 400/);

    await failed.getByRole('button', { name: 'test' }).click();
    await page.getByText('Last error:').waitFor();
    await page.getByText('What we sent').waitFor();

    receiver.respondWith = 200;
    await failed.getByTestId('delivery-retry').click();
    await eventually('the retry to be received', () => receiver.received.length === countBefore + 2);
    await eventually('no failures left', async () => (await page.locator('[data-testid="delivery-row"][data-status="failed"]').count()) === 0);
  });

  it('edits and deletes an endpoint', async () => {
    await page.getByRole('button', { name: 'Edit' }).click();
    await dialog().getByLabel('Enabled').click();
    await dialog().getByRole('button', { name: 'Save endpoint' }).click();
    await page.getByText('Endpoint saved').first().waitFor();
    const id = hookUrlPath.split('/').pop();
    assert.equal((await pageApi(page, 'GET', `/webhooks/${id}`, { project: state.projectId })).json.enabled, false);

    await page.getByRole('button', { name: 'Delete' }).click();
    await alert().getByRole('button', { name: 'Delete endpoint' }).click();
    await page.waitForURL('**/webhooks');
    assert.equal((await pageApi(page, 'GET', `/webhooks/${id}`, { project: state.projectId })).status, 404);
  });

  it('reports a private-address refusal from the server on the URL field', async () => {
    // This server allows private addresses for the tests, so a malformed URL exercises the same path.
    await page.getByRole('button', { name: 'Add endpoint' }).click();
    await dialog().getByLabel('URL').fill('ftp://example.com/hook');
    await dialog().getByRole('button', { name: 'Add endpoint' }).click();
    await dialog().getByText('Must be an http or https URL.').waitFor();
    await dialog().getByRole('button', { name: 'Cancel' }).click();
  });
});

describe('logs and events', () => {
  it('searches, filters by category and level, and says when a category is not written yet', async () => {
    await page.goto(`${DASH}/logs`);
    const rows = page.getByTestId('log-row');
    await rows.first().waitFor();

    await page.getByLabel('Search logs').fill('hooked-room');
    await eventually('search results', async () => (await rows.count()) > 0 && (await rows.allInnerTexts()).every((t) => t.includes('hooked-room')));
    assert.match(page.url(), /q=hooked-room/, 'the filter is in the address, to share');
    const viaApi = (await pageApi(page, 'GET', '/logs?q=hooked-room', { project: state.projectId })).json.items.length;
    assert.equal(await rows.count(), viaApi, 'the same rows the API returns');

    await page.getByLabel('Search logs').fill('nothing-matches-this-at-all');
    await page.getByText('No events match these filters').waitFor();
    await page.getByLabel('Search logs').fill('');

    await chooseOption('Category', 'Participant');
    await page.getByRole('heading', { name: /No events/ }).waitFor();
    await chooseOption('Category', 'Room');
    await eventually('room events only', async () => (await rows.count()) > 0 && (await rows.evaluateAll((els) => els.every((e) => e.dataset.category === 'room'))));

    await chooseOption('Category', 'Webrtc');
    await page.getByText('The server does not write webrtc events yet').waitFor();
    await chooseOption('Category', 'All categories');

    await chooseOption('Level', 'Error');
    await page.getByRole('heading', { name: 'No events match these filters' }).waitFor();
    await chooseOption('Level', 'All levels');
  });

  it('pages through older events', async () => {
    for (let i = 0; i < 28; i++) {
      await pageApi(page, 'POST', '/rooms', { project: state.projectId, body: { name: `paging-${i}` } });
      await pageApi(page, 'DELETE', `/rooms/paging-${i}`, { project: state.projectId });
    }
    await eventually('enough events', async () => (await pageApi(page, 'GET', '/logs?limit=60', { project: state.projectId })).json.items.length >= 55);
    await page.goto(`${DASH}/logs`);
    await page.getByTestId('log-row').first().waitFor();
    await eventually('the first page', async () => (await page.getByTestId('log-row').count()) === 50);
    await page.getByRole('switch', { name: 'Refresh every 3 seconds' }).click();
    const first = await page.getByTestId('log-message').first().innerText();
    await page.getByTestId('logs-more').click();
    await eventually('the second page', async () => (await page.getByTestId('log-row').count()) > 50);
    assert.equal(await page.getByTestId('log-message').first().innerText(), first, 'older events go after the newer ones');
  });

  it('shows the audit log to an admin, with who did what', async () => {
    await page.goto(`${DASH}/logs?tab=audit`);
    const row = page.locator('[data-testid="audit-row"][data-action="room.created"]').first();
    await row.waitFor();
    assert.match(await row.innerText(), new RegExp(email));
    // The rooms made for the paging test pushed older entries to later pages.
    await page.getByRole('button', { name: 'Load older entries' }).click();
    await page.locator('[data-testid="audit-row"][data-action="api_key.revoked"]').first().waitFor();
  });
});

describe('sessions', () => {
  it('lists sessions, and ending one signs that browser out', async () => {
    const other = await browser.newContext();
    const p = await other.newPage();
    await signIn(p, { email, password: state.password });

    await page.goto(`${DASH}/account`);
    const rows = page.getByTestId('session-row');
    await eventually('two sessions', async () => (await rows.count()) === 2);
    await page.locator('[data-testid="session-row"][data-current="true"]').getByText('This browser').waitFor();
    await page.locator('[data-testid="session-row"][data-current="false"]').getByRole('button', { name: 'End' }).click();
    await alert().getByRole('button', { name: 'End session' }).click();
    await eventually('one session', async () => (await rows.count()) === 1);

    assert.equal((await pageApi(p, 'GET', '/me')).status, 401);
    await p.goto(`${DASH}/overview`);
    await p.waitForURL('**/login**');
    await other.close();
  });
});

describe('theme', () => {
  it('toggles between light and dark, and remembers across reloads', async () => {
    await page.goto(`${DASH}/overview`);
    await page.getByRole('heading', { name: 'Overview' }).waitFor();
    const theme = () => page.evaluate(() => ({ attr: document.documentElement.dataset.theme, bg: getComputedStyle(document.body).backgroundColor }));
    await page.getByRole('button', { name: 'Theme' }).click();
    await page.getByRole('menuitem', { name: 'Dark' }).click();
    const dark = await theme();
    assert.equal(dark.attr, 'dark');
    await page.reload();
    await page.getByRole('heading', { name: 'Overview' }).waitFor();
    assert.equal((await theme()).attr, 'dark', 'still dark after reload');

    await page.getByRole('button', { name: 'Theme' }).click();
    await page.getByRole('menuitem', { name: 'Light' }).click();
    const light = await theme();
    assert.equal(light.attr, 'light');
    assert.notEqual(light.bg, dark.bg, 'the background really changed');
  });
});

describe('live data, with real participants', () => {
  let participants;
  let alice;
  let bob;
  let key;
  let project;
  const ROOM = 'live-room';
  const liveSockets = [];

  before(async () => {
    participants = new Participants(browser);
    project = await freshProject('live');
    key = await createKey(page, project.id, 'live');
    page.on('websocket', (ws) => liveSockets.push(ws.url()));
    await page.goto(`${DASH}/overview`);
    await page.getByRole('heading', { name: 'Overview' }).waitFor();
  });

  after(async () => {
    await participants.closeAll();
    await selectProject(state.projectId);
  });

  it('shows figures from the backend on the overview before anyone joins', async () => {
    const me = await pageApi(page, 'GET', '/me');
    assert.equal(await page.getByTestId('tile-projects').innerText(), String(me.json.projects.length));
    await eventually('the rooms tile', async () => (await page.getByTestId('tile-rooms').innerText()) === '0');
    assert.equal(await page.getByTestId('tile-participants').innerText(), '0');
    await page.getByTestId('tile-latency').getByText('Not measured').waitFor();

    const snap = (await pageApi(page, 'GET', '/monitor', { project: project.id })).json;
    const memPercent = (snap.node.memory_used / snap.node.memory_total) * 100;
    const shownMem = Number((await page.getByTestId('tile-memory').innerText()).replace('%', ''));
    assert.ok(Math.abs(shownMem - memPercent) < 10, `memory ${shownMem} against ${memPercent}`);
    assert.match(await page.getByTestId('tile-cpu').innerText(), /^\d+%$/);
    await page.getByText(snap.node.node_id).first().waitFor();
    await page.getByTestId('overview-verdict').getByText('All good').waitFor();
  });

  it('follows the WebSocket, not polling', async () => {
    await eventually('the live status', async () => (await page.getByTestId('live-status').getAttribute('data-status')) === 'live');
    assert.ok(liveSockets.some((u) => u.startsWith('ws://localhost:17880/api/v1/monitor/live?project_id=')), `sockets: ${liveSockets}`);
  });

  it('shows a real participant in the monitor, with non-zero bitrate and real video', async () => {
    alice = await participants.join({ apiKey: key.api_key, apiSecret: key.secret, identity: 'alice', room: ROOM });
    await page.goto(`${DASH}/monitor`);
    const row = page.locator('[data-testid="monitor-participant"][data-identity="alice"]');
    await row.waitFor({ timeout: 20000 });
    await eventually('a non-zero bitrate', async () => Number(await row.getAttribute('data-bitrate-in')) > 10_000, { timeout: 20000 });
    assert.match(await row.getByTestId('monitor-bitrate-in').innerText(), /(k|M)bps/);
    assert.equal(await page.getByTestId('monitor-participants').innerText(), '1');
    assert.equal(await page.getByTestId('monitor-rooms').innerText(), '1');
    await eventually('audio and video streams', async () => (await page.getByTestId('monitor-audio').innerText()) === '1' && (await page.getByTestId('monitor-video').innerText()) === '1');

    await row.getByRole('button', { name: 'alice', exact: true }).click();
    const video = page.locator('[data-testid="monitor-track"][data-kind="video"]');
    await eventually('a real frame rate', async () => /[1-9]\d* fps/.test(await video.getByTestId('monitor-fps').innerText()), { timeout: 20000 });
    assert.match(await video.getByTestId('monitor-resolution').innerText(), /\d+×\d+/);

    // The API agrees with the screen, within a second's difference.
    const api = (await pageApi(page, 'GET', `/participants?room=${ROOM}`, { project: project.id })).json.items[0];
    assert.ok(api.bitrate_in > 0);
    assert.equal(api.identity, 'alice');
    const chart = page.getByTestId('chart-bitrate').locator('.recharts-line-curve').first();
    await chart.waitFor({ state: 'attached' });
    assert.ok((await chart.getAttribute('d')).length > 10, 'the rolling chart has drawn a line');
  });

  it('measures RTT once someone subscribes', async () => {
    bob = await participants.join({ apiKey: key.api_key, apiSecret: key.secret, identity: 'bob', room: ROOM });
    await eventually('two participants', async () => (await page.getByTestId('monitor-participants').innerText()) === '2', { timeout: 15000 });
    const row = page.locator('[data-testid="monitor-participant"][data-identity="bob"]');
    await eventually('an RTT reading', async () => /\d ms/.test(await row.innerText()), { timeout: 30000 });
  });

  it('counts them on the overview, and in the status line', async () => {
    await page.goto(`${DASH}/overview`);
    await eventually('participants on the overview', async () => (await page.getByTestId('tile-participants').innerText()) === '2');
    assert.equal(await page.getByTestId('statusline-participants').innerText(), '2');
    assert.equal(await page.getByTestId('tile-connections').innerText(), '4');
    await eventually('non-zero bandwidth', async () => /(k|M)bps/.test(await page.getByTestId('tile-bitrate-in').innerText()));
    await eventually('a latency reading', async () => /ms$/.test(await page.getByTestId('tile-latency').innerText()), { timeout: 30000 });

    await eventually('streaming minutes, from recorded usage', async () => {
      const api = (await pageApi(page, 'GET', '/analytics/overview', { project: project.id })).json.totals.participant_minutes;
      return api > 0;
    }, { timeout: 30000 });
    await page.reload();
    await eventually('the tile to show them', async () => Number((await page.getByTestId('tile-minutes').innerText()).replace(/,/g, '')) > 0);
  });

  it('mutes a real participant from the UI, and they are told', async () => {
    await page.goto(`${DASH}/participants`);
    const row = page.locator('[data-testid="participant-row"][data-identity="alice"]');
    await row.waitFor();
    await page.getByText('Muting always works. Unmuting from here usually does not.').waitFor();
    await row.getByRole('button', { name: 'Mute audio of alice' }).click();
    await page.getByText('Track muted').first().waitFor();

    await eventually('the publisher to hear about it', () =>
      alice.evaluate(() => {
        const pub = [...window.harness.room.localParticipant.audioTrackPublications.values()][0];
        return pub?.isMuted === true;
      }),
    );
    await row.getByRole('button', { name: 'Unmute audio of alice' }).waitFor();
    const api = (await pageApi(page, 'GET', `/participants/${ROOM}/alice`, { project: project.id })).json;
    assert.equal(api.audio_muted, true);
  });

  it('says plainly when the server refuses to unmute', async () => {
    const row = page.locator('[data-testid="participant-row"][data-identity="alice"]');
    await row.getByRole('button', { name: 'Unmute audio of alice' }).click();
    await page.getByText('Could not unmute').first().waitFor();
    await page.getByText("Turning on someone's track from here is disabled on this server. Ask them to unmute.").first().waitFor();
    assert.equal((await pageApi(page, 'GET', `/participants/${ROOM}/alice`, { project: project.id })).json.audio_muted, true);
  });

  it('inspects tracks and updates permissions on a real participant', async () => {
    await page.goto(`${DASH}/participants/${ROOM}/alice`);
    await page.getByRole('heading', { name: 'Published tracks' }).waitFor();
    await eventually('both tracks', async () => (await page.getByTestId('track-row').count()) === 2);
    await page.locator('[data-testid="track-row"][data-kind="audio"]').getByText('Muted, unmute').waitFor();
    assert.match(await page.locator('[data-testid="track-row"][data-kind="video"]').innerText(), /\d+×\d+/);

    await page.getByLabel('Send data messages').click();
    await page.getByRole('button', { name: 'Save permissions' }).click();
    await page.getByText('Permissions updated').first().waitFor();
    await eventually('the participant to be told', () => alice.evaluate(() => window.harness.room.localParticipant.permissions?.canPublishData === false));
    // This route reads the monitor's latest sample, which can be a second old.
    await eventually('the API to agree', async () => {
      const api = (await pageApi(page, 'GET', `/participants/${ROOM}/alice`, { project: project.id })).json;
      return !api.permission.can_publish_data && api.permission.can_publish === true;
    });
  });

  it('removes a real participant from the UI, and they are disconnected', async () => {
    await page.goto(`${DASH}/participants`);
    const row = page.locator('[data-testid="participant-row"][data-identity="alice"]');
    await row.getByRole('button', { name: 'Remove alice' }).click();
    await alert().getByRole('button', { name: 'Remove participant' }).click();
    await eventually('alice to be disconnected', () => alice.evaluate(() => window.harness.connectionState() === 'disconnected'));
    await row.waitFor({ state: 'detached', timeout: 10000 });
    assert.equal((await pageApi(page, 'GET', `/participants/${ROOM}/alice`, { project: project.id })).status, 404);
    await page.locator('[data-testid="participant-row"][data-identity="bob"]').waitFor();
    assert.ok(bob);
  });

  it('shows the same events in the log', async () => {
    await page.goto(`${DASH}/logs?q=alice`);
    await page.getByText('alice joined').first().waitFor();
    await page.getByText('alice published CAMERA').first().waitFor();
  });

  it('shows recorded usage in analytics, matching the API', async () => {
    await page.goto(`${DASH}/analytics`);
    await page.getByTestId('chart-streaming').locator('.recharts-bar-rectangle').first().waitFor({ state: 'attached', timeout: 15000 });
    await chooseOption('Range', 'Last 24 hours');
    await page.getByTestId('chart-concurrent').locator('.recharts-bar-rectangle').first().waitFor({ state: 'attached', timeout: 15000 });
    const api = (await pageApi(page, 'GET', '/analytics/overview', { project: project.id })).json;
    assert.ok(api.totals.bytes_in > 0);
    await page.getByTestId('analytics-data-in').filter({ hasText: /MB|KB|GB/ }).waitFor();
    assert.match(await page.getByTestId('analytics-failures').innerText(), /^\d+$/);

    await page.getByRole('button', { name: 'Show Concurrent participants as a table' }).click();
    await page.getByRole('table', { name: 'Concurrent participants' }).waitFor();
    await chooseOption('Group by', 'Monthly');
    const monthly = (await pageApi(page, 'GET', '/analytics/usage?interval=month', { project: project.id })).json.items;
    assert.equal(monthly.length, 1);
  });

  it('closing a room from the UI disconnects the rest', async () => {
    await page.goto(`${DASH}/rooms/${ROOM}`);
    await page.getByRole('button', { name: 'Close room' }).click();
    await alert().getByRole('button', { name: 'Close room' }).click();
    await page.waitForURL('**/rooms');
    await eventually('bob to be disconnected', () => bob.evaluate(() => window.harness.connectionState() === 'disconnected'));
  });
});

describe('project isolation between two users', () => {
  let ctxB;
  let pageB;
  const emailB = `other-${stamp}@example.com`;

  before(async () => {
    ctxB = await browser.newContext({ viewport: { width: 1360, height: 900 } });
    pageB = await ctxB.newPage();
    await selectProject(state.projectId);
    await pageApi(page, 'POST', '/rooms', { project: state.projectId, body: { name: 'private' } });
  });
  after(async () => {
    await ctxB.close();
  });

  it('gives a second user nothing of the first user\'s', async () => {
    await register(pageB, { email: emailB, name: 'Other', organization: 'Other org' });
    await pageB.getByRole('heading', { name: 'Set up your first project' }).waitFor();
    const me = (await pageApi(pageB, 'GET', '/me')).json;
    assert.equal(me.user.is_platform_admin, false, 'only the first account administers the server');
    assert.deepEqual(me.projects, []);
    assert.ok(!JSON.stringify(me).includes(state.projectId));
  });

  it('refuses every route with the other user\'s project ID', async () => {
    for (const path of ['/rooms', '/keys', '/webhooks', '/logs', '/analytics/overview', '/monitor', '/audit', '/participants']) {
      const res = await pageApi(pageB, 'GET', path, { project: state.projectId });
      assert.equal(res.status, 404, `${path}: ${res.text}`);
    }
    assert.equal((await pageApi(pageB, 'GET', `/projects/${state.projectId}`)).status, 404);
  });

  it('ignores another project\'s ID in the browser, and does not show it', async () => {
    const own = await useNewProject(pageB, 'b-project');
    await selectOther(pageB, state.projectId);
    await pageB.goto(`${DASH}/rooms`);
    await eventually('its own project', async () => (await pageB.getByTestId('project-switcher').innerText()).includes('b-project'));
    await pageB.getByRole('heading', { name: 'No rooms are open' }).waitFor();
    assert.ok(!(await pageB.content()).includes('private'), 'no trace of the other room');
    await pageB.goto(`${DASH}/projects/${state.projectId}`);
    await pageB.getByRole('heading', { name: 'You cannot see this project' }).waitFor();

    // The same room name in another project is a different room.
    const mine = await pageApi(pageB, 'POST', '/rooms', { project: own.id, body: { name: 'private' } });
    const theirs = await pageApi(page, 'GET', '/rooms/private', { project: state.projectId });
    assert.notEqual(mine.json.sid, theirs.json.sid);
  });

  it('cannot see the live feed of another project', async () => {
    const code = await pageB.evaluate(
      (id) =>
        new Promise((resolve) => {
          const ws = new WebSocket(`ws://localhost:17880/api/v1/monitor/live?project_id=${id}`);
          ws.onopen = () => resolve('open');
          ws.onerror = () => resolve('refused');
        }),
      state.projectId,
    );
    assert.equal(code, 'refused');
  });

  it('asks a new user to confirm their email, and sends it again', async () => {
    await pageB.goto(`${DASH}/overview`);
    await pageB.getByText(`Confirm ${emailB}`).waitFor();
    const before = await mailLink(emailB, '/verify-email');
    await pageB.getByRole('button', { name: 'Send the email again' }).click();
    await pageB.getByText('Confirmation email sent').first().waitFor();
    const after = await eventually('a new mail', async () => {
      const link = await mailLink(emailB, '/verify-email');
      return link !== before ? link : null;
    });
    assert.ok(after);
  });

  it('only lets platform administrators drain a server', async () => {
    await pageB.goto(`${DASH}/servers`);
    // A non-administrator's card has no figures in it, so it is present but has no height.
    await pageB.getByTestId('server-card').first().waitFor({ state: 'attached' });
    await pageB.getByText('Only platform administrators can see machine details and drain a node').waitFor();
    assert.equal(await pageB.getByRole('button', { name: 'Drain' }).count(), 0);
    // Another organization's user must not learn the operator's hosts or load.
    for (const label of ['Host', 'Address', 'CPU', 'Memory', 'Active rooms', 'Participants']) {
      assert.equal(await pageB.getByTestId('server-card').first().getByText(label, { exact: true }).count(), 0, label);
    }
    assert.equal(await pageB.getByText('Host', { exact: true }).count(), 0);
    const node = (await pageApi(pageB, 'GET', '/servers')).json.items[0].node_id;
    assert.equal((await pageApi(pageB, 'POST', `/servers/${node}/drain`, { body: { draining: true } })).status, 403);
  });
});

describe('roles', () => {
  it('shows a member what they can do, and explains what they cannot', async () => {
    // The API has no route for inviting people yet, so the membership goes straight into the database.
    const memberEmail = `member-${stamp}@example.com`;
    const c = await browser.newContext({ viewport: { width: 1360, height: 900 } });
    const p = await c.newPage();
    await register(p, { email: memberEmail, name: 'Member', organization: 'Member org' });
    const org = (await pageApi(page, 'GET', `/projects/${state.projectId}`)).json.org_id;
    await sql(`INSERT INTO memberships (org_id, user_id, role) SELECT $1, id, 'member' FROM users WHERE email = $2`, [org, memberEmail]);

    await p.goto(`${DASH}/overview`);
    await p.getByTestId('project-switcher').waitFor();
    assert.match(await p.getByTestId('project-switcher').innerText(), /first-app/);

    await p.goto(`${DASH}/developer`);
    await p.getByText('Only owners and admins can see API keys').waitFor();
    await p.goto(`${DASH}/developer?tab=settings`);
    await p.getByText('You can look, but not change these').waitFor();
    assert.equal(await p.getByLabel('Origins, one per line').isDisabled(), true);
    await p.goto(`${DASH}/logs?tab=audit`);
    await p.getByText('Only owners and admins can read the audit log').waitFor();
    await p.goto(`${DASH}/webhooks`);
    await p.getByText('You can look, but not change these').waitFor();
    assert.equal(await p.getByRole('button', { name: 'Add endpoint' }).count(), 0);
    await p.goto(`${DASH}/projects/${state.projectId}`);
    await p.getByText('You can look, but not change this project').waitFor();
    assert.equal(await p.getByRole('button', { name: 'Delete project' }).count(), 0);

    // Members can operate rooms.
    await p.goto(`${DASH}/rooms`);
    await p.getByRole('button', { name: 'Create room' }).click();
    await p.getByRole('dialog').getByLabel('Name').fill('member-room');
    await p.getByRole('dialog').getByRole('button', { name: 'Create room' }).click();
    await p.waitForURL('**/rooms/member-room');
    assert.equal((await pageApi(page, 'GET', '/rooms/member-room', { project: state.projectId })).status, 200);
    await c.close();
  });
});

async function selectOther(p, id) {
  await p.evaluate((id) => localStorage.setItem('streamkit.project', JSON.stringify({ state: { projectId: id }, version: 0 })), id);
}

describe('servers', () => {
  it('shows real node figures, and drains and resumes after confirmation', async () => {
    await selectProject(state.projectId);
    await page.goto(`${DASH}/servers`);
    const card = page.getByTestId('server-card').first();
    await card.waitFor();
    const nodes = (await pageApi(page, 'GET', '/servers')).json.items;
    const node = nodes[nodes.length - 1];
    assert.equal(await page.getByTestId('node-version').first().innerText(), node.version);
    await page.getByText('Accepting sessions').first().waitFor();

    await page.getByTestId('node-drain').first().click();
    await alert().getByRole('button', { name: 'Cancel' }).click();
    assert.equal(await card.getAttribute('data-draining'), 'false', 'cancel does nothing');

    await page.getByTestId('node-drain').first().click();
    await alert().getByRole('button', { name: 'Drain node' }).click();
    await page.getByText('Draining').first().waitFor();
    await eventually('the API to agree', async () => (await pageApi(page, 'GET', '/servers')).json.items.some((n) => n.draining));
    await eventually('the status line to say so', async () => (await page.getByTestId('status-line').innerText()).includes('Draining'), { timeout: 15000 });

    await page.getByTestId('node-resume').first().click();
    await alert().getByRole('button', { name: 'Resume node' }).click();
    await eventually('the node to resume', async () => !(await pageApi(page, 'GET', '/servers')).json.items.some((n) => n.draining));
  });
});

describe('a worker that reports a problem', () => {
  const reason = 'the working directory has only 12 MB free';
  const nodeId = 'EW_says-it-is-unwell';

  before(async () => {
    await sql(`INSERT INTO servers (node_id, version, stats) VALUES ($1, 'test', $2)`, [nodeId, JSON.stringify({
      node_id: nodeId, kind: 'egress_worker', healthy: false, unhealthy_reason: reason, num_cpu: 4, memory_used: 1, memory_total: 2, cpu_percent: 1, uptime_seconds: 5, max_jobs: 2, active_jobs: 0,
    })]);
  });

  after(async () => {
    await sql(`DELETE FROM servers WHERE node_id = $1`, [nodeId]);
  });

  it('shows what it says is wrong to an operator', async () => {
    await page.goto(`${DASH}/servers`);
    const shown = page.getByTestId('unhealthy-reason');
    await shown.waitFor();
    assert.equal(await shown.innerText(), reason);
    await page.getByText('Unhealthy').first().waitFor();
    await page.getByText('It takes no new jobs until this is fixed.').first().waitFor();
  });
});

describe('getting around', () => {
  it('reaches any page from the keyboard', async () => {
    await page.goto(`${DASH}/overview`);
    await page.getByRole('heading', { name: 'Overview' }).waitFor();
    await page.keyboard.press('Control+k');
    await page.getByPlaceholder('Page or project name').fill('webhooks');
    await page.keyboard.press('Enter');
    await page.waitForURL('**/webhooks');
  });

  it('keeps every page inside a phone screen', async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    const paths = ['/overview', '/monitor', '/rooms', '/rooms?tab=history', '/participants', '/projects', `/projects/${state.projectId}`, '/developer', '/developer?tab=tokens', '/developer?tab=settings', '/webhooks', '/recordings', '/egress', '/ingress', '/analytics', '/servers', '/logs', '/logs?tab=audit', '/account', '/docs', '/docs/getting-started', '/docs/api-reference', '/docs/webhooks'];
    const problems = [];
    for (const path of paths) {
      await page.goto(`${DASH}${path}`);
      await sleep(500);
      const overflow = await horizontalOverflow(page);
      if (overflow) problems.push(`${path}: ${JSON.stringify(overflow)}`);
    }
    await page.setViewportSize({ width: 1360, height: 900 });
    assert.deepEqual(problems, []);
  });

  it('navigates on a phone through the menu', async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${DASH}/overview`);
    await page.getByRole('button', { name: 'Open navigation' }).click();
    await page.getByRole('dialog').getByRole('link', { name: 'Rooms' }).click();
    await page.waitForURL('**/rooms');
    await page.setViewportSize({ width: 1360, height: 900 });
  });
});

describe('documentation', () => {
  it('renders the API reference from the server\'s own spec', async () => {
    await page.goto(`${DASH}/docs/api-reference`);
    const operations = page.getByTestId('api-operation');
    await operations.first().waitFor();
    const spec = await (await fetch(`${BACKEND}/api/v1/openapi.yaml`)).text();
    const routes = [...spec.matchAll(/^ {2}\/[^\s:]+:/gm)].length;
    assert.ok((await operations.count()) >= routes, `${await operations.count()} operations for ${routes} paths`);
    await page.locator('[data-testid="api-operation"][data-path="/rooms"][data-method="post"]').locator('summary').click();
    await page.locator('[data-testid="api-operation"][data-path="/rooms"][data-method="post"]').getByText('Access token').waitFor();
    await page.getByLabel('Filter routes').fill('webhooks/{id}/test');
    await eventually('the filter', async () => (await operations.count()) === 1);
  });

  it('opens every page, signed out, with no broken links inside the docs', async () => {
    const anon = await browser.newContext();
    const p = await anon.newPage();
    await p.goto(`${DASH}/docs`);
    const hrefs = await p.locator('main a[href^="/docs/"]').evaluateAll((els) => els.map((e) => e.getAttribute('href')));
    assert.ok(hrefs.length >= 14, `${hrefs.length} pages`);
    const seen = new Set();
    for (const href of hrefs) {
      await p.goto(`${DASH}${href}`);
      await p.getByRole('heading', { level: 1 }).first().waitFor();
      const inner = await p.locator('article a[href^="/docs/"]').evaluateAll((els) => els.map((e) => e.getAttribute('href')));
      for (const link of inner) {
        if (seen.has(link)) continue;
        seen.add(link);
        assert.ok(hrefs.includes(link), `${href} links to ${link}, which does not exist`);
      }
    }
    // The snippets say where to connect, and with no session they cannot know.
    await p.goto(`${DASH}/docs/getting-started`);
    await p.getByText('as a placeholder').waitFor();
    await anon.close();
  });

  it('shows signed-in people their real project address in snippets', async () => {
    await page.goto(`${DASH}/docs/getting-started`);
    await page.locator('code').filter({ hasText: 'ws://localhost:17880' }).first().waitFor();
    await page.getByText('Snippets on this page use the address of your project').waitFor();
  });
});

describe('media services', () => {
  // Each page must say the service is not running if its routes 404, and must work when they exist.
  const pages = [
    { path: '/recordings', probe: '/recordings', name: 'recording' },
    { path: '/egress', probe: '/egress', name: 'egress' },
    { path: '/ingress', probe: '/ingress', name: 'ingress' },
  ];

  for (const { path, probe, name } of pages) {
    it(`${name} is honest about the real server`, async () => {
      await selectProject(state.projectId);
      const res = await pageApi(page, 'GET', probe, { project: state.projectId });
      await page.goto(`${DASH}${path}`);
      if (res.status === 404) {
        await page.getByRole('heading', { name: `The ${name} service is not running` }).waitFor();
        assert.equal(await page.getByRole('button', { name: /Start|Create ingress/ }).count(), 0, 'nothing to click that cannot work');
      } else {
        await page.getByRole('heading', { level: 1 }).first().waitFor();
        await sleep(500);
        assert.equal(await page.getByRole('heading', { name: `The ${name} service is not running` }).count(), 0);
      }
    });
  }

  describe('with real workers', () => {
    let participants;
    let project;
    let key;
    const ROOM = 'record-me';

    before(async () => {
      if (!workersRunning()) return;
      participants = new Participants(browser);
      project = await freshProject('media');
      key = await createKey(page, project.id, 'media');
      await eventually('both workers to report in', async () => {
        const kinds = (await pageApi(page, 'GET', '/servers')).json.items.map((n) => n.stats.kind);
        return kinds.includes('egress_worker') && kinds.includes('ingress_worker');
      }, { timeout: 30000 });
    });

    after(async () => {
      await participants?.closeAll();
      if (workersRunning()) await selectProject(state.projectId);
    });

    it('shows workers on the Servers page, and on the overview', async (t) => {
      if (!workersRunning()) return t.skip('the workers did not build');
      await page.goto(`${DASH}/servers`);
      await page.locator('[data-testid="server-card"][data-kind="egress_worker"]').waitFor();
      await page.locator('[data-testid="server-card"][data-kind="ingress_worker"]').waitFor();
      await page.getByText('Accepting jobs').waitFor();
      await page.getByText('Accepting streams').waitFor();
      await page.goto(`${DASH}/overview`);
      assert.equal(await page.getByText('No worker').count(), 0);
      await page.getByText('1 worker').first().waitFor();
    });

    it('records a real room, plays the file in a video element, downloads it and deletes it', async (t) => {
      if (!workersRunning()) return t.skip('the workers did not build');
      await participants.join({ apiKey: key.api_key, apiSecret: key.secret, identity: 'alice', room: ROOM });
      await page.goto(`${DASH}/recordings`);
      await page.getByRole('heading', { name: 'No recordings yet' }).waitFor();
      assert.equal(await page.getByText('No recording worker is running').count(), 0);

      await page.getByRole('button', { name: 'Start recording' }).first().click();
      await dialog().getByLabel('Room', { exact: true }).fill(ROOM);
      await dialog().getByRole('button', { name: 'Start recording' }).click();
      const row = page.getByTestId('recording-row').first();
      await row.waitFor();
      await page.locator('[data-testid="recording-row"][data-status="active"]').waitFor({ timeout: 60000 });
      await sleep(5000);
      await row.getByTestId('recording-stop').click();
      await page.locator('[data-testid="recording-row"][data-status="complete"]').waitFor({ timeout: 60000 });
      assert.match(await row.innerText(), /\d+(\.\d+)? (KB|MB)/, 'a real file size');
      assert.match(await row.innerText(), /\ds/, 'a real length');

      await row.getByTestId('recording-play').click();
      const video = page.getByTestId('recording-video');
      await video.waitFor();
      await eventually('the real recording to play', () => video.evaluate((v) => v.readyState >= 2 && v.videoWidth > 0 && v.duration > 1), { timeout: 20000 });
      // Seeking needs Range support all the way through the proxy.
      await video.evaluate((v) => new Promise((resolve, reject) => {
        v.pause();
        v.onseeked = resolve;
        v.onerror = () => reject(new Error('could not seek'));
        v.currentTime = v.duration / 2;
      }));
      await page.keyboard.press('Escape');

      const href = await row.getByTestId('recording-download').getAttribute('href');
      const download = await page.evaluate(async (href) => {
        const whole = await fetch(href);
        const part = await fetch(href, { headers: { Range: 'bytes=0-99' } });
        return { status: whole.status, disposition: whole.headers.get('content-disposition'), size: (await whole.arrayBuffer()).byteLength, partStatus: part.status, partSize: (await part.arrayBuffer()).byteLength };
      }, href);
      assert.equal(download.status, 200);
      assert.match(download.disposition, /^attachment/);
      assert.ok(download.size > 10000, `${download.size} bytes`);
      assert.equal(download.partStatus, 206);
      assert.equal(download.partSize, 100);

      const id = await row.getAttribute('data-id');
      await page.getByRole('button', { name: `Delete ${id}` }).click();
      await alert().getByRole('button', { name: 'Delete recording' }).click();
      await page.getByRole('heading', { name: 'No recordings yet' }).waitFor();
      assert.equal((await pageApi(page, 'GET', `/recordings/${id}`, { project: project.id })).status, 404);
    });

    it('runs a real egress job from the Egress page, and reports a refusal in plain words', async (t) => {
      if (!workersRunning()) return t.skip('the workers did not build');
      await page.goto(`${DASH}/egress`);
      await page.getByRole('button', { name: 'Start job' }).first().click();
      await dialog().getByLabel('Room', { exact: true }).fill('no-such-room');
      await dialog().getByRole('button', { name: 'Start job' }).click();
      await dialog().getByText('That room is not open.').waitFor();

      await dialog().getByLabel('Room', { exact: true }).fill(ROOM);
      await chooseOption('What to capture', 'Audio only', dialog());
      await dialog().getByRole('button', { name: 'Start job' }).click();
      const row = page.getByTestId('egress-row').first();
      await row.waitFor();
      await page.locator('[data-testid="egress-row"][data-status="active"]').waitFor({ timeout: 60000 });
      await sleep(3000);
      await row.getByTestId('egress-stop').click();
      await page.locator('[data-testid="egress-row"][data-status="complete"]').waitFor({ timeout: 60000 });
      assert.match(await row.innerText(), /Audio only/);

      // Anything saved to a file is also a recording.
      await page.goto(`${DASH}/recordings`);
      await page.locator('[data-testid="recording-row"][data-status="complete"]').waitFor();
    });

    it('takes a real RTMP stream through an ingress, shows its health, rotates the key and stops it', async (t) => {
      if (!workersRunning()) return t.skip('the workers did not build');
      await page.goto(`${DASH}/ingress`);
      await page.getByRole('heading', { name: 'No ingresses yet' }).waitFor();
      await page.getByRole('button', { name: 'Create ingress' }).first().click();
      await dialog().getByLabel('Name', { exact: true }).fill('Main stage');
      await dialog().getByLabel('Room', { exact: true }).fill('stage-room');
      await dialog().getByLabel('Participant identity').fill('stage');
      await dialog().getByRole('button', { name: 'Create ingress' }).click();

      const reveal = page.getByRole('dialog', { name: 'Your stream key' });
      await reveal.waitFor();
      const codes = await reveal.locator('pre code').allInnerTexts();
      const [publishUrl, streamKey] = codes;
      assert.match(publishUrl, /^rtmp:\/\/localhost:18935\/live$/);
      assert.ok(streamKey.length > 10, 'a real key');
      await reveal.getByLabel('I have saved the stream key').click();
      await reveal.getByRole('button', { name: 'Done' }).click();
      await page.waitForURL('**/ingress/IN_*');
      assert.ok(!(await page.content()).includes(streamKey), 'not shown again');
      await page.reload();
      await page.getByRole('heading', { name: /Main stage/ }).waitFor();
      assert.ok(!(await page.content()).includes(streamKey), 'not after reload either');
      const path = new URL(page.url()).pathname.replace(/^\/ingress\//, '/ingress/');

      const ffmpeg = pushRtmp(`${publishUrl}/${streamKey}`);
      try {
        await page.getByTestId('ingress-bitrate').filter({ hasText: /(k|M)bps/ }).waitFor({ timeout: 45000 });
        await page.getByText('640×360').waitFor({ timeout: 20000 });
        await eventually('the stream to be publishing', async () => {
          const api = (await pageApi(page, 'GET', path, { project: project.id })).json;
          return api.status === 'publishing' && api.stats.bitrate_in > 0;
        }, { timeout: 30000 });
        await page.locator('h1').getByText('Publishing').waitFor();
        await eventually('the stream to join the room as a participant', async () => {
          const res = await pageApi(page, 'GET', '/participants?room=stage-room', { project: project.id });
          return res.json.items?.some((p) => p.identity === 'stage');
        }, { timeout: 20000 });

        await page.getByRole('button', { name: 'Stop stream' }).click();
        await page.getByRole('heading', { name: 'No stream right now' }).waitFor({ timeout: 30000 });
      } finally {
        ffmpeg.kill('SIGKILL');
      }

      await page.getByRole('button', { name: 'Rotate stream key' }).click();
      await alert().getByRole('button', { name: 'Rotate key' }).click();
      await reveal.waitFor();
      const rotated = (await reveal.locator('pre code').allInnerTexts())[1];
      assert.notEqual(rotated, streamKey);
      await reveal.getByLabel('I have saved the stream key').click();
      await reveal.getByRole('button', { name: 'Done' }).click();

      await page.getByRole('button', { name: 'Delete' }).click();
      await alert().getByRole('button', { name: 'Delete ingress' }).click();
      await page.waitForURL('**/ingress');
      assert.equal((await pageApi(page, 'GET', path, { project: project.id })).status, 404);
    });

    it('says when no worker is available to start a job', async (t) => {
      if (!workersRunning()) return t.skip('the workers did not build');
      const node = (await pageApi(page, 'GET', '/servers')).json.items.find((n) => n.stats.kind === 'egress_worker').node_id;
      await pageApi(page, 'POST', `/servers/${node}/drain`, { body: { draining: true } });
      try {
        await page.goto(`${DASH}/recordings`);
        await page.getByText('No recording worker is running').waitFor({ timeout: 15000 });
        await page.getByRole('button', { name: 'Start recording' }).first().click();
        await dialog().getByLabel('Room', { exact: true }).fill(ROOM);
        await dialog().getByRole('button', { name: 'Start recording' }).click();
        await dialog().getByText('No egress worker is available').waitFor();
        await dialog().getByRole('button', { name: 'Cancel' }).click();
      } finally {
        await pageApi(page, 'POST', `/servers/${node}/drain`, { body: { draining: false } });
      }
    });
  });

  it('says plainly when a server has no such service', async () => {
    // Answered the way Go's mux answers an unknown route, for the build of a server without these services.
    const missing = (route) => route.fulfill({ status: 404, contentType: 'text/plain; charset=utf-8', body: '404 page not found\n' });
    await page.route(/\/api\/v1\/(egress|recordings|ingress)/, missing);
    for (const { path, name } of pages) {
      await page.goto(`${DASH}${path}`);
      await page.getByRole('heading', { name: `The ${name} service is not running` }).waitFor();
      assert.equal(await page.getByRole('button', { name: /Start|Create ingress/ }).count(), 0, 'nothing to click that cannot work');
    }
    await page.goto(`${DASH}/overview`);
    await page.getByText('Not running').first().waitFor();
    await page.unroute(/\/api\/v1\/(egress|recordings|ingress)/);
  });

  describe('against a stub of the documented contract', () => {
    // Test-only. Requests are answered by this test, in the browser, as docs/api/media-services.md describes. It covers
    // states that are hard to reach for real, such as a failed job, and says nothing about the real service.
    const job = (over) => ({
      id: 'EG_done', project_id: 'p', room_name: 'standup', room_sid: 'RM_1', type: 'room_composite', status: 'complete', format: 'webm',
      output_url: '/api/v1/recordings/EG_done/file', stream_urls: [], file_size: 12345, duration_ms: 2000, error: '',
      created_at: new Date().toISOString(), started_at: new Date().toISOString(), ended_at: new Date().toISOString(), retry_of: null, ...over,
    });
    const ingress = (over) => ({
      id: 'IN_1', name: 'Main stage', input_type: 'rtmp', room_name: 'standup', participant_identity: 'stage', participant_name: 'Main stage', source_url: '', enabled: true,
      publish_url: 'rtmp://localhost:1935/live', status: 'publishing', error: '',
      stats: { bitrate_in: 2500000, width: 1920, height: 1080, fps: 30, audio_codec: 'aac', video_codec: 'h264', started_at: new Date(Date.now() - 60000).toISOString() },
      created_at: new Date().toISOString(), updated_at: new Date().toISOString(), ...over,
    });
    const json = (route, body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    let video;

    before(async () => {
      execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'lavfi', '-i', 'testsrc=size=160x120:rate=10:duration=2', '-c:v', 'libvpx', '-b:v', '100k', join(tmpDir, 'stub.webm')]);
      video = readFileSync(join(tmpDir, 'stub.webm'));
    });

    it('plays a recording in a video element, downloads it, retries and deletes', async () => {
      const calls = [];
      const file = /\/api\/v1\/recordings\/EG_done\/file/;
      await page.route(/\/api\/v1\/(recordings|egress)/, (route) => {
        const req = route.request();
        const u = new URL(req.url());
        calls.push(`${req.method()} ${u.pathname}${u.search}`);
        if (file.test(u.pathname)) return route.fulfill({ status: 200, contentType: 'video/webm', body: video });
        if (req.method() === 'GET') {
          return json(route, { items: [job(), job({ id: 'EG_failed', status: 'failed', error: 'worker crashed', output_url: '', file_size: 0, duration_ms: 0 })], next_cursor: '' });
        }
        if (req.method() === 'POST' && u.pathname.endsWith('/retry')) return json(route, job({ id: 'EG_retry', status: 'starting' }));
        if (req.method() === 'POST' && u.pathname === '/api/v1/egress') return json(route, { error: { code: 'egress_unavailable', message: 'No egress worker is running.' } }, 503);
        if (req.method() === 'DELETE') return route.fulfill({ status: 204 });
        return json(route, {});
      });
      await page.goto(`${DASH}/recordings`);
      const row = page.locator('[data-testid="recording-row"][data-id="EG_done"]');
      await row.waitFor();
      assert.match(await row.innerText(), /12\.3 KB/);
      assert.match(await row.innerText(), /2s/);

      await row.getByTestId('recording-play').click();
      const el = page.getByTestId('recording-video');
      await el.waitFor();
      assert.match(await el.getAttribute('src'), new RegExp(`project_id=${state.projectId}`), 'a video element cannot send a header');
      await eventually('the video to load frames', () => el.evaluate((v) => v.readyState >= 2 && v.videoWidth === 160), { timeout: 15000 });
      await page.keyboard.press('Escape');

      assert.match(await row.getByTestId('recording-download').getAttribute('href'), /project_id=/);
      const failed = page.locator('[data-testid="recording-row"][data-id="EG_failed"]');
      await failed.getByText('worker crashed').waitFor();
      await failed.getByRole('button', { name: 'Retry' }).click();
      await page.getByText('New job EG_retry.').first().waitFor();
      assert.ok(calls.some((c) => c.startsWith('POST /api/v1/recordings/EG_failed/retry')));

      await page.getByRole('button', { name: 'Delete EG_failed' }).click();
      await alert().getByRole('button', { name: 'Delete recording' }).click();
      await eventually('the delete to be sent', () => calls.some((c) => c.startsWith('DELETE /api/v1/recordings/EG_failed')));

      await page.getByRole('button', { name: 'Start recording' }).click();
      await dialog().getByLabel('Room', { exact: true }).fill('standup');
      await dialog().getByRole('button', { name: 'Start recording' }).click();
      await dialog().getByText('No egress worker is available').waitFor();
      await dialog().getByRole('button', { name: 'Cancel' }).click();
      await page.unroute(/\/api\/v1\/(recordings|egress)/);
    });

    it('starts and stops an egress job, sending what the contract says', async () => {
      let started;
      const calls = [];
      await page.route(/\/api\/v1\/egress/, (route) => {
        const req = route.request();
        const u = new URL(req.url());
        calls.push(`${req.method()} ${u.pathname}`);
        if (req.method() === 'GET') return json(route, { items: [job({ id: 'EG_live', status: 'active', stream_urls: ['rtmp://x/y'] })], next_cursor: '' });
        if (u.pathname.endsWith('/stop')) return json(route, job({ id: 'EG_live', status: 'ending' }));
        started = req.postDataJSON();
        return json(route, job({ id: 'EG_new', status: 'starting' }));
      });
      await page.goto(`${DASH}/egress`);
      const row = page.locator('[data-testid="egress-row"][data-id="EG_live"]');
      await row.waitFor();
      await row.getByTestId('egress-stop').click();
      await eventually('stop to be sent', () => calls.includes('POST /api/v1/egress/EG_live/stop'));

      await page.getByRole('button', { name: 'Start job' }).click();
      await chooseOption('What to capture', 'One participant', dialog());
      await dialog().getByLabel('Room', { exact: true }).fill('standup');
      await dialog().getByRole('button', { name: 'Start job' }).click();
      await dialog().getByText('Name the participant to capture.').waitFor();
      await dialog().getByLabel('Participant identity').fill('alice');
      await dialog().getByLabel('Stream to').fill('rtmp://a.example.com/live/key');
      await dialog().getByRole('button', { name: 'Start job' }).click();
      await eventually('the job to be sent', () => started);
      assert.deepEqual(started, { type: 'participant', room: 'standup', participant: 'alice', output: { kind: 'file', format: 'mp4' }, stream_urls: ['rtmp://a.example.com/live/key'] });
      await page.unroute(/\/api\/v1\/egress/);
    });

    it('creates an ingress, reveals its key once, shows health, and rotates', async () => {
      const calls = [];
      let created;
      await page.route(/\/api\/v1\/ingress/, (route) => {
        const req = route.request();
        const u = new URL(req.url());
        calls.push(`${req.method()} ${u.pathname}`);
        if (u.pathname.endsWith('/logs')) return json(route, { items: [{ id: 1, message: 'Stream started', created_at: new Date().toISOString() }] });
        if (u.pathname.endsWith('/rotate-key')) return json(route, ingress({ stream_key: 'rotated-stream-key' }));
        if (u.pathname.endsWith('/stop')) return json(route, ingress({ status: 'inactive' }));
        if (req.method() === 'POST') {
          created = req.postDataJSON();
          return json(route, ingress({ stream_key: 'first-stream-key', status: 'inactive' }));
        }
        if (req.method() === 'PATCH') return json(route, ingress(req.postDataJSON()));
        if (req.method() === 'DELETE') return route.fulfill({ status: 204 });
        return json(route, u.pathname === '/api/v1/ingress' ? { items: [ingress()] } : ingress());
      });
      await page.goto(`${DASH}/ingress`);
      await page.locator('[data-testid="ingress-row"][data-id="IN_1"]').waitFor();
      await page.getByRole('button', { name: 'Create ingress' }).click();
      await dialog().getByLabel('Name', { exact: true }).fill('Main stage');
      await dialog().getByLabel('Room', { exact: true }).fill('standup');
      await dialog().getByLabel('Participant identity').fill('stage');
      await dialog().getByRole('button', { name: 'Create ingress' }).click();
      await page.getByRole('dialog', { name: 'Your stream key' }).getByText('first-stream-key').waitFor();
      assert.deepEqual(created, { name: 'Main stage', input_type: 'rtmp', room: 'standup', participant_identity: 'stage' });
      await dialog().getByLabel('I have saved the stream key').click();
      await dialog().getByRole('button', { name: 'Done' }).click();
      await page.waitForURL('**/ingress/IN_1');

      assert.ok(!(await page.content()).includes('first-stream-key'), 'not shown again');
      await page.getByTestId('ingress-bitrate').getByText('2.50 Mbps').waitFor();
      await page.getByText('1920×1080').waitFor();
      await page.getByText('Stream started').waitFor();

      await page.getByRole('button', { name: 'Rotate stream key' }).click();
      await alert().getByRole('button', { name: 'Rotate key' }).click();
      await page.getByRole('dialog', { name: 'Your stream key' }).getByText('rotated-stream-key').waitFor();
      await dialog().getByLabel('I have saved the stream key').click();
      await dialog().getByRole('button', { name: 'Done' }).click();

      await page.getByRole('button', { name: 'Stop stream' }).click();
      await eventually('stop to be sent', () => calls.includes('POST /api/v1/ingress/IN_1/stop'));
      await page.getByLabel('Enabled').click();
      await eventually('enabled to be sent', () => calls.includes('PATCH /api/v1/ingress/IN_1'));
      await page.getByRole('button', { name: 'Delete' }).click();
      await alert().getByRole('button', { name: 'Delete ingress' }).click();
      await page.waitForURL('**/ingress');
      assert.ok(calls.includes('DELETE /api/v1/ingress/IN_1'));
      await page.unroute(/\/api\/v1\/ingress/);
    });
  });
});

describe('product name', () => {
  it('says Elnova StreamKit in the wordmark, the page title and everywhere else on screen', async () => {
    await selectProject(state.projectId);
    const paths = ['/overview', '/monitor', '/rooms', '/participants', '/recordings', '/egress', '/ingress', '/analytics', '/logs', '/logs?tab=audit', '/servers', '/projects', `/projects/${state.projectId}`, '/developer', '/developer?tab=tokens', '/developer?tab=settings', '/webhooks', '/account', '/docs', '/docs/api-reference'];
    for (const path of paths) {
      await page.goto(`${DASH}${path}`);
      await page.getByRole('heading', { level: 1 }).first().waitFor();
      await page.waitForLoadState('networkidle', { timeout: 3000 }).catch(() => {});
      assert.match(await page.title(), /Elnova StreamKit/, `title of ${path}`);
      assert.ok(!(await page.locator('body').innerText()).includes('REA Streaming'), `${path} still says REA Streaming`);
    }
    await page.goto(`${DASH}/overview`);
    await page.getByRole('link', { name: 'Elnova StreamKit, overview' }).getByText('StreamKit').waitFor();

    const anon = await browser.newContext();
    const p = await anon.newPage();
    for (const path of ['/login', '/register', '/forgot-password', '/reset-password?token=abc', '/docs', '/docs/getting-started', '/docs/api-reference', '/docs/self-hosting']) {
      await p.goto(`${DASH}${path}`);
      await p.getByRole('heading', { level: 1 }).first().waitFor();
      await p.waitForLoadState('networkidle', { timeout: 3000 }).catch(() => {});
      assert.match(await p.title(), /Elnova StreamKit/, `title of ${path}`);
      assert.ok(!(await p.locator('body').innerText()).includes('REA Streaming'), `${path} still says REA Streaming`);
    }
    await anon.close();
  });
});

describe('after all that', () => {
  it('raised no uncaught errors in the browser', () => {
    assert.deepEqual(pageErrors, []);
  });

  it('wrote no failures to the event log of the project it used most', async () => {
    const res = await pageApi(page, 'GET', '/logs?category=api&level=error', { project: state.projectId });
    assert.deepEqual(res.json.items, []);
  });
});
