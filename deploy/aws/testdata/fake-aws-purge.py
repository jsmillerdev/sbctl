#!/usr/bin/env python3
"""A stand-in `aws` for purge_test.go: the calls deploy.sh makes to delete a stack and to purge what
it leaves (buckets with versioned objects, snapshots), answered from a JSON state file that every
call may change. It answers the exact --query strings the script sends, as the real CLI would print
them, and logs each call to $FAKE_AWS_LOG, so a changed query fails the test that pins it.

State (see purge_test.go): account, stack {name, id, status, exists, outputs, params, volume,
final_snapshot, final_tags, records}, deleted_stacks [{name, id, volume}], buckets {name: {tags,
versions [[key, id]], markers [[key, id]], sizes, uploads [[key, id]], keep [keys S3 refuses to
delete]}}, snapshots [{id, vol, size, start, state, tags, fail}], stop_fails.
"""
import fnmatch
import json
import os
import sys

STATE = os.environ["FAKE_AWS_STATE"]
LOG = os.environ["FAKE_AWS_LOG"]
argv = sys.argv[1:]
with open(LOG, "a") as f:
    f.write(" ".join(argv) + "\n")


def load():
    with open(STATE) as f:
        return json.load(f)


def save(st):
    with open(STATE, "w") as f:
        json.dump(st, f)


def fail(msg, code=254):
    sys.stderr.write(msg + "\n")
    sys.exit(code)


rest = []
i = 0
while i < len(argv):
    if argv[i] in ("--region", "--profile"):
        i += 2
        continue
    rest.append(argv[i])
    i += 1
svc, cmd = rest[0], rest[1]
opts = {}
j = 2
while j < len(rest):
    a = rest[j]
    if a.startswith("--"):
        if "=" in a:
            k, v = a.split("=", 1)
            opts[k] = v
            j += 1
        elif j + 1 < len(rest) and not rest[j + 1].startswith("--"):
            opts[a] = rest[j + 1]
            j += 2
        else:
            opts[a] = True
            j += 1
    else:
        j += 1
q = opts.get("--query", "")
st = load()
stack = st["stack"]
st["snapshots"] = st.get("snapshots") or []
st["buckets"] = st.get("buckets") or {}
for _b in st["buckets"].values():
    _b["versions"] = _b.get("versions") or []
    _b["markers"] = _b.get("markers") or []


def out_json(v):
    print(json.dumps(v, indent=4))


def check_owner():
    owner = opts.get("--expected-bucket-owner")
    if owner is not None and owner != st["account"]:
        fail("An error occurred (AccessDenied) when calling the operation: Access Denied")


def bucket():
    name = opts["--bucket"]
    b = st["buckets"].get(name)
    if b is None:
        fail("An error occurred (NoSuchBucket) when calling the operation: The specified bucket does not exist")
    return name, b


def stack_here():
    name = opts.get("--stack-name")
    return name == stack["name"] or name == stack.get("id")


if svc == "sts" and cmd == "get-caller-identity":
    print(st["account"])
elif svc == "cloudformation" and cmd == "describe-stacks":
    if not (stack_here() and stack["exists"]):
        fail("An error occurred (ValidationError) when calling the DescribeStacks operation: Stack with id %s does not exist" % opts.get("--stack-name"))
    if "StackStatus" in q:
        print(stack["status"])
    elif "StackId" in q:
        print(stack["id"])
    elif "Parameters" in q:
        for k, v in stack.get("params", {}).items():
            print("%s\t%s" % (k, v))
    elif "Outputs" in q:
        if not stack.get("outputs"):
            print("None")
        for k, v in stack["outputs"].items():
            print("%s\t%s" % (k, v))
    else:
        fail("fake aws: describe-stacks with an unknown query: " + q, 3)
elif svc == "cloudformation" and cmd == "list-stacks":
    if not q.startswith("StackSummaries[?StackName=='") or not q.endswith("'].StackId"):
        fail("fake aws: list-stacks with an unknown query: " + q, 3)
    name = q.split("'")[1]
    print("\t".join(d["id"] for d in st.get("deleted_stacks", []) if d["name"] == name))
elif svc == "cloudformation" and cmd == "describe-stack-resource":
    for d in st.get("deleted_stacks", []):
        if d["id"] == opts.get("--stack-name") and d.get("volume"):
            print(d["volume"])
            break
    else:
        fail("An error occurred (ValidationError) when calling the DescribeStackResource operation: Stack does not exist")
elif svc == "cloudformation" and cmd == "delete-stack":
    if not (stack_here() and stack["exists"]):
        fail("no such stack")
    stack["exists"] = False
    st.setdefault("deleted_stacks", []).append({"name": stack["name"], "id": stack["id"], "volume": stack.get("volume") if stack.get("records") else ""})
    if stack.get("final_snapshot"):
        st["snapshots"].append({"id": stack["final_snapshot"], "vol": stack["volume"], "size": 100, "start": "2026-10-08T12:00:00+00:00", "state": "completed", "tags": stack.get("final_tags", {})})
    save(st)
