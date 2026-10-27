#!/usr/bin/env bash
# Takes the README's screenshots (docs/screenshots/*.png) of the real editor in a
# real Chrome, in light and in dark, the same way every time.
#
#   web/shots/screenshots.sh
#
# It builds the editor and the binary, starts the binary and a headless Chrome
# beside it on a network of their own, and removes both when it is done.
# serve.mjs starts the two runtimes the pictures are of, screenshots.spec.ts
# takes them, and plant-flow.json is the oven line that is on the canvas. The
# demo lives in those files and never in the product.
#
# The version in the top bar is the one the next release will carry. Set
# SHOTS_VERSION for another.
#
# HOTLOOP_FLOW_TEST_PREFIX names the containers, so two people's runs on one box
# never meet.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
E=podman
P=${HOTLOOP_FLOW_TEST_PREFIX:-flow-test}-shots
IMG_NODE=docker.io/library/node:24.21.0-bookworm-slim
IMG_BROWSER=docker.io/chromedp/headless-shell:151.0.7922.109
IMG_GO=docker.io/library/golang:1.27.1
VERSION=${SHOTS_VERSION:-v2.5.0}
RUNNER_HOST=flow-runner.test
PORT=47124
DASH_PORT=$((PORT + 1))

mkdir -p "$ROOT/dist"
BIN=hotloop-flow-shots

# The editor is embedded in the binary, so it is built first, and current.
$E run --rm -v "$ROOT/web:/web" -v "$P-node-modules:/web/node_modules" -w /web "$IMG_NODE" \
  bash -c 'npm ci --no-audit --no-fund >&2 && npm run build >&2'

$E run --rm -v "$ROOT:/src" -w /src -v "$P-gomod:/go/pkg/mod" -v "$P-gobuild:/root/.cache/go-build" \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false "$IMG_GO" \
  go build -trimpath -ldflags "-X main.version=$VERSION" -o "dist/$BIN" ./cmd/hotloop-flow

cleanup() {
  $E rm -f "$P-browser" >/dev/null 2>&1 || true
  $E network rm "$P" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
$E network create "$P" >/dev/null

# Chrome reaches the runtimes as localhost, through a forward in its own
# container: the editor's sign-in wants a secure origin, and localhost is the
# one plain-HTTP origin a browser counts as secure.
$E run -d --name "$P-browser" --network "$P" --shm-size=1g --entrypoint /bin/bash "$IMG_BROWSER" -c \
  "socat TCP4-LISTEN:$PORT,fork,reuseaddr TCP4:$RUNNER_HOST:$PORT & socat TCP4-LISTEN:$DASH_PORT,fork,reuseaddr TCP4:$RUNNER_HOST:$DASH_PORT & exec /headless-shell/run.sh" >/dev/null

mkdir -p "$ROOT/docs/screenshots"
$E run --rm --network "$P" --network-alias "$RUNNER_HOST" --memory 4g --memory-swap 4g \
  -v "$ROOT/web/shots:/shots" -v "$P-shots-node-modules:/shots/node_modules" \
  -v "$ROOT/dist/$BIN:/usr/local/bin/$BIN:ro" \
  -v "$ROOT/docs/screenshots:/out" -w /shots \
  -e SHOTS_CDP="$P-browser:9222" -e SHOTS_URL="http://localhost:$PORT" -e SHOTS_DASH_URL="http://localhost:$DASH_PORT" \
  -e SHOTS_OUT=/out -e SHOTS_RESULTS=/tmp/flow-shots-results -e PORT="$PORT" -e BIN="$BIN" \
  "$IMG_NODE" bash -c '
    set -euo pipefail
    npm ci --no-audit --no-fund >&2
    node serve.mjs "/usr/local/bin/$BIN" "$PORT" /tmp/flow-shots > /tmp/flow-shots.log 2>&1 &
    server=$!
    trap "kill $server 2>/dev/null || true" EXIT
    up() { node -e "fetch(\"http://127.0.0.1:$1/health\").then((r) => process.exit(r.ok ? 0 : 1), () => process.exit(1))" "$1"; }
    for i in $(seq 1 100); do up "$PORT" && up "$((PORT + 1))" && break; sleep 0.2; done
    up "$PORT" && up "$((PORT + 1))" || { cat /tmp/flow-shots.log >&2; exit 1; }
    status=0
    npx playwright test --config playwright.config.ts || status=$?
    [ $status -eq 0 ] || { echo "--- the runtimes'"'"' log" >&2; tail -40 /tmp/flow-shots.log >&2; }
    exit $status
  '
