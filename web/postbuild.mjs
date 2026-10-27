// Finishes the build so the page runs from any admin root, and under a strict
// CSP that allows no inline script and no inline style:
//
//  - The runtime serves the editor at HOTLOOP_FLOW_ADMIN_ROOT, which can be any
//    path, and the EmberCORE dashboard proxies it under another. So no URL may
//    start with a slash. Astro writes /_astro/... everywhere; every one becomes
//    relative to the file that holds it.
//  - Astro writes its island bootstrap into the page as inline <script> and
//    <style>. They are the same bytes in every build, so they move into files
//    beside the page and the page links them. Same code, same order.
//  - Then the page is checked: any inline script or style, a style attribute, a
//    link to another host, or a leftover root-relative URL fails the build.
//
// dist/.gitkeep stays, because go:embed needs a directory that exists on a fresh
// clone.
import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

// What dist/.gitkeep says, byte for byte as it is tracked, so a build leaves the tree clean.
const GITKEEP = "Placeholder so //go:embed all:dist compiles on a fresh clone.\n\nThe real bundle is written here by `cd web && npm run build` and is not\ncommitted. A binary built without it starts, serves the API, and returns a\nplain-text explanation on the editor route rather than a blank page \u2014\nsee web.Assets.\n";
const page = 'dist/index.html';
let html = readFileSync(page, 'utf8');

const put = (name, body, ext) => {
  const hash = createHash('sha256').update(body).digest('base64url').slice(0, 8);
  const file = `_astro/${name}.${hash}.${ext}`;
  writeFileSync(`dist/${file}`, body);
  return `./${file}`;
};

let n = 0;
html = html.replace(/<script>([\s\S]*?)<\/script>/g, (_m, body) => `<script src="${put(`island-${++n}`, body, 'js')}"></script>`);
html = html.replace(/<style>([\s\S]*?)<\/style>/g, (_m, body) => `<link rel="stylesheet" href="${put('island', body, 'css')}">`);

// The island's own files are loaded by import() from the island scripts above,
// which live in _astro, so those URLs are relative to that folder. Every other
// URL in the page is relative to the page.
html = html.replace(/(component-url|renderer-url|before-hydration-url)="\/_astro\//g, '$1="./');
html = html.replace(/(href|src)="\/_astro\//g, '$1="./_astro/');
writeFileSync(page, html);

// CSS sits in _astro, next to the fonts it names.
for (const f of readdirSync('dist/_astro').filter((f) => f.endsWith('.css'))) {
  const p = `dist/_astro/${f}`;
  const css = readFileSync(p, 'utf8');
  const fixed = css.replace(/url\(\/_astro\//g, 'url(./');
  if (fixed !== css) writeFileSync(p, fixed);
}

const bad = [];
if (/<script(?![^>]*\ssrc=)[^>]*>/.test(html)) bad.push('an inline <script>');
if (/<style[\s>]/.test(html)) bad.push('an inline <style>');
if (/\sstyle="/.test(html)) bad.push('a style attribute');
if (/https?:\/\/(?!www\.w3\.org\/2000\/svg)/.test(html)) bad.push('a link to another host');
if (/(?:href|src|url)=?"\//.test(html)) bad.push('a root-relative URL');
for (const f of readdirSync('dist/_astro')) {
  const text = readFileSync(`dist/_astro/${f}`, f.endsWith('.woff2') ? null : 'utf8');
  if (typeof text === 'string' && f.endsWith('.css') && /url\(\/(?!\/)/.test(text)) bad.push(`a root-relative url() in ${f}`);
}
if (bad.length) {
  console.error(`postbuild: the page still has ${bad.join(', ')}; it would not run from every admin root`);
  process.exit(1);
}
writeFileSync('dist/.gitkeep', GITKEEP);
console.error(`postbuild: ${n} inline script${n === 1 ? '' : 's'} moved out, URLs are relative, page is clean`);
