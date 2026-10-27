// The editor build: Astro, with Svelte islands, to static files that the Go
// binary embeds. Nothing is fetched from anywhere at runtime: an edge box has no
// internet, and the EmberCORE dashboard's proxy refuses other hosts.
//
// The editor is served from whatever admin root the runtime was started with
// (HOTLOOP_FLOW_ADMIN_ROOT), so every URL in the build is relative to the page.
// postbuild.mjs makes them so. Stylesheets are never inlined, and postbuild
// moves the few inline scripts Astro writes into files, so the page runs under
// a strict CSP too.
import { defineConfig } from 'astro/config';
import svelte from '@astrojs/svelte';
import { writeFileSync } from 'node:fs';
import { relative } from 'node:path';

// Writes meta.json, the record of every file that went into the bundle, in the
// shape third-party-licenses.mjs reads, so the npm packages that ship are the
// ones credited and the ones license-checked.
function bundleRecord() {
  const inputs = {};
  return {
    name: 'bundle-record',
    generateBundle(_o, bundle) {
      for (const f of Object.values(bundle)) {
        if (f.type === 'chunk') for (const id of Object.keys(f.modules)) inputs[relative(process.cwd(), id.split('?')[0])] = { bytes: 0 };
        else for (const o of f.originalFileNames ?? []) inputs[relative(process.cwd(), o)] = { bytes: 0 };
      }
    },
    closeBundle() {
      writeFileSync('meta.json', JSON.stringify({ inputs }));
    },
  };
}

export default defineConfig({
  integrations: [svelte()],
  output: 'static',
  trailingSlash: 'never',
  build: { inlineStylesheets: 'never', format: 'file', assets: '_astro' },
  vite: { build: { assetsInlineLimit: 0 }, plugins: [bundleRecord()] },
  devToolbar: { enabled: false },
});
