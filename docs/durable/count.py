#!/usr/bin/env python3
"""Count what InfluxDB holds against what the flow said it accepted.

Reads the producer's log and asks the InfluxDB container for every stored seq.
The reading carries no timestamp, so InfluxDB stamps each write on arrival and a
message delivered twice is two rows. That is the point: a second row for a seq
is a duplicate the flow made, and it shows up here.

  accepted   seqs the flow answered 200 for. Every one must be stored.
  missing    accepted seqs InfluxDB doesn't have.
  doubled    seqs stored more than once.
  unacked    stored seqs the flow never answered 200 for. A request in flight when
             the process was killed can land either side of the kill, so these are
             allowed, and counted so it's clear they're not hidden.
"""
import argparse
import collections
import subprocess
import sys

ap = argparse.ArgumentParser()
ap.add_argument("--container", required=True)
ap.add_argument("--token-file", required=True, help="path inside the container")
ap.add_argument("--log", required=True)
ap.add_argument("--writes", help="the proxy log, to count deliveries as well as rows")
args = ap.parse_args()

flux = ('from(bucket: "proof") |> range(start: 2020-01-01T00:00:00Z) '
        '|> filter(fn: (r) => r._measurement == "readings" and r._field == "seq") '
        '|> keep(columns: ["_value"])')
out = subprocess.run(
    ["podman", "exec", args.container, "sh", "-c",
     f'influx query --org proof --token "$(cat {args.token_file})" --raw \'{flux}\''],
    capture_output=True, text=True, check=True).stdout

stored = collections.Counter()
for line in out.splitlines():
    if not line or line.startswith("#"):
        continue
    cols = line.split(",")
    if len(cols) < 4 or cols[3] in ("_value", ""):
        continue
    stored[int(float(cols[3]))] += 1

accepted, failed = set(), set()
for line in open(args.log):
    n, status = line.split()
    (accepted if status == "ok" else failed).add(int(n))

missing = sorted(accepted - set(stored))
doubled = sorted(s for s, c in stored.items() if c > 1)
unacked = sorted(set(stored) - accepted)
resent = []
if args.writes:
    deliveries = collections.Counter()
    for line in open(args.writes):
        deliveries[int(line.split()[0])] += 1
    resent = sorted(s for s, c in deliveries.items() if c > 1)
    print(f"deliveries      {sum(deliveries.values())} writes InfluxDB answered 204 to")
    print(f"delivered twice {len(resent)} {resent[:20]}")
print(f"sent            {len(accepted) + len(failed)}")
print(f"accepted (200)  {len(accepted)}")
print(f"failed          {len(failed)}")
print(f"rows stored     {sum(stored.values())}")
print(f"distinct seqs   {len(stored)}")
print(f"missing         {len(missing)} {missing[:20]}")
print(f"doubled         {len(doubled)} {doubled[:20]}")
print(f"unacked stored  {len(unacked)} {unacked[:20]}")
sys.exit(1 if missing or doubled else 0)
