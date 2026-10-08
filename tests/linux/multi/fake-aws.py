#!/usr/bin/env python3
"""Fake instance metadata (IMDSv2), EC2 Query API and Secrets Manager for the multi-node harness.

    fake-aws.py --bind 10.213.213.1 --port 38170 --log calls.jsonl \
        --node n1=10.213.213.11=i-0aaaaaaaaaaaaaaa1 --node n2=10.213.213.12=i-0aaaaaaaaaaaaaaa2 \
        [--secret NAME=VALUE ...] [--cluster NAME]

One listener answers all three services, the way the AWS_ENDPOINT_URL_* overrides point them at one
address. A node is known by the address it connects from, so the metadata a node reads is its own.

  IMDSv2        PUT /latest/api/token, then GET /latest/meta-data/... with the token header (a request
                without a token gets 401, like an instance with HttpTokens=required)
  EC2           POST / with Action=DescribeInstances, DescribeInstanceStatus, StopInstances,
                StartInstances, DescribeAddresses or AssociateAddress. DryRun=true answers 412
                DryRunOperation, which is how EC2 says "allowed". StopInstances and StartInstances
                change the state the next describe call reports.
  Secrets       POST / with X-Amz-Target: secretsmanager.GetSecretValue
  GET /_calls   every call so far, one JSON object per line; the same lines go to --log

The responses carry the fields Supavise reads and nothing else; they are not signed and not checked
against a signature.
"""
import argparse
import json
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROLE = "supavise-ci-role"
NS = "http://ec2.amazonaws.com/doc/2016-11-15/"


def xml(body, status=200):
    return status, "text/xml", ('<?xml version="1.0" encoding="UTF-8"?>' + body).encode()


def ec2_error(code, message, status=400):
    return xml('<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors>'
               '<RequestID>fake</RequestID></Response>' % (code, message), status)


class State:
    def __init__(self, args):
        self.cluster = args.cluster
        self.nodes = {}  # source address -> {name, ip, id, state}
        for spec in args.node:
            name, ip, iid = spec.split("=")
            self.nodes[ip] = {"name": name, "ip": ip, "id": iid, "state": "running"}
        self.secrets = dict(s.split("=", 1) for s in args.secret)
        self.tokens = set()
        self.lock = threading.Lock()
        self.log = open(args.log, "a", buffering=1) if args.log else None
        self.calls = []

    def record(self, src, service, action, params=None):
        node = self.nodes.get(src, {}).get("name", "")
        line = json.dumps({"ts": round(time.time(), 3), "src": src, "node": node, "service": service,
                           "action": action, "params": params or {}}, sort_keys=True)
        with self.lock:
            self.calls.append(line)
            if self.log:
                self.log.write(line + "\n")

    def tags(self, node):
        return {"Name": node["name"], "supavise:cluster": self.cluster}


def instance_xml(state, node):
    tags = "".join("<item><key>%s</key><value>%s</value></item>" % kv for kv in state.tags(node).items())
    return ("<item><instanceId>%s</instanceId><instanceState><code>%d</code><name>%s</name></instanceState>"
            "<privateIpAddress>%s</privateIpAddress><ipAddress>%s</ipAddress><tagSet>%s</tagSet></item>"
            % (node["id"], 16 if node["state"] == "running" else 80, node["state"], node["ip"], node["ip"], tags))


def ids_in(params):
    return [v[0] for k, v in sorted(params.items()) if k.startswith("InstanceId.")]


def ec2(state, params):
    action = params.get("Action", [""])[0]
    if action not in ("DescribeInstances", "DescribeInstanceStatus", "StopInstances", "StartInstances",
                      "DescribeAddresses", "AssociateAddress"):
        return ec2_error("InvalidAction", "The action %s is not valid for this web service." % action)
    if params.get("DryRun", ["false"])[0] == "true":
        return ec2_error("DryRunOperation", "Request would have succeeded, but DryRun flag is set.", 412)
    wanted = ids_in(params)
    nodes = [n for n in state.nodes.values() if not wanted or n["id"] in wanted]
    if action == "DescribeInstances":
        return xml('<DescribeInstancesResponse xmlns="%s"><reservationSet><item><instancesSet>%s</instancesSet>'
                   '</item></reservationSet></DescribeInstancesResponse>' % (NS, "".join(instance_xml(state, n) for n in nodes)))
    if action == "DescribeInstanceStatus":
        items = "".join("<item><instanceId>%s</instanceId><instanceState><name>%s</name></instanceState>"
                        "<systemStatus><status>ok</status></systemStatus><instanceStatus><status>ok</status>"
                        "</instanceStatus></item>" % (n["id"], n["state"]) for n in nodes)
        return xml('<DescribeInstanceStatusResponse xmlns="%s"><instanceStatusSet>%s</instanceStatusSet>'
                   '</DescribeInstanceStatusResponse>' % (NS, items))
    if action in ("StopInstances", "StartInstances"):
        stop = action == "StopInstances"
        items = ""
        for n in nodes:
            prev, n["state"] = n["state"], "stopped" if stop else "running"
            items += ("<item><instanceId>%s</instanceId><currentState><name>%s</name></currentState>"
                      "<previousState><name>%s</name></previousState></item>" % (n["id"], "stopping" if stop else "pending", prev))
        return xml('<%sResponse xmlns="%s"><instancesSet>%s</instancesSet></%sResponse>' % (action, NS, items, action))
    if action == "DescribeAddresses":
        items = "".join("<item><publicIp>%s</publicIp><allocationId>eipalloc-%s</allocationId><instanceId>%s</instanceId>"
                        "</item>" % (n["ip"], n["name"], n["id"]) for n in nodes)
        return xml('<DescribeAddressesResponse xmlns="%s"><addressesSet>%s</addressesSet></DescribeAddressesResponse>' % (NS, items))
    return xml('<AssociateAddressResponse xmlns="%s"><return>true</return><associationId>eipassoc-fake</associationId>'
               '</AssociateAddressResponse>' % NS)


