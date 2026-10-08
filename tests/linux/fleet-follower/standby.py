#!/usr/bin/env python3
"""Launch script and environment for a hot standby of a cluster this node runs, for
tests/linux/fleet-follower.sh.

    standby.py render --src-run postgres.run --src-env postgres.env --dir D --data DATA --sock SOCK --port P --name N

takes the launcher script and the systemd environment file that internal/units wrote for the
primary and writes D/run.sh and D/env for a standby of it: another port, data directory and
socket directory, the primary's own settings otherwise, plus hot_standby=on and no archiving (a
standby never pushes WAL, design 2.7.4). It prints the primary's port and socket directory as
JSON. The unit files are a /bin/sh script that ends in one `exec` line and a systemd
EnvironmentFile of KEY="value" lines with backslash and double quote escaped.
"""
import argparse
import json
import os
import re
import shlex
import sys


def read_env(path):
    out = {}
    with open(path) as f:
        for line in f.read().splitlines():
            if not line.strip() or line.startswith("#"):
                continue
            k, v = line.split("=", 1)
            if len(v) >= 2 and v[0] == '"' and v[-1] == '"':
                v = v[1:-1]
            out[k] = re.sub(r"\\(.)", r"\1", v)
    return out


def write_env(path, env):
    with open(path, "w") as f:
        for k in sorted(env):
            f.write('%s="%s"\n' % (k, env[k].replace("\\", "\\\\").replace('"', '\\"')))
    os.chmod(path, 0o600)


def read_exec(path):
    cd, exe = None, None
    with open(path) as f:
        for line in f.read().splitlines():
            if line.startswith("cd "):
                cd = shlex.split(line)[1]
            elif line.startswith("exec "):
                exe = shlex.split(line)[1:]
    if exe is None:
        sys.exit("%s: no exec line" % path)
    return cd, exe


def render(a):
    _, exe = read_exec(a.src_run)
    env = read_env(a.src_env)
    launcher, args = exe[0], exe[1:]
    settings, src_port, i = [], None, 0
    while i < len(args):
        if args[i] == "-p":
            src_port = int(args[i + 1])
            i += 2
        elif args[i] == "-c":
            settings.append(args[i + 1])
            i += 2
        else:
            sys.exit("unexpected argument %r in %s" % (args[i], a.src_run))
    src_sock = next((s.split("=", 1)[1] for s in settings if s.startswith("unix_socket_directories=")), "")
    drop = re.compile(r"(archive_mode|archive_command|archive_timeout|unix_socket_directories|cluster_name)=")
    keep = [s for s in settings if not drop.match(s)]
    keep += ["unix_socket_directories=" + a.sock, "archive_mode=off", "hot_standby=on", "hot_standby_feedback=on", "cluster_name=" + a.name]
    argv = [launcher, "-p", str(a.port)]
    for s in keep:
        argv += ["-c", s]
    env["PGDATA"] = a.data
    env.pop("POSTGRES_PASSWORD", None)
    os.makedirs(a.dir, exist_ok=True)
    write_env(os.path.join(a.dir, "env"), env)
    run = os.path.join(a.dir, "run.sh")
    with open(run, "w") as f:
        f.write("#!/bin/sh\nset -e\ncd %s\nexec %s\n" % (shlex.quote(a.dir), " ".join(shlex.quote(x) for x in argv)))
    os.chmod(run, 0o755)
    print(json.dumps({"src_port": src_port, "src_sock": src_sock}))


p = argparse.ArgumentParser()
sub = p.add_subparsers(dest="cmd", required=True)
s = sub.add_parser("render")
for name in ("src-run", "src-env", "dir", "data", "sock", "name"):
    s.add_argument("--" + name, required=True)
s.add_argument("--port", type=int, required=True)
s.set_defaults(fn=render)
a = p.parse_args()
a.fn(a)
