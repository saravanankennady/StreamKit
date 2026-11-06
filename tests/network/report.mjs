// Prints measured results as Markdown: one table per test case, ours and the reference side by side, one column per run.
// It adds nothing of its own. Every number comes from a row a test recorded.
//
//   node report.mjs <run ID> [<run ID> ...]
import { readResults } from './lib.mjs';

const runs = process.argv.slice(2);
if (runs.length === 0) {
  console.error('usage: node report.mjs <run ID> [<run ID> ...]');
  process.exit(2);
}

// The numbers worth showing, in reading order, with the names people will read them under.
const FIELDS = [
  ['firstFrameMs', 'First frame, ms'],
  ['connectMs', 'Time to connect, ms'],
  ['fps', 'Presented fps'],
  ['p50', 'Latency p50, ms'],
  ['p99', 'Latency p99, ms'],
  ['longestGapMs', 'Longest gap, ms'],
  ['freezes', 'Freezes'],
  ['freezeMs', 'Frozen, ms'],
  ['usualWidth', 'Usual width, px'],
  ['kbps', 'Received, kbps'],
  ['rttMs', 'Round trip seen by browser, ms'],
  ['nacks', 'NACKs sent'],
  ['plis', 'PLIs sent'],
  ['residualLossPct', 'Stayed lost, %'],
  ['retransmitted', 'Retransmitted packets (RTX)'],
  ['reconnects', 'Reconnects'],
  ['paused', 'Paused events'],
  ['atTopMs', 'Reached top layer after, ms'],
  ['adaptedMs', 'Steady again after the step, ms'],
  ['steadyMs', 'Steady video after, ms'],
  ['longestStallMs', 'Longest stall after the step, ms'],
  ['recoveryMs', 'Back to top layer after lift, ms'],
  ['afterFps', 'Fps afterwards'],
  ['afterWidth', 'Width afterwards, px'],
  ['roamerResumes', 'Moved client sees video after, ms'],
  ['thereResumes', 'Other client sees video after, ms'],
  ['addressBefore', 'Address before'],
  ['addressAfter', 'Address after'],
  ['reconnecting', 'Reconnecting events'],
  ['reconnected', 'Reconnected events'],
  ['viewerReconnected', 'Viewer reconnected events'],
  ['publisherReconnected', 'Sharer reconnected events'],
  ['newSession', 'New session'],
  ['sameSessions', 'Same sessions'],
];

// Fields that are shown another way, or only say a test ran as it should.
const HIDDEN = ['errors', 'timeline', 'widths', 'limited', 'netem', 'sender', 'seconds', 'readable', 'gapsOver1s', 'decodedFps', 'p90', 'firs', 'packets', 'topWidth', 'lastWidth', 'lost', 'audioConcealedPct', 'subnetA', 'subnetB', 'viewerStayedOnImpairedPorts'];

// A selected route, in short: what kind of candidate, over what.
const route = (p) => `${p.localType}/${p.localProtocol}${p.relayProtocol ? ` via TURN over ${p.relayProtocol}` : ''}`;

const show = (v) => {
  if (v === null || v === undefined) return '-';
  if (Array.isArray(v) && v[0]?.localType) return [...new Set(v.map(route))].join(', ');
  if (typeof v === 'object') return JSON.stringify(v).replaceAll('|', '/');
  return String(v).replaceAll('\n', ' ').replaceAll('|', '/');
};

/** Flattens the measurements one level, so that what was measured under the limit reads as a row of its own. */
function fieldsOf(metrics) {
  const out = { ...metrics };
  for (const [key, value] of Object.entries(metrics ?? {})) {
    if (key === 'limited' && value) {
      for (const [k, v] of Object.entries(value)) out[`limited.${k}`] = v;
    }
  }
  return out;
}

const rows = runs.flatMap((run) => readResults(run).map((r) => ({ ...r, run })));
const scenarios = [...new Set(rows.map((r) => r.scenario))];

for (const scenario of scenarios) {
  const here = rows.filter((r) => r.scenario === scenario);
  console.log(`\n#### ${scenario}\n`);
  console.log(`Emulated link: ${here[0].link}.\n`);
  const columns = [];
  for (const kind of ['reference', 'ours']) {
    for (const row of here.filter((r) => r.server === kind)) columns.push({ title: `${kind === 'ours' ? 'Ours' : 'Reference'} (${row.run})`, row, fields: fieldsOf(row.metrics) });
  }
  const names = [];
  for (const [key, label] of FIELDS) {
    names.push([key, label]);
    names.push([`limited.${key}`, `${label}, on the thin link`]);
  }
  // Anything else a test measured still gets a row under its own name, so no result is hidden by leaving it off the list.
  const listed = new Set([...names.map(([k]) => k), ...HIDDEN]);
  for (const c of columns) {
    for (const key of Object.keys(c.fields)) {
      if (!listed.has(key) && !key.startsWith('limited.')) {
        listed.add(key);
        names.push([key, key]);
      }
    }
  }
  console.log(`| | ${columns.map((c) => c.title).join(' | ')} |`);
  console.log(`| --- | ${columns.map(() => '---').join(' | ')} |`);
  for (const [key, label] of names) {
    if (columns.every((c) => c.fields[key] === undefined)) continue;
    console.log(`| ${label} | ${columns.map((c) => show(c.fields[key])).join(' | ')} |`);
  }
  console.log(`| Widths seen | ${columns.map((c) => show(c.fields.timeline ?? c.fields['limited.widths'] ?? c.fields.widths)).join(' | ')} |`);
  console.log(`| Machine load (1 min) | ${columns.map((c) => c.row.load?.started?.load1 ?? '-').join(' | ')} |`);
  console.log(`| CPU busy at the start, % | ${columns.map((c) => c.row.load?.started?.busyPct ?? '-').join(' | ')} |`);
  console.log(`| Result | ${columns.map((c) => (c.row.error ? `error: ${c.row.error}` : c.row.failed?.length ? `missed: ${c.row.failed.join('; ')}` : 'met every check')).join(' | ')} |`);
}
