// Starts the real hotloop-flow binary for the browser tests: a fresh data
// directory, two users, a credential secret, nothing shared with anything else.
// No mock server anywhere. The editor under test talks to the same binary that
// ships, with the editor bundle embedded in it.

import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const exe = process.platform === 'win32' ? 'hotloop-flow.exe' : 'hotloop-flow';
const bin = process.env.HOTLOOP_FLOW_BIN ?? path.resolve(here, '..', '..', exe);
export const port = Number(process.env.HOTLOOP_FLOW_E2E_PORT ?? 18990);

function hash(password) {
  const out = spawnSync(bin, ['hash-password', '-password', password], { encoding: 'utf8' });
  if (out.status !== 0) {
    throw new Error(`hash-password failed (${bin}): ${out.stderr || out.error}`);
  }
  return out.stdout.trim();
}

const dir = mkdtempSync(path.join(tmpdir(), 'hotloop-flow-e2e-'));
const config = path.join(dir, 'config.yaml');
writeFileSync(config, `server:
  host: 127.0.0.1
  port: ${port}
data:
  dir: ${JSON.stringify(path.join(dir, 'data'))}
  credentialSecret: e2e-credential-secret-long-enough
auth:
  users:
    - username: admin
      passwordHash: "${hash('e2e-admin-password')}"
      permissions: ["*"]
    - username: sam
      passwordHash: "${hash('e2e-sam-password')}"
      permissions: ["*"]
logging:
  level: warn
`);

const child = spawn(bin, ['-config', config], { stdio: 'inherit' });
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => child.kill(sig));
}
child.on('exit', (code) => process.exit(code ?? 0));
