#!/usr/bin/env python3
"""The continuous writer of the replication test: inserts one row at a time through a node's proxy.

    writer.py --host REF.api.DOMAIN --key-file FILE --out DIR --start 10000000 [--rate 20] [--table writes]

Each loop sends POST /rest/v1/<table> to 127.0.0.1:80 with the Host header of the project and the project's
secret key (the file holds only the key, mode 0600). The row is {"id": N, "note": "w"} and N counts up from
--start. The outcome of every attempt is one line in DIR/acked (the answer was 201) or DIR/failed (anything
else: a status other than 201, a refused connection, a timeout), as "N UNIX-TIME STATUS" with status 0 for no
answer. A failed attempt may still have been committed (the answer was lost), so its id is not retried; the
next attempt takes the next id. The test compares the two files with the table afterwards
(writer-compare.py). SIGTERM ends the loop after the attempt in progress.
"""
import argparse
import http.client
import json
import signal
import sys
import time

stop = False


def on_term(*_):
    global stop
    stop = True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", required=True)
    ap.add_argument("--key-file", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--start", type=int, default=1)
    ap.add_argument("--rate", type=float, default=20.0)
    ap.add_argument("--table", default="writes")
    ap.add_argument("--port", type=int, default=80)
    ap.add_argument("--timeout", type=float, default=3.0)
    a = ap.parse_args()
    key = open(a.key_file).read().strip()
    signal.signal(signal.SIGTERM, on_term)
    signal.signal(signal.SIGINT, on_term)
    acked = open(a.out + "/acked", "a", buffering=1)
    failed = open(a.out + "/failed", "a", buffering=1)
    headers = {"Host": a.host, "apikey": key, "Content-Type": "application/json", "Prefer": "return=minimal"}
    interval = 1.0 / a.rate
    n = a.start
    while not stop:
        t0 = time.time()
        status = 0
        try:
            c = http.client.HTTPConnection("127.0.0.1", a.port, timeout=a.timeout)
            c.request("POST", "/rest/v1/" + a.table, json.dumps({"id": n, "note": "w"}), headers)
            r = c.getresponse()
            r.read()
            status = r.status
            c.close()
        except Exception:
            status = 0
        (acked if status == 201 else failed).write("%d %.3f %d\n" % (n, time.time(), status))
        n += 1
        time.sleep(max(0.0, interval - (time.time() - t0)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
