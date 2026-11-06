// Bundles the recorder page from source, including the SDK, into assets/recorder.js.
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, '../..');

await build({
  entryPoints: [join(here, 'src/recorder.ts')],
  outfile: join(here, 'assets/recorder.js'),
  bundle: true,
  format: 'iife',
  target: 'es2021',
  minify: true,
  legalComments: 'none',
  logLevel: 'warning',
  define: { 'process.env.NODE_ENV': '"production"' },
  alias: { '@elnova-streamkit/client': join(repo, 'sdk-js/src/index.ts') },
  nodePaths: [join(repo, 'sdk-js/node_modules')],
});
