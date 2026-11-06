// Starts the real servers once for the whole test run. Set STREAMKIT_TEST_DATABASE_URL to run the tests that need them,
// and STREAMKIT_TEST_WORKERS=1 for the recording and ingest tests. See tests/sdk/fixture for what runs.
import { spawn, spawnSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import type { TestProject } from 'vitest/node';

const repo = join(dirname(fileURLToPath(import.meta.url)), '../../..');

export interface Server {
  url: string;
  http: string;
  api_key: string;
  api_secret: string;
}

export interface FixtureInfo {
  ours: Server;
  reference: Server | null;
  plain: Server;
  webhooks: string;
  cli: string;
  reference_binary: string;
  rtmp: string;
  workers: boolean;
}

declare module 'vitest' {
  export interface ProvidedContext {
    fixture: FixtureInfo | null;
  }
}

export default async function setup(project: TestProject) {
  if (!process.env.STREAMKIT_TEST_DATABASE_URL) {
    console.log('STREAMKIT_TEST_DATABASE_URL is not set: only the tests that need no server will run');
    project.provide('fixture', null);
    return;
  }
  const binary = join(repo, 'bin/sdkfixture');
  const built = spawnSync('go', ['build', '-o', binary, './tests/sdk/cmd/sdkfixture'], { cwd: repo, stdio: 'inherit' });
  if (built.status !== 0) {
    throw new Error('could not build the fixture. Is Go on PATH?');
  }
  const args = process.env.STREAMKIT_TEST_WORKERS ? ['--workers'] : [];
  // Closing its standard input stops everything, so nothing outlives this run however it ends.
  const child = spawn(binary, args, { cwd: repo, stdio: ['pipe', 'pipe', 'inherit'] });
  const lines = createInterface({ input: child.stdout });
  const info = await new Promise<FixtureInfo>((resolve, reject) => {
    child.on('exit', (code) => reject(new Error(`the fixture exited with ${code}`)));
    lines.once('line', (line) => resolve(JSON.parse(line)));
  });
  project.provide('fixture', info);
  return async () => {
    const exited = new Promise((resolve) => child.on('exit', resolve));
    child.stdin.end();
    await exited;
  };
}
