#!/usr/bin/env python3
"""A small S3 client for tests/linux/storage-migrate.sh: Signature Version 4, path-style, standard
library only, so that the test does not depend on the aws CLI or on a curl that signs.

  S3_ENDPOINT=http://127.0.0.1:9000 S3_BUCKET=b S3_KEY=... S3_SECRET=... [S3_REGION=us-east-1] \
    storage-migrate-s3.py list PREFIX     # one key per line
    storage-migrate-s3.py head KEY        # content-length, content-type, cache-control and last-modified
    storage-migrate-s3.py get KEY         # the object on stdout
    storage-migrate-s3.py delete KEY

Keys are UTF-8; PREFIX and KEY are taken from the command line as they are.
"""
import datetime
import hashlib
import hmac
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

NS = {"s": "http://s3.amazonaws.com/doc/2006-03-01/"}


def _hmac(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()


def request(method, key="", query=None, body=b""):
    endpoint, bucket = os.environ["S3_ENDPOINT"], os.environ["S3_BUCKET"]
    access, secret = os.environ["S3_KEY"], os.environ["S3_SECRET"]
    region = os.environ.get("S3_REGION", "us-east-1")
    host = urllib.parse.urlsplit(endpoint).netloc
    path = urllib.parse.quote("/" + bucket + ("/" + key if key else ""), safe="/-_.~")
    query = sorted((query or {}).items())
    qs = "&".join(urllib.parse.quote(k, safe="-_.~") + "=" + urllib.parse.quote(v, safe="-_.~") for k, v in query)
    now = datetime.datetime.now(datetime.timezone.utc)
    amz_date, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    payload = hashlib.sha256(body).hexdigest()
    headers = {"host": host, "x-amz-content-sha256": payload, "x-amz-date": amz_date}
    signed = ";".join(sorted(headers))
    canonical = "\n".join([method, path, qs, "".join(f"{h}:{headers[h]}\n" for h in sorted(headers)), signed, payload])
    scope = f"{day}/{region}/s3/aws4_request"
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    k = _hmac(_hmac(_hmac(_hmac(("AWS4" + secret).encode(), day), region), "s3"), "aws4_request")
    signature = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = f"AWS4-HMAC-SHA256 Credential={access}/{scope}, SignedHeaders={signed}, Signature={signature}"
    req = urllib.request.Request(endpoint + path + ("?" + qs if qs else ""), data=body or None, method=method,
                                 headers={k: v for k, v in headers.items() if k != "host"})
    try:
        return urllib.request.urlopen(req, timeout=120)
    except urllib.error.HTTPError as e:
        sys.exit(f"{method} {key or '/'}: HTTP {e.code} {e.read()[:300].decode(errors='replace')}")


def main():
    cmd, arg = sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else ""
    if cmd == "list":
        token = None
        while True:
            q = {"list-type": "2", "prefix": arg}
            if token:
                q["continuation-token"] = token
            root = ET.fromstring(request("GET", query=q).read())
            for c in root.findall("s:Contents", NS):
                sys.stdout.buffer.write(c.find("s:Key", NS).text.encode() + b"\n")
            if root.findtext("s:IsTruncated", namespaces=NS) != "true":
                return
            token = root.findtext("s:NextContinuationToken", namespaces=NS)
    elif cmd == "head":
        r = request("HEAD", arg)
        for h in ("content-length", "content-type", "cache-control", "last-modified"):
            print(f"{h}: {r.headers.get(h, '')}")
    elif cmd == "get":
        sys.stdout.buffer.write(request("GET", arg).read())
    elif cmd == "delete":
        request("DELETE", arg).read()
    else:
        sys.exit(f"unknown command {cmd}")


if __name__ == "__main__":
    main()