elif svc == "cloudformation" and cmd == "wait":
    pass
elif svc == "ec2" and cmd == "stop-instances":
    if st.get("stop_fails"):
        fail("An error occurred (IncorrectInstanceState) when calling the StopInstances operation")
elif svc == "ec2" and cmd == "wait":
    pass
elif svc == "s3api" and cmd == "list-buckets":
    prefix = q.split("'")[1]
    print("\t".join(n for n in st["buckets"] if n.startswith(prefix)))
elif svc == "s3api" and cmd == "get-bucket-tagging":
    check_owner()
    name, b = bucket()
    if not b.get("tags"):
        fail("An error occurred (NoSuchTagSet) when calling the GetBucketTagging operation: The TagSet does not exist")
    for k, v in b["tags"].items():
        print("%s\t%s" % (k, v))
elif svc == "s3api" and cmd == "list-object-versions":
    check_owner()
    name, b = bucket()
    if q.startswith("[length(Versions"):
        # a large bucket comes back in pages, one line each
        v, m = b["versions"], b["markers"]
        sizes = b.get("sizes", 0)
        print("%d\t%d\t%d" % (len(v), len(m), sizes))
        if b.get("pages"):
            print("0\t0\t0")
    elif q.startswith("{Objects:"):
        if "--no-paginate" not in opts:
            fail("fake aws: a batch is listed with --no-paginate", 3)
        page = ([("v", e) for e in b["versions"]] + [("m", e) for e in b["markers"]])[:1000]
        kind = "v" if "Objects: Versions[]" in q else "m"
        picked = [{"Key": e[0], "VersionId": e[1]} for t, e in page if t == kind]
        out_json({"Objects": picked or None, "Quiet": True})
    else:
        fail("fake aws: list-object-versions with an unknown query: " + q, 3)
elif svc == "s3api" and cmd == "delete-objects":
    check_owner()
    name, b = bucket()
    path = opts["--delete"]
    if not path.startswith("file://"):
        fail("fake aws: --delete takes file://", 3)
    with open(path[len("file://"):]) as f:
        doc = json.load(f)
    objs = doc.get("Objects") or []
    if not objs or len(objs) > 1000:
        fail("An error occurred (MalformedXML) when calling the DeleteObjects operation: %d objects" % len(objs))
    errors = []
    for o in objs:
        pair = [o["Key"], o["VersionId"]]
        if o["Key"] in b.get("keep", []):
            errors.append({"Key": o["Key"], "VersionId": o["VersionId"], "Code": "AccessDenied", "Message": "Access Denied because object protected by object lock."})
            continue
        for lst in ("versions", "markers"):
            if pair in b[lst]:
                b[lst].remove(pair)
    save(st)
    if errors:
        out_json({"Errors": errors})
elif svc == "s3api" and cmd == "list-multipart-uploads":
    check_owner()
    name, b = bucket()
    ups = b.get("uploads", [])
    if ups:
        print("%s\t%s" % tuple(ups[0]))
    else:
        print("None")
elif svc == "s3api" and cmd == "abort-multipart-upload":
    check_owner()
    name, b = bucket()
    pair = [opts["--key"], opts["--upload-id"]]
    if pair not in b.get("uploads", []):
        fail("An error occurred (NoSuchUpload) when calling the AbortMultipartUpload operation")
    b["uploads"].remove(pair)
    save(st)
elif svc == "s3api" and cmd == "delete-bucket":
    check_owner()
    name, b = bucket()
    if b["versions"] or b["markers"]:
        fail("An error occurred (BucketNotEmpty) when calling the DeleteBucket operation: The bucket you tried to delete is not empty. You must delete all versions in the bucket.")
    del st["buckets"][name]
    save(st)
elif svc == "ec2" and cmd == "describe-snapshots":
    flt = opts["--filters"]
    name, _, values = flt[len("Name="):].partition(",Values=")
    vals = values.split(",")
    for s in sorted(st["snapshots"], key=lambda s: s["id"]):
        if name == "volume-id":
            ok = s["vol"] in vals
        elif name.startswith("tag:"):
            ok = fnmatch.fnmatchcase(s.get("tags", {}).get(name[4:], ""), values)
        else:
            fail("fake aws: describe-snapshots with an unknown filter: " + flt, 3)
        if ok:
            print("%s\t%s\t%s\t%s\t%s" % (s["id"], s["vol"], s["size"], s["start"], s["state"]))
elif svc == "ec2" and cmd == "delete-snapshot":
    sid = opts["--snapshot-id"]
    for s in st["snapshots"]:
        if s["id"] == sid:
            if s.get("fail"):
                fail("An error occurred (InvalidSnapshot.InUse) when calling the DeleteSnapshot operation: The snapshot %s is currently in use by ami-0123456789abcdef0" % sid)
            st["snapshots"].remove(s)
            save(st)
            break
    else:
        fail("An error occurred (InvalidSnapshot.NotFound) when calling the DeleteSnapshot operation")
else:
    fail("fake aws: unexpected call: " + " ".join(argv), 3)
