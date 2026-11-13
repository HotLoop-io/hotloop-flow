#!/bin/bash
# A lab for binding a Modbus client to one VLAN of several that share a subnet.
#
# The Shepherd Boy Farms box has one port that carries a VLAN for each dryer.
# Every VLAN is the same 192.168.1.0/24 and the devices keep their addresses
# from one VLAN to the next. This builds the same thing from network namespaces:
# for each VLAN number N, a namespace flxlab-N holds a Modbus TCP simulator at
# 192.168.1.5:5020 that answers N (register 0 holds 1100 + N, input register 0
# holds 2200 + N), and a veth pair joins it to the host, whose end, flxvN, is
# 192.168.1.100/24. So the host has the same subnet on every flxvN, and a
# connection to 192.168.1.5 reaches whichever simulator the interface it leaves
# by is cabled to. Without SO_BINDTODEVICE the kernel picks one of them.
#
#   lab.sh up [N ...]       build the namespaces, interfaces and simulators (default 14 15), as root
#   lab.sh down             take all of it away, as root. Touches only flxlab-*, flxv* and flxlab-sim-*
#   lab.sh status           what is there
#   lab.sh flap N down|up   take the link of VLAN N down or up, from a process that is not root: it
#                           hands the command to the helper `up` started, which does only that
#   lab.sh read N HOST PORT ADDR QTY
#                           read QTY holding registers from ADDR of the Modbus TCP server at HOST:PORT, as a
#                           client on VLAN N (inside its namespace), and print them. Also through the helper.
#
# The host end of each VLAN also holds 192.168.1.9/24 (LAB_SERVER_ADDR), the address a Flow's
# modbus-server listens on in the plant, so a server bound to flxv14 and another to flxv15 can both
# listen on 192.168.1.9:10502 and a client in each namespace reaches only its own.
#
# The simulator is oitc/modbus-server (pymodbus) run by Podman in the namespace.
# The register layout is testdata/plant/modbus_server.json with the two values changed.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
ctl=/run/flxlab
image=${LAB_IMAGE:-docker.io/oitc/modbus-server:2.3.0}
port=${LAB_PORT:-5020}
owner=${LAB_OWNER:-${SUDO_USER:-$(id -un)}}

need_root() { [ "$(id -u)" = 0 ] || { echo "lab.sh $1 has to run as root" >&2; exit 1; }; }

