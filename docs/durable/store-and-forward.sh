#!/usr/bin/env bash
# Store-and-forward, the ugly way.
#
#   1. Real InfluxDB in Podman, a flow writing to it over a durable wire, and a
#      producer posting readings at a steady rate.
#   2. kill -9 the InfluxDB container for $OUTAGE seconds. The readings keep
#      coming and the wire spills them to disk. Halfway through the outage, with
#      the queue on disk, kill -9 the flow runtime and start it again.
#   3. Bring InfluxDB back. While the backlog is draining, kill -9 the runtime
#      mid-queue a second time and start it again.
#   4. Let everything settle and count: nothing the flow answered 200 for may be
#      missing from InfluxDB, and no reading may be stored twice.
#
# Between the flow and InfluxDB sits a small pass-through (proxy.py) that logs
# every write InfluxDB accepted. InfluxDB stores a point written twice at the
# same time once, so rows alone can't show a duplicate delivery; the log can.
#
# Needs podman, python3, curl, and a hotloop-flow binary (BIN, default
# ./hotloop-flow). Linux only. Uses ports 18086 (InfluxDB), 18087 (the
# pass-through) and 18897 (the flow) on 127.0.0.1 and removes its container when
# it finishes.
#
#   BIN=./hotloop-flow RATE=20 OUTAGE=600 bash docs/durable/store-and-forward.sh
#
# ACK_FLUSH (a duration such as 0s or 250ms) sets store.durable.ackFlush for the
# run; unset, the default applies.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=$(readlink -f "${BIN:-./hotloop-flow}")
RATE=${RATE:-20}              # readings per second
WARM=${WARM:-60}              # seconds of normal running before the outage
OUTAGE=${OUTAGE:-600}         # seconds InfluxDB is dead
TAIL=${TAIL:-90}              # seconds of normal running after the backlog drains
INFLUX_IMAGE=${INFLUX_IMAGE:-docker.io/library/influxdb:2.9.1}
WORK=${WORK:-$(mktemp -d)}
NAME=${NAME:-sf-influx}
FLOWPORT=18897

DATA=$WORK/data
mkdir -p "$DATA" "$WORK/secrets"
chmod 700 "$WORK/secrets"
LOG=$WORK/acked.log
: > "$LOG"

okcount() { grep -c ' ok$' "$LOG" || true; }
say() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }

