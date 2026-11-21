# Node-RED oracle

`oracle.js` runs every example flow in `../examples` on real Node-RED 5.0.10 (pinned in
`package.json`): the flow is deployed, every Inject is pressed, and every Debug output is
recorded. Each flow runs twice in fresh runtimes; a flow whose two runs differ depends on a
clock or a random number and is listed under `volatile`, not in `outputs`.

    podman run --rm -v <repo>:/r:Z -w /r/testdata/nodered/oracle docker.io/library/node:24-alpine \
      sh -c "npm install --no-audit --no-fund && node oracle.js ../examples /tmp/oracle.json"

Copy `/tmp/oracle.json` over `../oracle.json`. The Go test
`TestNodeREDExampleFlowsGiveWhatNodeREDGives` holds Flow to it. Checked 2026-11-21: the harness
reproduced all 39 entries the earlier hand-run recorded, byte for byte, before it was trusted.