up() {
  need_root up
  local vlans=("$@"); [ ${#vlans[@]} -gt 0 ] || vlans=(14 15)
  mkdir -p "$ctl/cmd" "$ctl/cfg"
  cat >"$ctl/modbus_read.py" <<'PY'
import socket, struct, sys
host, port, addr, qty = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
s = socket.create_connection((host, port), timeout=3)
s.sendall(struct.pack(">HHHBBHH", 1, 0, 6, 1, 3, addr, qty))
head = s.recv(9)
if len(head) < 9 or head[7] & 0x80:
    sys.exit("modbus exception or short answer: %r" % head)
n = head[8]
body = b""
while len(body) < n:
    chunk = s.recv(n - len(body))
    if not chunk:
        break
    body += chunk
print(" ".join(str(v) for v in struct.unpack(">%dH" % (len(body) // 2), body)))
PY
  for v in "${vlans[@]}"; do
    if ip netns list | grep -q "^flxlab-$v\b"; then echo "VLAN $v is already up"; continue; fi
    python3 - "$repo/testdata/plant/modbus_server.json" "$ctl/cfg/$v.json" "$v" "$port" <<'PY'
import json, sys
src, dst, v, port = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
d = json.load(open(src))
d["server"]["listenerPort"] = port
d["registers"]["holdingRegister"]["1"] = 1100 + v
d["registers"]["inputRegister"]["1"] = 2200 + v
json.dump(d, open(dst, "w"), indent=1)
PY
    ip netns add "flxlab-$v"
    ip link add "flxv$v" type veth peer name "flxp$v"
    ip link set "flxp$v" netns "flxlab-$v"
    ip addr add 192.168.1.100/24 dev "flxv$v"
    ip addr add "${LAB_SERVER_ADDR:-192.168.1.9}/24" dev "flxv$v"
    ip link set "flxv$v" up
    ip -n "flxlab-$v" addr add 192.168.1.5/24 dev "flxp$v"
    ip -n "flxlab-$v" link set "flxp$v" up
    ip -n "flxlab-$v" link set lo up
    podman rm -f "flxlab-sim-$v" >/dev/null 2>&1 || true
    podman run -d --name "flxlab-sim-$v" --network "ns:/run/netns/flxlab-$v" \
      -v "$ctl/cfg/$v.json:/app/modbus_server.json:ro" "$image" >/dev/null
    echo "VLAN $v: flxv$v 192.168.1.100/24 <-> flxlab-$v 192.168.1.5:$port (register 0 = $((1100 + v)))"
  done
  # The helper that takes a link down and up for a process that is not root.
  # It runs one fixed command on interfaces named flxv<digits> and nothing else.
  chown "$owner" "$ctl/cmd"
  if [ ! -f "$ctl/helper.pid" ] || ! kill -0 "$(cat "$ctl/helper.pid")" 2>/dev/null; then
    nohup "$0" helper >/dev/null 2>&1 &
    echo $! >"$ctl/helper.pid"
  fi
  # Wait for each simulator to answer, so a test starts against a lab that is up.
  for v in "${vlans[@]}"; do
    for _ in $(seq 1 60); do
      if ip netns exec "flxlab-$v" bash -c "exec 3<>/dev/tcp/192.168.1.5/$port" 2>/dev/null; then break; fi
      sleep 0.5
    done
  done
}

down() {
  need_root down
  if [ -f "$ctl/helper.pid" ]; then kill "$(cat "$ctl/helper.pid")" 2>/dev/null || true; fi
  for c in $(podman ps -a --format '{{.Names}}' | grep '^flxlab-sim-' || true); do podman rm -f "$c" >/dev/null; done
  for ns in $(ip netns list | awk '{print $1}' | grep '^flxlab-' || true); do
    v=${ns#flxlab-}
    ip link del "flxv$v" 2>/dev/null || true
    ip netns del "$ns"
  done
  rm -rf "$ctl"
  echo "lab is down"
}

status() {
  ip -br addr | grep '^flxv' || echo "no flxv interfaces"
  ip netns list | grep '^flxlab-' || echo "no flxlab namespaces"
  podman ps --format '{{.Names}} {{.Status}}' 2>/dev/null | grep '^flxlab-sim-' || echo "no simulators"
}

helper() {
  need_root helper
  while true; do
    for f in "$ctl"/cmd/*.req; do
      [ -e "$f" ] || continue
      read -r action v host port reg qty <"$f" || true
      id=${f%.req}
      if [[ "$action" =~ ^(up|down)$ && "$v" =~ ^[0-9]+$ ]] && ip link show "flxv$v" >/dev/null 2>&1; then
        if ip link set "flxv$v" "$action"; then echo ok >"$id.done"; else echo failed >"$id.done"; fi
      elif [[ "$action" = read && "$v" =~ ^[0-9]+$ && "$host" =~ ^[0-9.]+$ && "$port" =~ ^[0-9]+$ && "$reg" =~ ^[0-9]+$ && "$qty" =~ ^[0-9]+$ ]] \
          && ip netns list | grep -q "^flxlab-$v\b"; then
        if ip netns exec "flxlab-$v" python3 "$ctl/modbus_read.py" "$host" "$port" "$reg" "$qty" >"$id.out" 2>&1; then echo ok >"$id.done"; else echo failed >"$id.done"; fi
      else
        echo refused >"$id.done"
      fi
      rm -f "$f"
    done
    sleep 0.1
  done
}

flap() {
  local v=${1:-} action=${2:-}
  [[ "$action" =~ ^(up|down)$ && "$v" =~ ^[0-9]+$ ]] || { echo "usage: lab.sh flap N down|up" >&2; exit 2; }
  local id="$ctl/cmd/$$-$RANDOM"
  echo "$action $v" >"$id.tmp" && mv "$id.tmp" "$id.req"
  for _ in $(seq 1 100); do
    if [ -f "$id.done" ]; then
      r=$(cat "$id.done"); rm -f "$id.done"
      [ "$r" = ok ] && exit 0
      echo "the helper said: $r" >&2; exit 1
    fi
    sleep 0.1
  done
  echo "the helper did not answer. Is the lab up?" >&2; exit 1
}

read_cmd() {
  local v=${1:-} host=${2:-} port=${3:-} reg=${4:-} qty=${5:-}
  local id="$ctl/cmd/$$-$RANDOM"
  echo "read $v $host $port $reg $qty" >"$id.tmp" && mv "$id.tmp" "$id.req"
  for _ in $(seq 1 100); do
    if [ -f "$id.done" ]; then
      r=$(cat "$id.done"); rm -f "$id.done"
      out=$(cat "$id.out" 2>/dev/null || true); rm -f "$id.out"
      if [ "$r" = ok ]; then echo "$out"; exit 0; fi
      echo "the helper said: $r $out" >&2; exit 1
    fi
    sleep 0.1
  done
  echo "the helper did not answer. Is the lab up?" >&2; exit 1
}

case "${1:-}" in
  read) shift; read_cmd "$@" ;;
  up) shift; up "$@" ;;
  down) down ;;
  status) status ;;
  helper) helper ;;
  flap) shift; flap "$@" ;;
  *) sed -n '2,24p' "$0" >&2; exit 2 ;;
esac
