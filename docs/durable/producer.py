#!/usr/bin/env python3
"""Steady input for the store-and-forward proof.

Posts {"seq": N} to the flow at a fixed rate and writes one line per attempt to
the log: "N ok" when the flow answered 200, "N fail" when it didn't. A failed
sequence number is never sent again. Re-sending would put a duplicate in
InfluxDB that the flow didn't make, and the count is there to catch the flow's
duplicates.
"""
import argparse
import http.client
import json
import signal
import sys
import time
from urllib.parse import urlparse

ap = argparse.ArgumentParser()
ap.add_argument("--url", required=True)
ap.add_argument("--rate", type=float, default=20.0, help="messages per second")
ap.add_argument("--log", required=True)
ap.add_argument("--start", type=int, default=1)
args = ap.parse_args()

u = urlparse(args.url)
stop = False


def on_term(*_):
    global stop
    stop = True


signal.signal(signal.SIGTERM, on_term)
signal.signal(signal.SIGINT, on_term)

conn = None
seq = args.start
interval = 1.0 / args.rate
next_at = time.monotonic()
with open(args.log, "a", buffering=1) as log:
    while not stop:
        try:
            if conn is None:
                conn = http.client.HTTPConnection(u.hostname, u.port, timeout=5)
            conn.request("POST", u.path, json.dumps({"seq": seq}), {"Content-Type": "application/json"})
            r = conn.getresponse()
            r.read()
            log.write(f"{seq} {'ok' if r.status == 200 else 'fail'}\n")
        except Exception:
            log.write(f"{seq} fail\n")
            try:
                conn.close()
            except Exception:
                pass
            conn = None
        seq += 1
        next_at += interval
        delay = next_at - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        else:
            next_at = time.monotonic()
