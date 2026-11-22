// Records what real Node-RED prints for each example flow. Usage:
//   node oracle.js <examples dir> <out.json>
// Every flow is deployed twice in fresh runtimes with every Inject pressed and
// every Debug output recorded. A flow whose two runs differ depends on a clock
// or a random number and is listed under "volatile", not in "outputs".
const http = require("http");
const fs = require("fs");
const os = require("os");
const path = require("path");
const WebSocket = require("ws");
const RED = require("node-red");
const express = require("express");

const [, , examplesDir, outFile] = process.argv;
const PORT = 18800;
// How long after the last Inject the debug output is read, in milliseconds. A flow that waits
// longer than that (a Trigger's timeout) says more when this is raised for it:
//   SETTLE=12000 ONLY="trigger__04" node oracle.js ../examples /tmp/trigger.json
const SETTLE = Number(process.env.SETTLE || 1500);
const ONLY = process.env.ONLY || "";
// Flows whose output is the machine the oracle ran on (its home directory, the modules
// installed beside it), so they say nothing about Flow and are kept out of "outputs".
const ENVIRONMENT = ["function__change__04 - Set value from env var.json", "function__function__08 - Using external modules.json"];

function request(method, p, body, headers) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? null : JSON.stringify(body);
    const req = http.request(
      { host: "127.0.0.1", port: PORT, path: p, method, headers: Object.assign(data ? { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(data) } : {}, headers || {}) },
      (res) => { let s = ""; res.on("data", (c) => (s += c)); res.on("end", () => resolve({ status: res.statusCode, body: s })); }
    );
    req.on("error", reject);
    if (data) req.write(data);
    req.end();
  });
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// what the editor's Import does with nodes whose flow is not in the file
function withTabForLooseNodes(d) {
  const tabs = new Set(d.filter((n) => n.type === "tab" || n.type === "subflow").map((n) => n.id));
  let loose = false;
  for (const n of d) if (typeof n.z === "string" && !tabs.has(n.z)) { n.z = "imported-tab"; loose = true; }
  return loose ? [{ id: "imported-tab", type: "tab", label: "Imported" }, ...d] : d;
}

// the same canonical form cmd/hotloop-flow/nodered_examples_test.go compares in:
// the message id dropped, whitespace removed, object keys sorted
function canon(v) {
  let s = typeof v === "string" ? v : JSON.stringify(v);
  s = s.replace(/"_msgid":\s*"[0-9a-f]+",?/g, "").replace(/\s+/g, "");
  let j;
  try { j = JSON.parse(s); } catch { return s; }
  const sort = (x) => Array.isArray(x) ? x.map(sort) : x && typeof x === "object" ? Object.fromEntries(Object.keys(x).sort().map((k) => [k, sort(x[k])])) : x;
  return JSON.stringify(sort(j));
}

async function runOnce(flows) {
  const userDir = fs.mkdtempSync(path.join(os.tmpdir(), "nr-"));
  const app = express();
  const server = http.createServer(app);
  RED.init(server, { httpAdminRoot: "/", httpNodeRoot: "/n", userDir, credentialSecret: false, logging: { console: { level: "off" } }, functionExternalModules: false, uiPort: PORT });
  app.use("/", RED.httpAdmin);
  app.use("/n", RED.httpNode);
  await new Promise((r) => server.listen(PORT, "127.0.0.1", r));
  await RED.start();
  const got = [];
  const ws = new WebSocket("ws://127.0.0.1:" + PORT + "/comms");
  await new Promise((r) => ws.on("open", r));
  ws.on("message", (m) => {
    let j; try { j = JSON.parse(m.toString()); } catch { return; }
    if (Array.isArray(j)) for (const e of j) if (e.topic === "debug") got.push(e.data);
  });
  ws.send(JSON.stringify({ subscribe: "debug" }));
  await sleep(300);
  const r = await request("POST", "/flows", flows, { "Node-RED-Deployment-Type": "full" });
  if (r.status !== 204 && r.status !== 200) throw new Error("deploy " + r.status + " " + r.body);
  await sleep(500);
  for (const n of flows) if (n.type === "inject") await request("POST", "/inject/" + n.id, {});
  await sleep(SETTLE);
  ws.close();
  await RED.stop();
  await new Promise((r) => server.close(r));
  fs.rmSync(userDir, { recursive: true, force: true });
  // A node's error that no Catch node took reaches the debug sidebar as a log line with a
  // level (10 fatal, 20 error). It is recorded as "[error] <text>" so a test can tell it from
  // a Debug node's output: Flow words its own errors, so only that the line exists is compared.
  return got.map((d) => (typeof d.level === "number" && d.level <= 20 ? "[error] " : "") + canon(d.msg)).sort();
}

(async () => {
  const outputs = {}, volatile = [], environment = [], failed = {};
  for (const f of fs.readdirSync(examplesDir).filter((x) => x.endsWith(".json") && x.includes(ONLY)).sort()) {
    const flows = withTabForLooseNodes(JSON.parse(fs.readFileSync(path.join(examplesDir, f), "utf8")));
    try {
      const a = await runOnce(JSON.parse(JSON.stringify(flows)));
      const b = await runOnce(JSON.parse(JSON.stringify(flows)));
      if (ENVIRONMENT.includes(f)) environment.push(f);
      else if (JSON.stringify(a) === JSON.stringify(b)) outputs[f] = a; else volatile.push(f);
    } catch (e) { failed[f] = String(e.message || e); }
    process.stderr.write(f + "\n");
  }
  fs.writeFileSync(outFile, JSON.stringify({ source: "real Node-RED 5.0.10, each example flow of node-red/node-red @nodes/examples deployed, every Inject pressed, every Debug output recorded", outputs, volatile, environment, failed }, null, 1) + "\n");
})().catch((e) => { console.error(e); process.exit(1); });
