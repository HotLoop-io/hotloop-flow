// Writes the editor bundle's part of THIRD_PARTY_LICENSES, and fails the build
// if anything in the bundle carries a license we haven't agreed to ship.
//
//   node third-party-licenses.mjs meta.json /build/THIRD_PARTY_LICENSES.editor
//
// meta.json is what the Astro build writes (astro.config.mjs): the record of
// every file the bundler pulled into dist. That's the real graph.
// package.json's devDependencies aren't: Astro, Svelte's compiler and
// TypeScript build the bundle and ship none of themselves, while Svelte's small
// runtime and the fonts do end up in dist and embedded in the Go binary. So the
// list comes from what the bundler actually read, the same way the Go side
// reads the binary's import graph.
//
// No dependencies on purpose. A license checker that needs its own npm tree
// is one more thing in node_modules nobody reviewed.

import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

// SPDX ids from each package.json. Anything else fails the build. OFL-1.1 is
// here for the self-hosted fonts: free to bundle and embed with software, as
// long as the license goes with them and nobody sells the fonts on their own.
// GPL, AGPL, LGPL, a missing license field or a missing license file never
// get in.
const ALLOWED = new Set([
  'MIT', 'MIT-0', 'ISC', '0BSD', 'BSD-2-Clause', 'BSD-3-Clause', 'Apache-2.0',
  'Zlib', 'BSL-1.0', 'Unlicense', 'CC0-1.0', 'OFL-1.1',
]);

const [metaPath, outPath] = process.argv.slice(2);
if (!metaPath || !outPath) {
  console.error('usage: node third-party-licenses.mjs META.json OUT');
  process.exit(2);
}

function fail(msg) {
  console.error('\nTHIRD-PARTY LICENSE CHECK FAILED for the editor bundle');
  console.error(msg);
  console.error(`Allowed: ${[...ALLOWED].join(' ')}`);
  console.error("Don't widen the allowlist to get past this. Replace the package, or");
  console.error('get a yes from whoever owns licensing first.');
  process.exit(1);
}

// node_modules/@scope/name/files/x.woff2 -> node_modules/@scope/name. The last
// node_modules in the path wins, so a nested copy is credited, not its parent.
function packageDir(input) {
  const i = input.lastIndexOf('node_modules/');
  if (i < 0) return null;
  const rest = input.slice(i + 'node_modules/'.length).split('/');
  const n = rest[0].startsWith('@') ? 2 : 1;
  return input.slice(0, i) + 'node_modules/' + rest.slice(0, n).join('/');
}

function licenseOf(pkg) {
  if (typeof pkg.license === 'string') return pkg.license;
  if (pkg.license && typeof pkg.license.type === 'string') return pkg.license.type;
  if (Array.isArray(pkg.licenses)) return pkg.licenses.map((l) => l.type).join(' OR ');
  return '';
}

// "(MIT OR Apache-2.0)" is fine if either side is; "MIT AND X" needs both.
function allowedExpr(expr) {
  const e = expr.replace(/[()]/g, '').trim();
  if (!e) return false;
  if (/ OR /.test(e)) return e.split(/ OR /).some((p) => allowedExpr(p));
  if (/ AND /.test(e)) return e.split(/ AND /).every((p) => allowedExpr(p));
  return ALLOWED.has(e);
}

const meta = JSON.parse(readFileSync(metaPath, 'utf8'));
const dirs = new Map();
for (const input of Object.keys(meta.inputs)) {
  const dir = packageDir(input);
  if (dir) {
    if (!dirs.has(dir)) dirs.set(dir, []);
    dirs.get(dir).push(input);
  }
}

const pkgs = [];
const bad = [];
for (const [dir, files] of [...dirs].sort()) {
  const pkg = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
  const license = licenseOf(pkg);
  const texts = readdirSync(dir)
    .filter((f) => /^(un)?licen[cs]e|^copying|^notice|^ofl/i.test(f) && statSync(join(dir, f)).isFile())
    .sort();
  if (!allowedExpr(license)) bad.push(`    ${pkg.name} ${pkg.version}: ${license || 'no license field'}`);
  else if (texts.length === 0) bad.push(`    ${pkg.name} ${pkg.version}: ${license}, but no license file in the package`);
  pkgs.push({ name: pkg.name, version: pkg.version, license, dir, texts, files });
}
if (bad.length) fail(`Not on the allowlist, unidentifiable, or shipped without its text:\n${bad.join('\n')}`);

const rule = '='.repeat(80);
const thin = '-'.repeat(80);
let out = `${rule}\nEditor bundle (npm)\n${rule}\n\n`;
out += 'The browser editor is built with Astro and Svelte and embedded in the binary.\n';
out += 'These packages are the ones the bundler put into it, from its own record.\n';
if (pkgs.length === 0) out += 'None: the bundle is all HotLoop Flow source.\n';
out += '\n';
for (const p of pkgs) out += `${p.name.padEnd(58)} ${p.version.padEnd(20)} ${p.license}\n`;
out += '\n';
for (const p of pkgs) {
  out += `${rule}\n${p.name} ${p.version} (npm)\nLicense: ${p.license}\n`;
  out += `In the bundle: ${p.files.map((f) => f.slice(p.dir.length + 1)).join(', ')}\n${thin}\n`;
  for (const t of p.texts) out += `[${t}]\n${readFileSync(join(p.dir, t), 'utf8').trimEnd()}\n\n`;
}
writeFileSync(outPath, out);

const counts = {};
for (const p of pkgs) counts[p.license] = (counts[p.license] || 0) + 1;
console.error(`editor bundle: ${pkgs.length} npm packages, written to ${outPath}`);
for (const [l, n] of Object.entries(counts).sort()) console.error(`  ${String(n).padStart(6)} ${l}`);
