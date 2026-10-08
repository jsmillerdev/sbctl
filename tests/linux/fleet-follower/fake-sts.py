#!/usr/bin/env python3
"""A stand-in for AWS STS on loopback, for tests/linux/fleet-follower.sh.

POST / with Action=AssumeRole answers with the credentials given on the command line (the access key
of the CI object store), valid for one hour, and appends one line per call to --log:

    <time> AssumeRole role=<RoleArn> session=<name> seconds=<DurationSeconds> signed_by=<access key id>

signed_by is the key the request was signed with, read from the Authorization header, which shows
that the daemon called STS with the credentials it was given and not anonymously. The signature is
not checked: awsapi's own tests do that.
"""
import argparse
import datetime
import http.server
import re
import urllib.parse

ap = argparse.ArgumentParser()
ap.add_argument("--port", type=int, required=True)
ap.add_argument("--key-id", required=True)
ap.add_argument("--secret", required=True)
ap.add_argument("--log", required=True)
args = ap.parse_args()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        form = urllib.parse.parse_qs(self.rfile.read(int(self.headers.get("Content-Length", "0"))).decode())
        action = form.get("Action", [""])[0]
        m = re.search(r"Credential=([^/]+)/", self.headers.get("Authorization", ""))
        now = datetime.datetime.now(datetime.timezone.utc)
        with open(args.log, "a") as f:
            f.write("%s %s role=%s session=%s seconds=%s signed_by=%s\n" % (
                now.strftime("%H:%M:%S"), action, form.get("RoleArn", [""])[0], form.get("RoleSessionName", [""])[0],
                form.get("DurationSeconds", [""])[0], m.group(1) if m else "none"))
        if action != "AssumeRole":
            self.send_response(400)
            self.end_headers()
            return
        exp = (now + datetime.timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        body = (
            '<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials>'
            "<AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>ci-session-token</SessionToken>"
            "<Expiration>%s</Expiration></Credentials><AssumedRoleUser><AssumedRoleId>AROACI:%s</AssumedRoleId>"
            "<Arn>arn:aws:sts::123456789012:assumed-role/supavise-storage/%s</Arn></AssumedRoleUser></AssumeRoleResult>"
            "<ResponseMetadata><RequestId>ci</RequestId></ResponseMetadata></AssumeRoleResponse>"
        ) % (args.key_id, args.secret, exp, form.get("RoleSessionName", [""])[0], form.get("RoleSessionName", [""])[0])
        data = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/xml")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
