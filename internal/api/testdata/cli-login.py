#!/usr/bin/env python3
"""Drives `supabase login` (browser flow) against a served stack, with a pty.

  cli-login.py <profile-file> <stack.json>

Plays both halves of the handshake: it runs the real CLI with --no-browser, reads the
login link it prints, authorizes that session the way Studio's /cli/login page does
(POST /platform/cli/login with the dashboard session), types the 8-character
verification code the page would show, and then checks that the saved token works.
"""
import json, os, pty, re, select, subprocess, sys, time, urllib.request

profile, stack_file = sys.argv[1:3]
stack = json.load(open(stack_file))
home = os.path.join(os.path.dirname(os.path.abspath(profile)), "login-home")
os.makedirs(home, exist_ok=True)
env = dict(os.environ, SUPABASE_HOME=home, SUPABASE_NO_KEYRING="1", DO_NOT_TRACK="1", TERM="dumb")
for k in ("SUPABASE_ACCESS_TOKEN", "CLAUDECODE", "CLAUDE_CODE"):
    env.pop(k, None)

master, slave = pty.openpty()
proc = subprocess.Popen(["supabase", f"--profile={profile}", "--agent=no", "--output-format=text", "login", "--no-browser", "--name", "cli_login_test"],
                        stdin=slave, stdout=slave, stderr=slave, env=env, close_fds=True)
buf = ""

def squashed():
    # The prompt library redraws character by character; compare without whitespace
    # or terminal escapes.
    return re.sub(r"\s+", "", re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", buf))

def read_until(pattern, timeout=60):
    global buf
    end = time.time() + timeout
    while time.time() < end:
        if re.search(pattern, squashed()):
            return
        r, _, _ = select.select([master], [], [], 0.5)
        if r:
            try:
                buf += os.read(master, 4096).decode(errors="replace")
            except OSError:
                break
    raise SystemExit(f"timed out waiting for {pattern!r}; output tail:\n{squashed()[-600:]}")

read_until(r"session_id=[0-9a-f-]+&token_name=\S+&public_key=[0-9a-f]+")  # the CLI prints a supabase.com link for custom profiles (CLI 2.119 ignores dashboard_url here)
m = re.search(r"session_id=([0-9a-f-]+)&token_name=(\S+)&public_key=([0-9a-f]+)", buf)
session_id, token_name, public_key = m.groups()
print(f"login link seen: session {session_id}, token name {token_name}")

# Studio's side: the signed-in user authorizes the session.
req = urllib.request.Request(stack["api_url"] + "/platform/cli/login", method="POST",
    data=json.dumps({"session_id": session_id, "public_key": public_key, "token_name": token_name}).encode(),
    headers={"Authorization": "Bearer " + stack["jwt"], "Content-Type": "application/json"})
nonce = json.load(urllib.request.urlopen(req))["nonce"]
code = nonce[:8]
print(f"dashboard shows verification code {code}")

read_until(r"Enteryourverificationcode")
os.write(master, (code + "\r").encode())
read_until(r"loggedin", 60)
proc.wait(timeout=30)
print("CLI output tail:", squashed()[-120:])

# The token the CLI saved must work against the API.
token = open(os.path.join(home, "access-token")).read().strip()
req = urllib.request.Request(stack["api_url"] + "/v1/projects", headers={"Authorization": "Bearer " + token})
projects = json.load(urllib.request.urlopen(req))
assert any(p["ref"] == stack["ref"] for p in projects), projects
print("saved token lists the project: login flow OK")
