// Starts the two Flows the README's screenshots are of, both the real binary:
//
//   PORT       the runtime with its own sign-in and three users, which the
//              editor shots are taken on
//   PORT + 1   a runtime that lets the EmberCORE dashboard sign people in, for
//              the "open this from the dashboard" shot. It gets a made-up
//              public key and nobody ever signs a token for it.
//
// usage: node serve.mjs BINARY PORT DIR
import { generateKeyPairSync } from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';

const [bin, portArg, dir] = process.argv.slice(2);
const port = Number(portArg);
mkdirSync(dir, { recursive: true });

function hash(password) {
  const out = spawnSync(bin, ['hash-password', '-password', password], { encoding: 'utf8' });
  if (out.status !== 0) throw new Error(`hash-password failed: ${out.stderr || out.error}`);
  return out.stdout.trim();
}

const users = ['admin', 'sam', 'kim'].map(
  (u) => `    - username: ${u}\n      passwordHash: "${hash(`demo-${u}-password`)}"\n      permissions: ["*"]`,
).join('\n');

const { publicKey } = generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
const keyFile = path.join(dir, 'dashboard.pub');
writeFileSync(keyFile, publicKey.export({ type: 'spki', format: 'pem' }));

function start(name, p, extra) {
  const config = path.join(dir, `${name}.yaml`);
  writeFileSync(config, `server:
  host: 0.0.0.0
  port: ${p}
data:
  dir: ${JSON.stringify(path.join(dir, name))}
  credentialSecret: demo-credential-secret-long-enough
auth:
  users:
${users}
${extra}logging:
  level: warn
`);
  const child = spawn(bin, ['-config', config], { stdio: 'inherit' });
  child.on('exit', (code) => process.exit(code ?? 1));
  return child;
}

const children = [
  start('runtime', port, ''),
  start('dashboard', port + 1, `  assertion:\n    publicKeyFile: ${JSON.stringify(keyFile)}\n    audience: demo/flow\n`),
];
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => children.forEach((c) => c.kill(sig)));
}
