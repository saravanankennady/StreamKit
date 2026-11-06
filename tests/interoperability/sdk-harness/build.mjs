// Bundles the test pages. Each SDK is built from source, so a test run always exercises the code in the
// tree. Both of our packages have to resolve to one copy of the client, or React would see different classes.
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, '../../..');

export async function buildHarness() {
  const common = {
    bundle: true,
    format: 'esm',
    logLevel: 'warning',
    sourcemap: 'inline',
    target: 'es2021',
    define: { 'process.env.NODE_ENV': '"development"' },
    jsx: 'automatic',
    alias: {
      '@elnova-streamkit/client': join(repo, 'sdk-js/src/index.ts'),
      '@elnova-streamkit/react': join(repo, 'sdk-react/src/index.ts'),
    },
    nodePaths: [join(repo, 'sdk-js/node_modules'), join(repo, 'sdk-react/node_modules'), join(here, '../node_modules')],
  };
  await build({ ...common, entryPoints: [join(here, 'client.ts')], outfile: join(here, 'dist/client.js') });
  // The official client exists only as something to interoperate with. Nothing else may import it.
  await build({ ...common, entryPoints: [join(here, '../oracle/client.ts')], outfile: join(here, 'dist/official.js') });
  await build({ ...common, entryPoints: [join(here, 'react-client.tsx')], outfile: join(here, 'dist/react-client.js') });
}

if (import.meta.url === `file://${process.argv[1]}`) await buildHarness();