cleanup() {
  [ -n "${PRODUCER:-}" ] && kill "$PRODUCER" 2>/dev/null || true
  [ -n "${PROXY:-}" ] && kill "$PROXY" 2>/dev/null || true
  [ -n "${FLOW:-}" ] && kill -9 "$FLOW" 2>/dev/null || true
  podman rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Throwaway secrets for a throwaway container. Written to files and never
# printed: the token reaches InfluxDB and the flow by file.
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/secrets/token"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/secrets/password"
cp "$WORK/secrets/token" "$DATA/influx-token"
chmod 600 "$WORK/secrets/"* "$DATA/influx-token"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/secrets/credsecret"

sed "s#@DATA@#$DATA#g" "$HERE/flow.json" > "$DATA/flows.json"

say "starting $INFLUX_IMAGE"
podman rm -f "$NAME" >/dev/null 2>&1 || true
podman run -d --name "$NAME" --memory 6g --memory-swap 6g \
  -p 127.0.0.1:18086:8086 \
  -v "$WORK/secrets:/run/p4:ro,z" \
  -e DOCKER_INFLUXDB_INIT_MODE=setup \
  -e DOCKER_INFLUXDB_INIT_USERNAME=proof \
  -e DOCKER_INFLUXDB_INIT_PASSWORD_FILE=/run/p4/password \
  -e DOCKER_INFLUXDB_INIT_ADMIN_TOKEN_FILE=/run/p4/token \
  -e DOCKER_INFLUXDB_INIT_ORG=proof \
  -e DOCKER_INFLUXDB_INIT_BUCKET=proof \
  "$INFLUX_IMAGE" >/dev/null
podman exec "$NAME" influxd version

wait_influx() {
  for _ in $(seq 1 120); do
    curl -fsS http://127.0.0.1:18086/health >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "InfluxDB did not come up" >&2; exit 1
}
wait_influx

python3 "$HERE/proxy.py" --listen 18087 --upstream 18086 --log "$WORK/writes.log" &
PROXY=$!

start_flow() {
  (
    cd "$DATA"
    export HOTLOOP_FLOW_DATA_DIR="$DATA" HOTLOOP_FLOW_ADMIN_USER=admin \
      HOTLOOP_FLOW_ADMIN_PASSWORD_HASH="$("$BIN" hash-password -password proofproof123)" \
      HOTLOOP_FLOW_CREDENTIAL_SECRET="$(cat "$WORK/secrets/credsecret")" \
      HOTLOOP_FLOW_HOST=127.0.0.1 HOTLOOP_FLOW_PORT=$FLOWPORT HOTLOOP_FLOW_LOG_LEVEL=info       HOTLOOP_FLOW_DURABLE_ACK_FLUSH="${ACK_FLUSH:-}"
    exec "$BIN" >> "$WORK/flow.log" 2>&1
  ) &
  FLOW=$!
  for _ in $(seq 1 100); do
    curl -fsS "http://127.0.0.1:$FLOWPORT/health" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "the flow did not come up" >&2; tail -20 "$WORK/flow.log" >&2; exit 1
}

depth() {
  curl -fsS "http://127.0.0.1:$FLOWPORT/metrics" 2>/dev/null \
    | awk '/^hotloop_flow_durable_wire_depth\{/ {s+=$2} END {print s+0}'
}

say "starting the flow"
start_flow
say "flow pid $FLOW"
python3 "$HERE/producer.py" --url "http://127.0.0.1:$FLOWPORT/reading" --rate "$RATE" --log "$LOG" &
PRODUCER=$!

say "steady input for ${WARM}s"
sleep "$WARM"
say "queue depth $(depth), accepted so far $(okcount)"

say "kill -9 InfluxDB, down for ${OUTAGE}s"
podman kill "$NAME" >/dev/null
sleep $((OUTAGE / 2))
say "queue depth $(depth), kill -9 on the runtime mid-outage (pid $FLOW)"
kill -9 "$FLOW"
wait "$FLOW" 2>/dev/null || true
sleep 5
say "starting the runtime again, InfluxDB still down"
start_flow
say "flow pid $FLOW, queue depth $(depth)"
sleep $((OUTAGE - OUTAGE / 2))
PEAK=$(depth)
say "outage over, queue depth $PEAK, accepted so far $(okcount)"

say "starting InfluxDB again"
podman start "$NAME" >/dev/null
wait_influx

# The backlog drains one message at a time, in order. Cut the runtime down once
# it is about half gone, which is the middle of the queue.
TARGET=$((PEAK / 2))
for _ in $(seq 1 600); do
  d=$(depth)
  [ "$d" -le "$TARGET" ] && break
  sleep 0.2
done
say "queue depth $(depth), kill -9 on the runtime (pid $FLOW)"
kill -9 "$FLOW"
wait "$FLOW" 2>/dev/null || true
sleep 5
say "starting the runtime again"
start_flow
say "flow pid $FLOW, queue depth $(depth)"

for _ in $(seq 1 1800); do
  [ "$(depth)" -le 1 ] && break
  sleep 1
done
say "backlog drained, running ${TAIL}s more"
sleep "$TAIL"
kill "$PRODUCER"; wait "$PRODUCER" 2>/dev/null || true; PRODUCER=
for _ in $(seq 1 60); do
  [ "$(depth)" -eq 0 ] && break
  sleep 1
done
sleep 3
say "final queue depth $(depth)"

say "counting"
set +e
python3 "$HERE/count.py" --container "$NAME" --token-file /run/p4/token --log "$LOG" --writes "$WORK/writes.log" | tee "$WORK/result.txt"
rc=${PIPESTATUS[0]}
set -e
say "result in $WORK/result.txt, flow log in $WORK/flow.log"
exit "$rc"
