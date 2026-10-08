#!/usr/bin/env python3
"""Compares what the writer saw with what the table holds, and prints key=value lines.

    writer-compare.py --acked FILE --failed FILE --db FILE [--after UNIX-TIME]

--acked and --failed are the writer's files ("N UNIX-TIME STATUS" per attempt), --db holds the ids the table
has, one per line. Printed:

    attempts, acked, failed   the writer's counts
    db_rows                   ids in the table
    lost                      acknowledged ids that are not in the table (RPO, in rows)
    lost_window_s             seconds between the first and the last lost acknowledgement
    unacked_present           failed attempts whose row is in the table (committed, answer lost)
    phantom                   ids in the table that no attempt wrote
    duplicates                ids acknowledged more than once
    max_gap_s, max_gap_at     the longest silence between two acknowledgements, and when it began
    rto_s                     with --after: seconds from that time to the first acknowledgement after it
"""
import argparse


def read(path):
    rows = []
    try:
        with open(path) as f:
            for line in f:
                p = line.split()
                if len(p) == 3:
                    rows.append((int(p[0]), float(p[1]), int(p[2])))
    except FileNotFoundError:
        pass
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--acked", required=True)
    ap.add_argument("--failed", required=True)
    ap.add_argument("--db", required=True)
    ap.add_argument("--after", type=float)
    a = ap.parse_args()
    acked, failed = read(a.acked), read(a.failed)
    with open(a.db) as f:
        db = {int(x) for x in f.read().split()}
    acked_ids = [r[0] for r in acked]
    attempted = set(acked_ids) | {r[0] for r in failed}
    lost = [r for r in acked if r[0] not in db]
    out = {
        "attempts": len(acked) + len(failed),
        "acked": len(acked),
        "failed": len(failed),
        "db_rows": len(db),
        "lost": len(lost),
        "lost_window_s": "%.3f" % (max(r[1] for r in lost) - min(r[1] for r in lost)) if lost else "0.000",
        "unacked_present": sum(1 for r in failed if r[0] in db),
        "phantom": len(db - attempted),
        "duplicates": len(acked_ids) - len(set(acked_ids)),
    }
    times = sorted(r[1] for r in acked)
    gap, at = 0.0, 0.0
    for t0, t1 in zip(times, times[1:]):
        if t1 - t0 > gap:
            gap, at = t1 - t0, t0
    out["max_gap_s"] = "%.3f" % gap
    out["max_gap_at"] = "%.3f" % at
    if a.after is not None:
        later = [t for t in times if t > a.after]
        out["rto_s"] = "%.3f" % (later[0] - a.after) if later else "none"
    for k, v in out.items():
        print("%s=%s" % (k, v))


if __name__ == "__main__":
    main()
