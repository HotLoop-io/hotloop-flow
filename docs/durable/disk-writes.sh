#!/usr/bin/env bash
# How much the runtime writes to disk, idle and under steady load.
#
# Starts the runtime on a fresh data directory for each scenario, lets it settle,
# and reads /proc/<pid>/io before and after SECS seconds. write_bytes is what the
# kernel was asked to send to storage on the process's behalf (cancelled writes
# taken off), which is the number an SD card or SSD cares about. wchar is bytes
# passed to write(2) and syscw is the number of write calls, for comparison.
#
# The flow is the one from store-and-forward.sh, with InfluxDB behind it, so the
# load is real. "plain" is the same flow with the wire left ordinary, which
# isolates what the durable wire adds from what persistent context costs.
#
#   BIN=./hotloop-flow SECS=300 bash docs/durable/disk-writes.sh
#   ONLY="durable-20 idle" picks scenarios by name
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=$(readlink -f "${BIN:-./hotloop-flow}")
SECS=${SECS:-300}
SETTLE=${SETTLE:-30}
INFLUX_IMAGE=${INFLUX_IMAGE:-docker.io/library/influxdb:2.9.1}
WORK=${WORK:-$(mktemp -d)}
NAME=${NAME:-dw-influx}
FLOWPORT=18897
mkdir -p "$WORK"
RESULTS=$WORK/results.txt
: > "$RESULTS"

say() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }
cleanup() {
  [ -n "${PRODUCER:-}" ] && kill "$PRODUCER" 2>/dev/null || true
  [ -n "${PROXY:-}" ] && kill "$PROXY" 2>/dev/null || true
  [ -n "${FLOW:-}" ] && kill "$FLOW" 2>/dev/null || true
  podman rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$WORK/secrets"; chmod 700 "$WORK/secrets"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/secrets/token"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/secrets/password"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/secrets/credsecret"
chmod 600 "$WORK/secrets/"*

podman rm -f "$NAME" >/dev/null 2>&1 || true
podman run -d --name "$NAME" --memory 6g --memory-swap 6g \
  -p 127.0.0.1:18086:8086 -v "$WORK/secrets:/run/p4:ro,z" \
  -e DOCKER_INFLUXDB_INIT_MODE=setup -e DOCKER_INFLUXDB_INIT_USERNAME=proof \
  -e DOCKER_INFLUXDB_INIT_PASSWORD_FILE=/run/p4/password \
  -e DOCKER_INFLUXDB_INIT_ADMIN_TOKEN_FILE=/run/p4/token \
  -e DOCKER_INFLUXDB_INIT_ORG=proof -e DOCKER_INFLUXDB_INIT_BUCKET=proof \
  "$INFLUX_IMAGE" >/dev/null
for _ in $(seq 1 120); do curl -fsS http://127.0.0.1:18086/health >/dev/null 2>&1 && break; sleep 1; done
python3 "$HERE/proxy.py" --listen 18087 --upstream 18086 --log /dev/null &
PROXY=$!

# scenario NAME KIND RATE SYNCHRONOUS ACK_FLUSH [up|down] [CONTEXT_FLUSH_INTERVAL]
# "down" stops InfluxDB for the whole scenario, which is the wire spilling to disk.
scenario() {
  local name=$1 kind=$2 rate=$3 sync=$4 ack=$5 down=${6:-up} flush=${7:-}
  local data=$WORK/data-$name
  rm -rf "$data"; mkdir -p "$data"
  cp "$WORK/secrets/token" "$data/influx-token"; chmod 600 "$data/influx-token"
  if [ "$kind" = plain ]; then
    python3 - "$HERE/flow.json" "$data" > "$data/flows.json" <<'PY'
import json, sys
flow = json.load(open(sys.argv[1]))
for n in flow:
    n.pop("ew_durable", None)
print(json.dumps(flow).replace("@DATA@", sys.argv[2]))
PY
  else
    sed "s#@DATA@#$data#g" "$HERE/flow.json" > "$data/flows.json"
  fi
  if [ "$down" = down ]; then podman stop -t 0 "$NAME" >/dev/null; fi
  say "$name: starting"
  (
    cd "$data"
    export HOTLOOP_FLOW_DATA_DIR="$data" HOTLOOP_FLOW_ADMIN_USER=admin \
      HOTLOOP_FLOW_ADMIN_PASSWORD_HASH="$("$BIN" hash-password -password proofproof123)" \
      HOTLOOP_FLOW_CREDENTIAL_SECRET="$(cat "$WORK/secrets/credsecret")" \
      HOTLOOP_FLOW_HOST=127.0.0.1 HOTLOOP_FLOW_PORT=$FLOWPORT HOTLOOP_FLOW_LOG_LEVEL=warn \
      HOTLOOP_FLOW_STORE_SYNCHRONOUS="$sync" HOTLOOP_FLOW_DURABLE_ACK_FLUSH="$ack" \n      HOTLOOP_FLOW_CONTEXT_FLUSH_INTERVAL="$flush"
    exec "$BIN" >> "$WORK/flow-$name.log" 2>&1
  ) &
  FLOW=$!
  for _ in $(seq 1 100); do curl -fsS "http://127.0.0.1:$FLOWPORT/health" >/dev/null 2>&1 && break; sleep 0.2; done
  if [ "$rate" != 0 ]; then
    python3 "$HERE/producer.py" --url "http://127.0.0.1:$FLOWPORT/reading" --rate "$rate" --log "$WORK/acked-$name.log" &
    PRODUCER=$!
  fi
  sleep "$SETTLE"
  read -r w1 c1 s1 < <(awk '/^write_bytes/ {w=$2} /^wchar/ {c=$2} /^syscw/ {s=$2} END {print w, c, s}' /proc/$FLOW/io)
  sleep "$SECS"
  read -r w2 c2 s2 < <(awk '/^write_bytes/ {w=$2} /^wchar/ {c=$2} /^syscw/ {s=$2} END {print w, c, s}' /proc/$FLOW/io)
  local ok=0
  [ "$rate" != 0 ] && ok=$(grep -c ' ok$' "$WORK/acked-$name.log" || true)
  [ -n "${PRODUCER:-}" ] && { kill "$PRODUCER"; wait "$PRODUCER" 2>/dev/null || true; PRODUCER=; }
  sleep 2
  kill "$FLOW"; wait "$FLOW" 2>/dev/null || true; FLOW=
  if [ "$down" = down ]; then
    podman start "$NAME" >/dev/null
    for _ in $(seq 1 120); do curl -fsS http://127.0.0.1:18086/health >/dev/null 2>&1 && break; sleep 1; done
  fi
  local dbsize
  dbsize=$(du -cb "$data"/state.db* 2>/dev/null | tail -1 | cut -f1)
  awk -v n="$name" -v k="$kind" -v r="$rate" -v sy="$sync" -v a="$ack" -v cf="${flush:-5s}" -v secs="$SECS" \
      -v w="$((w2 - w1))" -v c="$((c2 - c1))" -v s="$((s2 - s1))" -v ok="$ok" -v db="$dbsize" 'BEGIN {
    printf "%-22s %-8s %5s/s  sync=%-6s ack=%-6s ctx=%-4s %4ds  write_bytes %10d  wchar %10d  syscw %7d  = %8.1f MiB/hour, %8.1f write calls/s  (state files %d bytes)\n",
      n, k, r, sy, a, cf, secs, w, c, s, w * 3600 / secs / 1048576, s / secs, db }' | tee -a "$RESULTS"
}

# name        kind     rate  synchronous  ackFlush
sel() { [ -z "${ONLY:-}" ] || [[ " $ONLY " == *" $1 "* ]]; }
sel idle && scenario idle            durable 0   normal 100ms
sel context-only && scenario context-only    plain   20  normal 100ms
sel durable-20 && scenario durable-20      durable 20  normal 100ms
sel durable-100 && scenario durable-100     durable 100 normal 100ms
sel durable-20-ctx30 && scenario durable-20-ctx30 durable 20  normal 100ms up 30s
sel durable-50 && scenario durable-50      durable 50  normal 100ms
sel durable-50-ctx30 && scenario durable-50-ctx30 durable 50  normal 100ms up 30s
sel context-only-ctx30 && scenario context-only-ctx30 plain 20 normal 100ms up 30s
sel spill-20 && scenario spill-20        durable 20  normal 100ms down
sel spill-20-full && scenario spill-20-full   durable 20  full   100ms down

say "done, results in $RESULTS"
