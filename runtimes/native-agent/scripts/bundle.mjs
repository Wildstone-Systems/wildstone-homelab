// Bundles the sidecar into one self-contained file for release:
//
//   dist/bundle/main.mjs                 the sidecar, dependencies included
//   dist/bundle/THIRD_PARTY_LICENSES.txt the licenses of what was bundled
//
// The release ships this file next to a Node runtime, so an installed
// host needs neither npm nor a node_modules tree.

import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const out = join(root, 'dist', 'bundle');
mkdirSync(out, { recursive: true });

const readJSON = (p) => JSON.parse(readFileSync(p, 'utf8'));
const sdkVersion = readJSON(join(root, 'node_modules', '@openai', 'agents-core', 'package.json')).version;

await build({
  entryPoints: [join(root, 'src', 'main.ts')],
  outfile: join(out, 'main.mjs'),
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node22',
  minify: false,
  sourcemap: false,
  legalComments: 'none',
  // Some dependencies still call require(); an ESM bundle needs one.
  banner: { js: "import { createRequire as __wsCreateRequire } from 'node:module'; const require = __wsCreateRequire(import.meta.url);" },
  // Bundled, the SDK's package.json is no longer on disk to read.
  define: { __AGENTS_SDK_VERSION__: JSON.stringify(sdkVersion) },
  logLevel: 'warning',
});

// Every production dependency's license, in one file.
const tree = execFileSync('npm', ['ls', '--omit=dev', '--all', '--parseable'], { cwd: root, encoding: 'utf8' })
  .split('\n')
  .filter((p) => p && p !== root);
const seen = new Set();
const sections = [];
for (const dir of tree.sort()) {
  const pkgPath = join(dir, 'package.json');
  if (!existsSync(pkgPath)) continue;
  const pkg = readJSON(pkgPath);
  const id = `${pkg.name}@${pkg.version}`;
  if (seen.has(id)) continue;
  seen.add(id);
  const file = readdirSync(dir).find((f) => /^(licen[cs]e|copying)(\..*)?$/i.test(f));
  const text = file ? readFileSync(join(dir, file), 'utf8').trim() : `License: ${pkg.license ?? 'UNKNOWN'} (no license file shipped)`;
  sections.push(`${id}\n${'-'.repeat(id.length)}\n${text}\n`);
}
writeFileSync(join(out, 'THIRD_PARTY_LICENSES.txt'), sections.join('\n'));
console.log(`bundled ${join(out, 'main.mjs')} (SDK ${sdkVersion}, ${seen.size} licensed dependencies)`);
