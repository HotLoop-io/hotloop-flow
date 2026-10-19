#!/usr/bin/env python3
"""A pass-through between the flow and InfluxDB that counts what it forwards.

InfluxDB treats a point written twice at the same time as one point, so a
duplicate delivery doesn't show up as a second row. This proxy is how the proof
sees the duplicates anyway: every write InfluxDB answered 204 to is logged as
"seq timestamp", and count.py reads the log. If InfluxDB is down the proxy
answers 502, which is what a dead upstream looks like to the flow.
"""
import argparse
import http.client
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ap = argparse.ArgumentParser()
ap.add_argument("--listen", type=int, required=True)
ap.add_argument("--upstream", type=int, required=True)
ap.add_argument("--log", required=True)
args = ap.parse_args()

lock = threading.Lock()
logf = open(args.log, "a", buffering=1)
point = re.compile(r"^readings seq=([0-9.e+]+) (\d+)$")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        try:
            up = http.client.HTTPConnection("127.0.0.1", args.upstream, timeout=5)
            up.request("POST", self.path, body, {k: v for k, v in self.headers.items()
                                                  if k.lower() in ("authorization", "content-type")})
            r = up.getresponse()
            data = r.read()
            status = r.status
            up.close()
        except Exception:
            status, data = 502, b"upstream is down"
        if status == 204:
            with lock:
                for line in body.decode().splitlines():
                    m = point.match(line)
                    if m:
                        logf.write(f"{int(float(m.group(1)))} {m.group(2)}\n")
        self.send_response(status)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        # The flow gets killed on purpose. A reset connection is the expected end
        # of a request that was in flight, not something to print a trace for.
        pass


Server(("127.0.0.1", args.listen), Handler).serve_forever()