def imds(state, node, path):
    meta = path[len("/latest/meta-data/"):] if path.startswith("/latest/meta-data/") else None
    tags = state.tags(node)
    if path == "/latest/dynamic/instance-identity/document":
        return 200, "application/json", json.dumps({"instanceId": node["id"], "region": "us-east-1",
                                                     "availabilityZone": "us-east-1a", "privateIp": node["ip"]}).encode()
    values = {"instance-id": node["id"], "public-ipv4": node["ip"], "local-ipv4": node["ip"],
              "placement/region": "us-east-1", "placement/availability-zone": "us-east-1a",
              "iam/security-credentials/": ROLE, "tags/instance": "\n".join(tags)}
    if meta in values:
        return 200, "text/plain", values[meta].encode()
    if meta and meta.startswith("tags/instance/") and meta[len("tags/instance/"):] in tags:
        return 200, "text/plain", tags[meta[len("tags/instance/"):]].encode()
    if meta == "iam/security-credentials/" + ROLE:
        return 200, "application/json", json.dumps({
            "Code": "Success", "Type": "AWS-HMAC", "AccessKeyId": "ASIAFAKEFAKEFAKEFAKE",
            "SecretAccessKey": "fake-secret", "Token": "fake-session-token",
            "Expiration": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 3600))}).encode()
    return 404, "text/plain", b"not found"


def make_handler(state):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def reply(self, result):
            status, ctype, body = result
            self.send_response(status)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_PUT(self):
            src = self.client_address[0]
            if self.path != "/latest/api/token" or not self.headers.get("X-aws-ec2-metadata-token-ttl-seconds"):
                return self.reply((400, "text/plain", b"bad request"))
            tok = "fake-imds-token-%d" % len(state.tokens)
            state.tokens.add(tok)
            state.record(src, "imds", "token")
            self.reply((200, "text/plain", tok.encode()))

        def do_GET(self):
            src = self.client_address[0]
            if self.path == "/_calls":
                with state.lock:
                    return self.reply((200, "application/x-ndjson", ("\n".join(state.calls) + "\n").encode()))
            if self.headers.get("X-aws-ec2-metadata-token") not in state.tokens:
                return self.reply((401, "text/plain", b"unauthorized"))
            node = state.nodes.get(src)
            if node is None:
                return self.reply((404, "text/plain", b"unknown instance"))
            state.record(src, "imds", "GET " + self.path)
            self.reply(imds(state, node, self.path))

        def do_POST(self):
            src = self.client_address[0]
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
            target = self.headers.get("X-Amz-Target", "")
            if target.startswith("secretsmanager."):
                req = json.loads(body or "{}")
                state.record(src, "secretsmanager", target.split(".", 1)[1], {"SecretId": req.get("SecretId")})
                if target.endswith(".GetSecretValue") and req.get("SecretId") in state.secrets:
                    out = {"ARN": "arn:aws:secretsmanager:us-east-1:000000000000:secret:" + req["SecretId"],
                           "Name": req["SecretId"], "VersionId": "fake", "SecretString": state.secrets[req["SecretId"]]}
                    return self.reply((200, "application/x-amz-json-1.1", json.dumps(out).encode()))
                return self.reply((400, "application/x-amz-json-1.1", json.dumps(
                    {"__type": "ResourceNotFoundException", "Message": "Secrets Manager can't find the specified secret."}).encode()))
            params = urllib.parse.parse_qs(body)
            state.record(src, "ec2", params.get("Action", [""])[0], {k: v[0] for k, v in params.items() if k != "Action"})
            self.reply(ec2(state, params))

        def log_message(self, fmt, *args):
            pass

    return Handler


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--bind", required=True)
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--log", default="")
    p.add_argument("--cluster", default="ci")
    p.add_argument("--node", action="append", default=[], help="NAME=ADDRESS=INSTANCE_ID")
    p.add_argument("--secret", action="append", default=[], help="NAME=VALUE")
    args = p.parse_args()
    ThreadingHTTPServer((args.bind, args.port), make_handler(State(args))).serve_forever()


if __name__ == "__main__":
    main()
