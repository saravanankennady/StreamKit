// Runs the whole suite, a group of tests at a time, each group through the machine's .toolchain/heavy.sh, so other work gets
// a turn between groups. See .toolchain/agent-rules.md. All the groups in one run share a run ID, so report.mjs can show them
// together.
//
//   node run-all.mjs [word ...]     only the groups whose file or pattern contains one of the words
import { spawnSync } from 'node:child_process';
import { readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
let toolchain = here;
while (!readdirSync(toolchain).includes('.toolchain')) toolchain = dirname(toolchain);
const heavy = join(toolchain, '.toolchain/heavy.sh');

const GROUPS = [
  ['impairment', 'hand-measured'],
  ['impairment', 'packet loss'],
  ['impairment', 'round trip'],
  ['impairment', 'jitter'],
  ['bandwidth', 'screen share'],
  ['bandwidth', 'camera'],
  ['outage'],
  ['fallback', 'UDP blocked'],
  ['fallback', 'TURN'],
  ['reconnect'],
  ['roam'],
  ['firefox'],
  ['features'],
];
const words = process.argv.slice(2);
const chosen = GROUPS.filter((g) => words.length === 0 || words.some((w) => g.join(' ').includes(w)));
const runId = process.env.NET_RUN_ID ?? new Date().toISOString().slice(0, 16).replace(/[:T]/g, '-');

let failed = 0;
for (const [name, pattern] of chosen) {
  console.log(`\n=== ${name}${pattern ? `: ${pattern}` : ''} (run ${runId}) ===`);
  const test = ['--test', '--test-concurrency=1', '--test-reporter=spec', ...(pattern ? [`--test-name-pattern=${pattern}`] : []), `${name}.test.mjs`];
  const result = spawnSync(heavy, ['node', ...test], {
    cwd: here,
    stdio: 'inherit',
    env: { ...process.env, NET_RUN_ID: runId },
  });
  if (result.status !== 0) failed++;
}
console.log(`\n${chosen.length - failed} of ${chosen.length} groups passed. Results: tests/network/results/${runId}.jsonl`);
process.exit(failed ? 1 : 0);
