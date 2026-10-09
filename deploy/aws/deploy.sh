#!/usr/bin/env bash
# One-command deploy of a Supavise node to AWS with the CloudFormation template, and the guarded
# update of a stack that exists.
#
#   deploy.sh --region us-east-1 --email you@example.com
#   deploy.sh --region us-east-1 --email you@example.com --domain example.com --hosted-zone-id Z0123456789ABCDEFGHIJ
#   deploy.sh --region us-east-1 --stack-name supavise --delete
#   deploy.sh --region us-east-1 --stack-name supavise --delete --purge
#   deploy.sh purge   --region us-east-1 --stack-name supavise
#   deploy.sh update  --stack supavise [--set Failover=on] [--set PeerCidr1=203.0.113.4/32]
#   deploy.sh status  --stack supavise
#   deploy.sh replica --leader-stack supavise --region eu-west-1 --az eu-west-1b --token-file token.txt
#
# Needs the AWS CLI (v2) with credentials that may create CloudFormation, EC2, IAM, S3, Route 53
# and Secrets Manager resources. --dry-run prints every aws command it would run and runs none.
# Runs on bash 3.2 (macOS) and later. update, status and replica also need python3 (standard library).
#
# Where the template comes from: --template FILE, else (create only) supavise.yaml next to this
# script (a release download), else ../cloudformation/supavise.yaml (a checkout), else the release
# asset of the version. update, status and replica download the template of the release this script
# belongs to and verify it against the release's signed SHA256SUMS with the key stamped into this
# script; with --template FILE the file is used as it is.
set -euo pipefail

NAME=supavise
REPO=supavise/supavise
SELF=$(basename "$0")
# deploy/release-assets.sh puts the release's public key and tag in these two lines of the copy it
# attaches to a release. A copy that was not stamped has no key: update then needs --template.
RELEASE_PUBKEY_B64=${SUPAVISE_DEPLOY_PUBKEY_B64:-__SUPAVISE_RELEASE_PUBKEY_B64__}
RELEASE_TAG=__SUPAVISE_RELEASE_TAG__
case $RELEASE_TAG in v[0-9]*) ;; *) RELEASE_TAG=latest ;; esac
# Test hooks (environment): SUPAVISE_DEPLOY_BASE_URL replaces https://github.com/<repo>/releases,
# SUPAVISE_DEPLOY_PUBKEY_B64 the stamped key, SUPAVISE_IMDS_ENDPOINT the metadata service address.
BASE_URL=${SUPAVISE_DEPLOY_BASE_URL:-https://github.com/$REPO/releases}
IMDS_BASE=${SUPAVISE_IMDS_ENDPOINT:-http://169.254.169.254}
# The API takes a template inline up to this many bytes; a larger one goes through S3.
INLINE_LIMIT=51200

usage() {
  cat <<USAGE
Usage: $SELF --region REGION --email ADDRESS [options]
       $SELF --region REGION --stack-name NAME --delete [--purge] [--yes]
       $SELF purge   --region REGION --stack-name NAME [--yes]
       $SELF update  --stack NAME [options]
       $SELF status  --stack NAME [options]
       $SELF replica --leader-stack NAME --region REGION --az ZONE --token-file FILE [options]

Required to create or update:
  --region REGION          AWS region, for example us-east-1 (or set AWS_REGION)
  --email ADDRESS          Let's Encrypt contact address

Options:
  --domain DOMAIN          base domain, for example example.com (default: <ip>.sslip.io names)
  --hosted-zone-id ID      Route 53 hosted zone of the domain: the stack makes the DNS records
                           and a wildcard certificate (needs --domain)
  --instance-type TYPE     default t4g.large (Graviton, 8 GiB, about 20 projects); see the README
  --stack-name NAME        default $NAME
  --volume-size GIB        data volume size, default 100 (can grow, never shrink)
  --daily-snapshots COUNT  daily snapshots of the data volume to keep, default 7; 0 takes none
  --version TAG            $NAME release to install, default latest (for example v1.2.3)
  --access-cidr CIDR       who may reach ports 80, 443, 5432 and 6543, default 0.0.0.0/0
  --ssh-cidr CIDR          open SSH to this range (needs --key-name); default: no SSH,
                           use Session Manager
  --key-name NAME          EC2 key pair for SSH as the user ubuntu
  --no-session-manager     give the instance role no Systems Manager rights
  --ami-id AMI             Ubuntu 24.04 image to use (default: the current Canonical image for
                           a new stack, the running image for an existing stack)
  --data-snapshot-id SNAP  restore the data volume from the final snapshot of an earlier stack
  --template FILE          use this template file
  --template-bucket NAME   S3 bucket of your account, in the stack's region, that holds a template
                           over $INLINE_LIMIT bytes while CloudFormation reads it (default: the
                           stack's backup bucket for update; for a new stack
                           supavise-templates-<account>-<region>, which the script creates)
  --profile NAME           AWS CLI profile
  --dry-run                print the aws commands, run nothing
  --delete                 stop the instance, then delete the stack (the backup and objects
                           buckets, a final snapshot of the data volume and any daily snapshots
                           stay in your account); asks for confirmation
  --purge                  with --delete: also destroy what the stack leaves in your account, the
                           backup and objects buckets (every object version) and the snapshots of
                           the data volume. One confirmation lists all of it; nothing is left to
                           clean by hand, and none of it can be brought back
  --yes                    with --delete, purge, update or replica: do not ask
  -h, --help

update: brings the stack of a node forward to the template of this release. It shows a change
set, refuses any that would replace or remove a resource, and never changes SupaviseVersion. A
change that CloudFormation decides only while the change set runs (guarded) runs under a temporary
stack policy that denies any replacement or removal; the stack's own policy is put back after. It
uses your credentials, never the instance role of the node it runs on.
  --stack NAME             the stack (default: the one the node's instance tags name)
  --params-from-stack      keep the stack's parameters (this is what update always does)
  --set NAME=VALUE         set a parameter, for example Failover=on or PeerCidr1=203.0.113.4/32;
                           repeat for more (SupaviseVersion cannot be set)
  --allow-risky            allow changes that replace or remove a resource, after you type the
                           stack name; no guard is set (a change that would move the service
                           address back from a failover is never allowed)

status: shows the stack, its infrastructure revision, whether the service address is on its
instance and the repair rule for replacing an instance.

replica: creates a second server (a stack from the same template that joins the leader) and
opens the leader's security group to it.
  --leader-stack NAME      the leader's stack (needs infrastructure revision 2: run update first)
  --leader-region REGION   the leader's region (default: --region)
  --az ZONE                availability zone of the new server (for example eu-west-1b)
  --token-file FILE        the join token that "supavise node token" printed on the leader
  --vpc-id ID, --subnet-id ID   an existing network for the new server (both, in --region)

purge: destroys what an already deleted stack left in your account, for a stack that was deleted
without --purge. It finds only what carries that stack's own marks (buckets tagged by CloudFormation
with the stack's name and id, snapshots tagged with the stack's id and the other snapshots of the
volumes those came from), lists them, and asks you to type the stack name. It refuses while the
stack exists. Only the AWS CLI is needed.
  --region REGION          the stack's region
  --stack-name NAME        the deleted stack

Exit status of update, status, replica and purge: 0 done or nothing to do, 2 refused (arguments, a
change that is not allowed, a signature that does not verify, a stack that still exists), 3 failed.
USAGE
}

die() { printf '%s: %s\n' "$SELF" "$*" >&2; exit 2; }
FAIL_CODE=1
fail() { printf '%s: %s\n' "$SELF" "$*" >&2; exit "$FAIL_CODE"; }
say() { printf '%s\n' "$*"; }

# Quote an argument for display only when the shell would need it.
q() {
  case $1 in
    '' | *[!A-Za-z0-9_./:=@%+,-]*) printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
    *) printf '%s' "$1" ;;
  esac
}
show() { local a out=""; for a in "$@"; do out="$out $(q "$a")"; done; printf '%s\n' "${out# }"; }

CLEANUP=""
# A join token secret made by replica is removed when the script stops before the stack that needs it exists.
EARLY_SECRET=""
# What replica made and leaves in place when it stops: said once, as the script ends with a failure.
LEFTOVER=""
# The guard of an update (see "the guard" below): on_exit takes it off.
GUARD=0          # 1 while the guard is (or may be) on the stack
GUARD_RUNNING=0  # 1 once the guarded change set may be running
GIVE_UP=0        # 1 when the person interrupts the wait for the end of the update
GUARD_STACK="" GUARD_FILE="" GUARD_HAD=""
GUARD_AWS=()
on_exit() {
  local rc=$?
  # The guard comes off before the work directory, which holds the policy to put back, goes.
  if [[ $GUARD -eq 1 ]]; then guard_off_at_exit; fi
  [[ -z $CLEANUP ]] || rm -rf "$CLEANUP"
  if [[ -n $EARLY_SECRET ]]; then
    "${AWS[@]}" secretsmanager delete-secret --secret-id "$EARLY_SECRET" --force-delete-without-recovery >/dev/null 2>&1 || true
  fi
  if [[ $rc -ne 0 && -n $LEFTOVER ]]; then printf '%s\n' "$LEFTOVER" >&2; fi
}
trap on_exit EXIT
WORK=""
mkwork() { if [[ -z $WORK ]]; then WORK=$(mktemp -d); CLEANUP=$WORK; fi; }

# ---- arguments -----------------------------------------------------------------------------
MODE=create
case ${1:-} in
  update | status | replica | purge | __classify | __params) MODE=$1; shift ;;
esac

REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-}}
EMAIL="" DOMAIN="" ZONE="" ITYPE="" STACK=$NAME VOLSIZE="" SNAPS="" VERSION="" ACCESS="" SSH="" KEY=""
SSM="" AMI="" SNAP="" TEMPLATE="" PROFILE="" DRY=0 DELETE=0 PURGE=0 YES=0
REGION_GIVEN="" STACK_GIVEN="" TBUCKET="" ALLOW_RISKY=0 SETS=() LEADER_STACK="" LEADER_REGION="" AZ="" TOKEN_FILE="" VPC="" SUBNET=""
REST=()

# An option takes its value from "--opt=value" or from the next argument.
need() { # called as: need "$@"  (the option is $1)
  if [[ $has_val -eq 0 ]]; then
    [[ $# -ge 2 && $2 != --* ]] || die "$opt needs a value"
    val=$2
    shift_extra=1
  fi
}

while [[ $# -gt 0 ]]; do
  opt=$1 val="" has_val=0 shift_extra=0
  if [[ $MODE == __classify || $MODE == __params ]]; then REST+=("$1"); shift; continue; fi
  if [[ $opt == --*=* ]]; then
    val=${opt#*=}
    opt=${opt%%=*}
    has_val=1
  fi
  case $opt in
    --region) need "$@"; REGION=$val; REGION_GIVEN=1 ;;
    --email) need "$@"; EMAIL=$val ;;
    --domain) need "$@"; DOMAIN=$val ;;
    --hosted-zone-id) need "$@"; ZONE=$val ;;
    --instance-type) need "$@"; ITYPE=$val ;;
    --stack-name | --stack) need "$@"; STACK=$val; STACK_GIVEN=1 ;;
    --volume-size) need "$@"; VOLSIZE=$val ;;
    --daily-snapshots) need "$@"; SNAPS=$val ;;
    --version) need "$@"; VERSION=$val ;;
    --access-cidr) need "$@"; ACCESS=$val ;;
    --ssh-cidr) need "$@"; SSH=$val ;;
    --key-name) need "$@"; KEY=$val ;;
    --ami-id) need "$@"; AMI=$val ;;
    --data-snapshot-id) need "$@"; SNAP=$val ;;
    --template) need "$@"; TEMPLATE=$val ;;
    --template-bucket) need "$@"; TBUCKET=$val ;;
    --profile) need "$@"; PROFILE=$val ;;
    --set) need "$@"; SETS+=("$val") ;;
    --leader-stack) need "$@"; LEADER_STACK=$val ;;
    --leader-region) need "$@"; LEADER_REGION=$val ;;
    --az) need "$@"; AZ=$val ;;
    --token-file) need "$@"; TOKEN_FILE=$val ;;
    --vpc-id) need "$@"; VPC=$val ;;
    --subnet-id) need "$@"; SUBNET=$val ;;
    --no-session-manager) SSM=false ;;
    --allow-risky) ALLOW_RISKY=1 ;;
    --params-from-stack) ;;
    --dry-run) DRY=1 ;;
    --delete) DELETE=1 ;;
    --purge) PURGE=1 ;;
    --yes | -y) YES=1 ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
  shift
  if [[ $shift_extra -eq 1 ]]; then shift; fi
done

# ---- the Python of update, status and replica ----------------------------------------------
# Standard library only. It reads what `aws ... --output json` printed, so that no jq is needed.
#   facts  STACK.json                     the stack as KEY<TAB>value lines
#   params STACK.json TEMPLATE [K=V...]   the --parameters JSON of an update
#   classify CHANGESET.json [--failed-over]   the review of a change set; exit 0 allowed, 10 refused,
#                                         11 blocked, 12 guarded (allowed only under the guard)
#   guard  OWN.json TEMPLATE CHANGESET.json RESTORE   prints the guard policy; writes the policy to put back
#   failures EVENTS.json STACK             what failed in the last update of the stack
#   same   TEMPLATE STAGED.json           is the template CloudFormation holds the verified file? exit 0 yes, 1 no
IFS= read -r -d '' PYHELPER <<'PY' || true
import io, json, re, sys

# Resource types whose replacement does not interrupt the node: a rule and two policies. A change
# set that replaces any other type is refused, and the guard denies it while a change set runs.
REPLACE_OK = set([
    "AWS::EC2::SecurityGroupIngress", "AWS::IAM::Policy", "AWS::S3::BucketPolicy",
])
# Resource types whose removal is allowed: turning Failover off or clearing a PeerCidr takes a
# permission or a firewall rule away, which interrupts nothing. Any other removal is refused, and
# the guard denies it.
REMOVE_OK = set([
    "AWS::EC2::SecurityGroupIngress", "AWS::IAM::Policy",
])
# Properties that change in place without an interruption, per type. Any other change to a
# resource is refused until a person has read it (--allow-risky).
SAFE = {
    "AWS::EC2::Instance": set(["Tags", "MetadataOptions"]),
    "AWS::EC2::EIP": set(["Tags"]),
    "AWS::IAM::Role": set(["Tags", "Policies"]),
    "AWS::IAM::Policy": set(["PolicyDocument"]),
    "AWS::S3::BucketPolicy": set(["PolicyDocument"]),
    "AWS::EC2::SecurityGroup": set(["SecurityGroupIngress", "Tags"]),
}
IMMUTABLE_PARAMS = ("SupaviseVersion",)
# The change sources of a Ref (ResourceReference) and of an Fn::GetAtt (ResourceAttribute).
REFERENCES = ("ResourceReference", "ResourceAttribute")
AMI = re.compile(r"^ami-[0-9a-f]{8,17}$")
# The statement of a stack policy that allows every update: what a stack without a policy gets back
# after the guard, because a stack policy cannot be deleted.
ALLOW_ALL = {"Effect": "Allow", "Action": "Update:*", "Principal": "*", "Resource": "*"}
# The most SetStackPolicy takes in StackPolicyBody.
POLICY_MAX = 16384


def die(msg, code=1):
    sys.stderr.write("deploy.sh: %s\n" % msg)
    sys.exit(code)


def load(path):
    with open(path) as f:
        return json.load(f)


def cmd_facts(stack_json):
    stack = load(stack_json)["Stacks"][0]
    print("status\t%s" % stack["StackStatus"])
    print("id\t%s" % stack["StackId"])
    for p in stack.get("Parameters", []):
        print("param:%s\t%s" % (p["ParameterKey"], p.get("ParameterValue", "")))
    for o in stack.get("Outputs", []):
        print("output:%s\t%s" % (o["OutputKey"], o["OutputValue"]))


def template_params(path):
    """The names under the top-level Parameters: key of the template."""
    names, inside = [], False
    with open(path) as f:
        for line in f:
            if re.match(r"^Parameters:\s*$", line):
                inside = True
                continue
            if inside:
                if re.match(r"^[A-Za-z]", line):
                    break
                m = re.match(r"^  ([A-Za-z0-9]+):\s*$", line)
                if m:
                    names.append(m.group(1))
    return names


def cmd_params(stack_json, template, *sets):
    """UsePreviousValue for every parameter the stack has and the template still declares;
    a given value for each K=V; nothing for a parameter that is new to the stack (it takes its
    default)."""
    stack = load(stack_json)["Stacks"][0]
    have = [p["ParameterKey"] for p in stack.get("Parameters", [])]
    want = template_params(template)
    if not want:
        die("no parameters found in %s: is it a Supavise template?" % template, 2)
    given = {}
    for s in sets:
        if "=" not in s:
            die("--set %s: write NAME=VALUE" % s, 2)
        k, v = s.split("=", 1)
        if k in IMMUTABLE_PARAMS:
            die("%s cannot be set: it feeds the user data, and a different value would replace the instance" % k, 2)
        if k not in want:
            die("--set %s: the template has no parameter %s" % (s, k), 2)
        given[k] = v
    out = []
    for k in want:
        if k in given:
            out.append({"ParameterKey": k, "ParameterValue": given[k]})
        elif k in have:
            out.append({"ParameterKey": k, "UsePreviousValue": True})
    dropped = [k for k in have if k not in want]
    if dropped:
        sys.stderr.write("note: the template no longer declares %s\n" % ", ".join(dropped))
    json.dump(out, sys.stdout)
    sys.stdout.write("\n")


def cause_of(d):
    """The resource that a Ref or Fn::GetAtt detail follows: Instance of Instance.AvailabilityZone."""
    return (d.get("CausingEntity") or "").split(".", 1)[0]


def source_of(d, plan):
    """Where the new value of one detail of a change comes from:
    static   CloudFormation knows it while it makes the change set (Evaluation Static), or the value
             is a Ref or Fn::GetAtt of a resource that is added, replaced for certain, or not in the
             change set at all: it changes, or nothing explains why it would not;
    cascade  a Ref or Fn::GetAtt (Evaluation Dynamic) of a resource that the change set modifies
             without replacing it for certain, and whose replacement the guard denies: the value
             changes only if that resource is replaced, which the guard refuses;
    dynamic  anything else that CloudFormation resolves only while the change set runs, such as the
             {{resolve:ssm:...}} image of a stack made in the console, or a Ref of a resource whose
             replacement the guard allows."""
    if d.get("Evaluation") != "Dynamic":
        return "static"
    if d.get("ChangeSource") in REFERENCES:
        cause = plan.get(cause_of(d))
        if not cause or cause["action"] != "Modify" or cause["replacement"] == "True":
            return "static"
        if cause["replacement"] == "Conditional" and cause["type"] in REPLACE_OK:
            return "dynamic"
        return "cascade"
    return "dynamic"


def image_change(rc):
    """(before, after) of the ImageId of an instance when the change set shows both values: the
    BeforeValue and AfterValue of its detail (describe-change-set --include-property-values), else
    the resource's BeforeContext and AfterContext. None when it shows them not."""
    for d in rc.get("Details") or []:
        t = d.get("Target") or {}
        if t.get("Name") == "ImageId" or t.get("Path") == "/Properties/ImageId":
            if t.get("BeforeValue") is not None and t.get("AfterValue") is not None:
                return t["BeforeValue"], t["AfterValue"]
    values = []
    for key in ("BeforeContext", "AfterContext"):
        try:
            values.append(json.loads(rc.get(key) or "null")["Properties"]["ImageId"])
        except (ValueError, TypeError, KeyError):
            return None
    return values[0], values[1]


def short(v, n=60):
    s = v if isinstance(v, str) else json.dumps(v)
    return s if len(s) <= n else s[:n - 3] + "..."


def judge(rc, failed_over, plan):
    """One resource change: (verdict, words). Verdict is ok, guarded, refused or blocked.

    guarded: CloudFormation decides only while the change set runs whether the resource is
    replaced, and the only ways it can go are no change, a change in place known to be safe, or a
    replacement that the guard denies. Such a change set runs under the guard and nowhere else."""
    typ = rc.get("ResourceType", "?")
    rid = rc.get("LogicalResourceId", "?")
    action = rc.get("Action", "?")
    repl = rc.get("Replacement", "False")
    labels, recreate = [], repl in ("True", "Conditional")
    for d in rc.get("Details") or []:
        t = d.get("Target") or {}
        if t.get("RequiresRecreation", "Never") in ("Always", "Conditionally"):
            recreate = True
        label = t.get("Name", "") or t.get("Attribute", "")
        if label and label not in labels:
            labels.append(label)
    what = "%s (%s)" % (rid, typ)
    detail = ": " + ", ".join(labels) if labels else ""

    if failed_over and typ in ("AWS::EC2::EIPAssociation", "AWS::EC2::EIP"):
        only_tags = action == "Modify" and not recreate and labels and not [l for l in labels if l != "Tags"]
        if not only_tags and action != "Add":
            return "blocked", "%s: the service address is on another server after a failover, and this would move it back" % what
    if failed_over and typ == "AWS::EC2::Instance" and action != "Add":
        # CloudFormation attaches an Elastic IP again after it updates the instance it belongs to,
        # whatever the property was; until a rehearsal shows that a change of tags does not, any
        # update of the instance is held back while the address is on another server.
        return "blocked", "%s: the service address is on another server after a failover, and an update of the instance can move it back" % what
    if action == "Add":
        return "ok", "add      %s" % what
    if action == "Remove":
        if typ in REMOVE_OK:
            return "ok", "remove   %s" % what
        return "refused", "REMOVE   %s would be deleted" % what
    if action != "Modify":
        return "refused", "%-8s %s is a change this script does not know" % (action.upper(), what)
    if recreate and typ in REPLACE_OK:
        return "ok", "replace  %s%s" % (what, detail)
    if not labels:
        return "refused", "MODIFY   %s changes in a way the change set does not describe" % what

    # Each detail on its own. A property can have several (a parameter that changes gives one
    # Dynamic DirectModification and one Static ParameterReference); the worst one counts.
    certain, unsafe, guarded = [], [], []
    for d in rc.get("Details") or []:
        t = d.get("Target") or {}
        attr = t.get("Attribute", "")
        label = t.get("Name", "") or attr or "(a change without a name)"
        if attr == "Metadata":
            continue
        need = t.get("RequiresRecreation", "Never")
        safe = label in SAFE.get(typ, ())
        source = source_of(d, plan)
        if source == "cascade":
            # It follows a resource that the guard keeps from being replaced: it stays as it is.
            if need != "Never" or not safe:
                guarded.append((label, "%s follows %s" % (label, cause_of(d)), need))
        elif source == "dynamic":
            # Resolved while the change set runs. A replacement is the guard's to deny; a change in
            # place is not, so it must be one that is known to be safe.
            if need == "Always" or (need == "Conditionally" and safe):
                guarded.append((label, "%s is resolved when the change set runs" % label, need))
            elif not safe:
                unsafe.append(label)
        elif need != "Never":
            certain.append(label)
        elif not safe:
            unsafe.append(label)

    if repl == "True" or certain:
        word = "would be replaced" if repl == "True" else "may be replaced"
        return "refused", "REPLACE  %s %s%s" % (what, word, detail)
    if unsafe:
        names = []
        for u in unsafe:
            if u not in names:
                names.append(u)
        return "refused", "MODIFY   %s: %s changes in place, which this script does not know to be safe" % (what, ", ".join(names))
    if not guarded:
        if repl == "Conditional":
            # The change set says the resource may be replaced, and no detail says why.
            return "refused", "REPLACE  %s may be replaced%s" % (what, detail)
        return "ok", "modify   %s%s (no interruption)" % (what, detail)

    notes = []
    for g in guarded:
        if g[1] not in notes:
            notes.append(g[1])
    if typ == "AWS::EC2::Instance" and "ImageId" in [g[0] for g in guarded]:
        values = image_change(rc)
        if values is not None:
            before, after = values
            if isinstance(before, str) and isinstance(after, str) and AMI.match(before) and AMI.match(after):
                if before != after:
                    return "refused", "REPLACE  %s would be replaced: ImageId changes from %s to %s, another image" % (what, before, after)
                notes.append("ImageId stays %s" % before)
            else:
                notes.append("ImageId %s -> %s, which cannot be compared before it runs" % (short(before), short(after)))
    may = repl == "Conditional" or [g for g in guarded if g[2] != "Never"]
    return "guarded", "guarded  %s%s%s; %s" % (what, " may be replaced" if may else "", detail, ", ".join(notes))


def cmd_classify(path, *flags):
    failed_over = "--failed-over" in flags
    cs = load(path)
    changes = [c["ResourceChange"] for c in cs.get("Changes", []) if c.get("Type") == "Resource" and "ResourceChange" in c]
    plan = {}
    for rc in changes:
        plan[rc.get("LogicalResourceId", "?")] = {
            "action": rc.get("Action"), "replacement": rc.get("Replacement", "False"), "type": rc.get("ResourceType", "?")}
    verdicts = {"ok": 0, "guarded": 0, "refused": 0, "blocked": 0}
    lines = []
    for rc in changes:
        v, text = judge(rc, failed_over, plan)
        verdicts[v] += 1
        lines.append((v, text))
    order = {"blocked": 0, "refused": 1, "guarded": 2, "ok": 3}
    for v, text in sorted(lines, key=lambda x: order[x[0]]):
        print("  %s" % text)
    if not changes:
        print("  (no resource changes)")
    print("%d change(s): %d allowed, %d refused, %d blocked, %d guarded" % (
        len(changes), verdicts["ok"], verdicts["refused"], verdicts["blocked"], verdicts["guarded"]))
    if verdicts["blocked"]:
        sys.exit(11)
    if verdicts["refused"]:
        sys.exit(10)
    if verdicts["guarded"]:
        sys.exit(12)


def template_types(path):
    """The resource types under the top-level Resources: key of the template."""
    types, inside = set(), False
    with open(path) as f:
        for line in f:
            if re.match(r"^Resources:\s*$", line):
                inside = True
                continue
            if inside:
                if re.match(r"^[A-Za-z]", line):
                    break
                m = re.match(r"""^    Type:\s*["']?([A-Za-z0-9]+(?:::[A-Za-z0-9]+)+)""", line)
                if m:
                    types.add(m.group(1))
    return types


def cmd_guard(own_json, template, cs_json, restore_out):
    """Prints the guard: the stack's own policy (or one that allows every update, when it has none)
    and two Deny statements, one for the replacement of every resource type of the template and the
    change set but REPLACE_OK, one for the removal of every such type but REMOVE_OK. A Deny always
    wins over an Allow, so the guard never allows what the stack's own policy denies. The types are
    named one by one: the stack policy grammar has StringEquals and StringLike for ResourceType, not
    a negation. Writes the policy to put back afterwards to RESTORE_OUT, the stack's own as it was,
    or the one that allows every update (a stack policy cannot be deleted)."""
    with open(own_json) as f:
        text = f.read().strip()
    own = json.loads(text) if text else {}
    body = own.get("StackPolicyBody") if isinstance(own, dict) else None
    if body:
        try:
            statements = json.loads(body)["Statement"]
        except (ValueError, TypeError, KeyError):
            die("the stack's own policy is not a stack policy this script can read", 3)
        if isinstance(statements, dict):
            statements = [statements]
        if not isinstance(statements, list):
            die("the stack's own policy is not a stack policy this script can read", 3)
        restore = body
    else:
        statements = [ALLOW_ALL]
        restore = json.dumps({"Statement": [ALLOW_ALL]})
    types = template_types(template)
    for c in load(cs_json).get("Changes", []):
        t = (c.get("ResourceChange") or {}).get("ResourceType")
        if t:
            types.add(t)
    if not types:
        die("found no resource types to guard in %s" % template, 3)
    deny = []
    for action, allowed in (("Update:Replace", REPLACE_OK), ("Update:Delete", REMOVE_OK)):
        kept = sorted(t for t in types if t not in allowed)
        if kept:
            deny.append({"Effect": "Deny", "Action": action, "Principal": "*", "Resource": "*",
                         "Condition": {"StringEquals": {"ResourceType": kept}}})
    policy = json.dumps({"Statement": list(statements) + deny})
    if len(policy) > POLICY_MAX:
        die("the guard with the stack's own policy is %d bytes, over the %d a stack policy may have" % (len(policy), POLICY_MAX), 3)
    with open(restore_out, "w") as f:
        f.write(restore)
    print(policy)


def cmd_failures(events_json, stack):
    """What failed in the last update of the stack: LogicalId<TAB>reason for each UPDATE_FAILED
    event since the stack's own UPDATE_IN_PROGRESS (describe-stack-events lists the newest first)."""
    for e in load(events_json).get("StackEvents", []):
        if e.get("LogicalResourceId") == stack and e.get("ResourceType") == "AWS::CloudFormation::Stack":
            if e.get("ResourceStatus") == "UPDATE_IN_PROGRESS":
                break
            continue
        if e.get("ResourceStatus") == "UPDATE_FAILED":
            print("%s\t%s" % (e.get("LogicalResourceId", "?"), " ".join((e.get("ResourceStatusReason") or "").split())))


def cmd_same(template, staged):
    """Exit 0 when STAGED (the TemplateBody that get-template printed as JSON: a YAML template comes
    back as one string) is the TEMPLATE file. Line ends and the trailing white space of the whole
    file do not count."""
    def norm(text):
        return text.replace("\r\n", "\n").rstrip()
    with io.open(template, encoding="utf-8", newline="") as f:
        mine = f.read()
    got = load(staged)
    sys.exit(0 if isinstance(got, str) and norm(got) == norm(mine) else 1)


def main(argv):
    cmds = {"facts": cmd_facts, "params": cmd_params, "classify": cmd_classify, "guard": cmd_guard,
            "failures": cmd_failures, "same": cmd_same}
    if len(argv) < 2 or argv[1] not in cmds:
        die("unknown helper command", 2)
    cmds[argv[1]](*argv[2:])


main(sys.argv)
PY
py() { python3 -c "$PYHELPER" "$@"; }

if [[ $MODE == __classify ]]; then py classify ${REST[@]+"${REST[@]}"}; exit $?; fi
if [[ $MODE == __params ]]; then py params ${REST[@]+"${REST[@]}"}; exit $?; fi

# ---- validation ----------------------------------------------------------------------------
re_region='^[a-z]{2}(-[a-z]+)+-[0-9]+$'
re_email='^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'
re_stack='^[A-Za-z][A-Za-z0-9-]{0,127}$'
re_domain='^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'
re_zone='^Z[A-Z0-9]{5,31}$'
re_type='^[a-z0-9]+\.[a-z0-9]+$'
re_version='^(latest|v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?)$'
re_cidr='^([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[12][0-9]|3[0-2])$'
re_ami='^ami-[0-9a-f]{8,17}$'
re_snap='^snap-[0-9a-f]{8,17}$'
re_az='^[a-z]{2}(-[a-z]+)+-[0-9][a-z]$'
re_vpc='^vpc-[0-9a-f]{8,17}$'
re_subnet='^subnet-[0-9a-f]{8,17}$'
re_bucket='^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$'
re_allf='^vol-f+$'

[[ -n $REGION || $MODE == update || $MODE == status ]] || die "--region is required (or set AWS_REGION)"
[[ -z $REGION ]] || [[ $REGION =~ $re_region ]] || die "--region $REGION is not a region name such as us-east-1"
[[ $STACK =~ $re_stack ]] || die "--stack-name must start with a letter and hold only letters, digits and hyphens"
[[ -z $TBUCKET || $TBUCKET =~ $re_bucket ]] || die "--template-bucket $TBUCKET is not an S3 bucket name"

validate_create_options() {
  [[ -n $EMAIL ]] || die "--email is required"
  [[ $EMAIL =~ $re_email ]] || die "--email $EMAIL is not an email address"
  [[ -z $DOMAIN || $DOMAIN =~ $re_domain ]] || die "--domain must be a lower-case domain name such as example.com"
  [[ -z $ZONE || $ZONE =~ $re_zone ]] || die "--hosted-zone-id must look like Z0123456789ABCDEFGHIJ"
  [[ -z $ZONE || -n $DOMAIN ]] || die "--hosted-zone-id needs --domain"
  [[ -z $ITYPE || $ITYPE =~ $re_type ]] || die "--instance-type $ITYPE is not an instance type such as t4g.large"
  [[ -z $VERSION || $VERSION =~ $re_version ]] || die "--version must be latest or a release tag such as v1.2.3"
  [[ -z $ACCESS || $ACCESS =~ $re_cidr ]] || die "--access-cidr must be an IPv4 range such as 203.0.113.0/24"
  [[ -z $SSH || $SSH =~ $re_cidr ]] || die "--ssh-cidr must be an IPv4 range such as 203.0.113.4/32"
  [[ -z $SSH || -n $KEY ]] || die "--ssh-cidr needs --key-name (an EC2 key pair); or leave SSH off and use Session Manager"
  [[ -z $AMI || $AMI =~ $re_ami ]] || die "--ami-id must look like ami-0123456789abcdef0"
  [[ -z $SNAP || $SNAP =~ $re_snap ]] || die "--data-snapshot-id must look like snap-0123456789abcdef0"
  if [[ -n $VOLSIZE ]]; then
    if ! [[ $VOLSIZE =~ ^[0-9]+$ ]] || [[ $VOLSIZE -lt 20 || $VOLSIZE -gt 16384 ]]; then
      die "--volume-size must be a whole number of GiB from 20 to 16384"
    fi
  fi
  if [[ -n $SNAPS ]]; then
    # At most four digits, so the arithmetic below cannot overflow; a leading zero is not octal.
    if ! [[ $SNAPS =~ ^[0-9]{1,4}$ ]] || [[ $((10#$SNAPS)) -gt 1000 ]]; then
      die "--daily-snapshots must be a whole number from 0 to 1000"
    fi
    SNAPS=$((10#$SNAPS))
  fi
}

case $MODE in
  create)
    if [[ $DELETE -eq 1 ]]; then
      [[ -z $EMAIL$DOMAIN$ZONE$ITYPE$VOLSIZE$SNAPS$VERSION$ACCESS$SSH$KEY$SSM$AMI$SNAP$TEMPLATE ]] \
        || die "--delete takes only --region, --stack-name, --profile, --purge, --yes and --dry-run"
    else
      [[ $PURGE -eq 0 ]] || die "--purge belongs to --delete (or use: $SELF purge --region REGION --stack-name NAME for a stack that is already deleted)"
      [[ $YES -eq 0 ]] || die "--yes belongs to --delete"
      validate_create_options
    fi ;;
  update)
    [[ $DELETE -eq 0 ]] || die "update does not take --delete"
    [[ $PURGE -eq 0 ]] || die "update does not take --purge"
    [[ -z $EMAIL$DOMAIN$ZONE$ITYPE$VOLSIZE$SNAPS$ACCESS$SSH$KEY$SSM$AMI$SNAP$LEADER_STACK$LEADER_REGION$AZ$TOKEN_FILE$VPC$SUBNET ]] \
      || die "update changes nothing but what --set names (use --set NAME=VALUE for a parameter)"
    [[ -z $VERSION || $VERSION =~ $re_version ]] || die "--version must be latest or a release tag such as v1.2.3" ;;
  status)
    [[ $DELETE -eq 0 && $PURGE -eq 0 && $ALLOW_RISKY -eq 0 && ${#SETS[@]} -eq 0 ]] || die "status takes only --stack, --region, --profile and --template"
    [[ -z $VERSION || $VERSION =~ $re_version ]] || die "--version must be latest or a release tag such as v1.2.3" ;;
  replica)
    [[ $DELETE -eq 0 ]] || die "replica does not take --delete"
    [[ $PURGE -eq 0 ]] || die "replica does not take --purge"
    [[ ${#SETS[@]} -eq 0 ]] || die "replica takes no --set"
    [[ -n $LEADER_STACK ]] || die "--leader-stack is required"
    [[ $LEADER_STACK =~ $re_stack ]] || die "--leader-stack must start with a letter and hold only letters, digits and hyphens"
    [[ -n $AZ || -n $VPC ]] || die "--az is required (or --vpc-id and --subnet-id)"
    [[ -z $AZ ]] || [[ $AZ =~ $re_az ]] || die "--az $AZ is not a zone name such as eu-west-1b"
    [[ -z $AZ || $AZ == "$REGION"* ]] || die "--az $AZ is not in --region $REGION"
    [[ -n $TOKEN_FILE ]] || die "--token-file is required"
    [[ -z $LEADER_REGION || $LEADER_REGION =~ $re_region ]] || die "--leader-region $LEADER_REGION is not a region name such as us-east-1"
    [[ -z $VPC || $VPC =~ $re_vpc ]] || die "--vpc-id must look like vpc-0123456789abcdef0"
    [[ -z $SUBNET || $SUBNET =~ $re_subnet ]] || die "--subnet-id must look like subnet-0123456789abcdef0"
    [[ -z $VPC && -z $SUBNET ]] || [[ -n $VPC && -n $SUBNET ]] || die "give both --vpc-id and --subnet-id, or neither"
    [[ -z $VPC || -z $AZ ]] || die "--az is for the subnet the stack creates; with --subnet-id the subnet decides"
    [[ -n $STACK_GIVEN ]] || STACK=$LEADER_STACK-replica
    [[ $STACK =~ $re_stack ]] || die "the stack name $STACK is not valid; give --stack-name"
    [[ -n $LEADER_REGION ]] || LEADER_REGION=$REGION
    [[ -z $EMAIL || $EMAIL =~ $re_email ]] || die "--email $EMAIL is not an email address"
    [[ -z $ITYPE || $ITYPE =~ $re_type ]] || die "--instance-type $ITYPE is not an instance type such as t4g.large"
    [[ -z $VERSION || $VERSION =~ $re_version ]] || die "--version must be latest or a release tag such as v1.2.3"
    [[ -z $ACCESS || $ACCESS =~ $re_cidr ]] || die "--access-cidr must be an IPv4 range such as 203.0.113.0/24"
    [[ -z $SSH || $SSH =~ $re_cidr ]] || die "--ssh-cidr must be an IPv4 range such as 203.0.113.4/32"
    [[ -z $SSH || -n $KEY ]] || die "--ssh-cidr needs --key-name (an EC2 key pair); or leave SSH off and use Session Manager"
    [[ -z $AMI || $AMI =~ $re_ami ]] || die "--ami-id must look like ami-0123456789abcdef0"
    [[ -z $VOLSIZE ]] || { [[ $VOLSIZE =~ ^[0-9]+$ ]] && [[ $VOLSIZE -ge 20 && $VOLSIZE -le 16384 ]]; } || die "--volume-size must be a whole number of GiB from 20 to 16384"
    [[ -z $DOMAIN$ZONE$SNAP ]] || die "a replica server takes its domain from the leader: --domain, --hosted-zone-id and --data-snapshot-id are not for it" ;;
  purge)
    [[ $DELETE -eq 0 ]] || die "purge does not take --delete: it is for a stack that is already deleted ($SELF --delete --purge deletes a stack that exists and purges it)"
    [[ -n $STACK_GIVEN ]] || die "purge needs --stack-name: it destroys data, so the stack is never guessed"
    [[ -z $EMAIL$DOMAIN$ZONE$ITYPE$VOLSIZE$SNAPS$VERSION$ACCESS$SSH$KEY$SSM$AMI$SNAP$TEMPLATE$LEADER_STACK$LEADER_REGION$AZ$TOKEN_FILE$VPC$SUBNET && $ALLOW_RISKY -eq 0 && ${#SETS[@]} -eq 0 ]] \
      || die "purge takes only --region, --stack-name, --profile, --yes and --dry-run" ;;
esac

AWS=(aws)
[[ -z $REGION ]] || AWS+=(--region "$REGION")
[[ -z $PROFILE ]] || AWS+=(--profile "$PROFILE")

# Run an aws command, or only print it with --dry-run.
run() {
  if [[ $DRY -eq 1 ]]; then show "$@"; else "$@"; fi
}
note() { if [[ $DRY -eq 1 ]]; then printf '# %s\n' "$*"; fi; }

# One describe-stacks call per list; the values are picked out of its tab-separated text.
Q_OUTPUTS='Stacks[0].Outputs[].[OutputKey,OutputValue]'
Q_PARAMS='Stacks[0].Parameters[].[ParameterKey,ParameterValue]'
describe() { "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$1" --output text; }
pick() { # KEY, from "KEY<TAB>value" lines on stdin
  awk -F'\t' -v k="$1" '$1 == k { print substr($0, index($0, "\t") + 1) }'
}

stack_status() { # prints the stack's status, or "none" when it does not exist
  local err rc=0 status
  err=$(mktemp)
  status=$("${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text 2>"$err") || rc=$?
  if [[ $rc -ne 0 ]]; then
    if grep -q 'does not exist' "$err"; then rm -f "$err"; echo none; return 0; fi
    cat "$err" >&2; rm -f "$err"
    fail "cannot read the stack (credentials, region and permissions?)"
  fi
  rm -f "$err"
  echo "$status"
}

# ---- the template: a file, a download, or a verified release download ----------------------
sha256_of() { # FILE
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}
absolute() { (cd "$(dirname "$1")" && printf '%s/%s' "$(pwd)" "$(basename "$1")"); }

# OpenSSL with ed25519 verification: 1.1.1 or later or any 3.x; macOS's own LibreSSL has none.
find_openssl() {
  local c
  for c in openssl /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl; do
    if command -v "$c" >/dev/null 2>&1 && "$c" genpkey -algorithm ed25519 >/dev/null 2>&1; then command -v "$c"; return 0; fi
  done
  return 1
}
b64decode() { if printf 'eA==' | base64 -d >/dev/null 2>&1; then base64 -d; else base64 -D; fi; }

# Downloads the template of the release this script belongs to (--version names another) with its
# signed checksum list, and checks the signature with the key stamped into this script, the
# template against the list, and this script against the list.
fetch_verified_template() {
  local tag=${VERSION:-$RELEASE_TAG} url ssl want got me
  if [[ $tag == latest ]]; then url=$BASE_URL/latest/download; else url=$BASE_URL/download/$tag; fi
  mkwork
  mkdir -p "$WORK/release"
  if [[ $DRY -eq 1 ]]; then
    note "download the template of the release and verify it against its signed checksum list with the key in this script:"
    show curl -fsSL -o "$WORK/release/SHA256SUMS" "$url/SHA256SUMS"
    show curl -fsSL -o "$WORK/release/SHA256SUMS.sig" "$url/SHA256SUMS.sig"
    show curl -fsSL -o "$WORK/release/supavise.yaml" "$url/supavise.yaml"
    TEMPLATE=$WORK/release/supavise.yaml
    return 0
  fi
  say "Downloading the template of $tag"
  for f in SHA256SUMS SHA256SUMS.sig supavise.yaml; do
    curl -fsSL -o "$WORK/release/$f" "$url/$f" || fail "cannot download $url/$f (pass --template FILE to use a file you have)"
  done
  printf '%s' "$RELEASE_PUBKEY_B64" | b64decode >"$WORK/release/release.pem" 2>/dev/null || true
  if ! ssl=$(find_openssl); then
    fail "no OpenSSL that verifies ed25519 signatures (CloudShell and Ubuntu have one; on macOS: brew install openssl@3). Or pass --template FILE with a template you have verified"
  fi
  "$ssl" pkey -pubin -in "$WORK/release/release.pem" -noout 2>/dev/null \
    || die "this copy of $SELF holds no release key, so it cannot verify a download. Use the script attached to a release, or pass --template FILE"
  "$ssl" pkeyutl -verify -rawin -pubin -inkey "$WORK/release/release.pem" -sigfile "$WORK/release/SHA256SUMS.sig" -in "$WORK/release/SHA256SUMS" >/dev/null 2>&1 \
    || die "the signature of SHA256SUMS does not verify against the release key: refusing to use the template"
  want=$(awk '$2 == "supavise.yaml" || $2 == "*supavise.yaml" { print $1; exit }' "$WORK/release/SHA256SUMS")
  got=$(sha256_of "$WORK/release/supavise.yaml")
  [[ -n $want && $want == "$got" ]] || die "supavise.yaml does not match the signed checksum list: refusing to use it"
  # This script is part of the release too: an edited copy would not be the code that was signed.
  want=$(awk '$2 == "supavise-aws-deploy.sh" || $2 == "*supavise-aws-deploy.sh" { print $1; exit }' "$WORK/release/SHA256SUMS")
  me=$(sha256_of "${BASH_SOURCE[0]}")
  [[ -n $want ]] || die "the signed checksum list of $tag does not name supavise-aws-deploy.sh, so this copy cannot be checked: refusing to go on"
  [[ $want == "$me" ]] || die "this copy of $SELF is not the one release $tag signed: download supavise-aws-deploy.sh from that release again"
  TEMPLATE=$WORK/release/supavise.yaml
  say "Template verified against the signed checksums of $tag"
}

resolve_template() {
  if [[ -n $TEMPLATE ]]; then
    [[ $DRY -eq 1 || -f $TEMPLATE ]] || fail "template not found: $TEMPLATE"
    [[ $DRY -eq 1 ]] || TEMPLATE=$(absolute "$TEMPLATE")
    return 0
  fi
  fetch_verified_template
}

template_revision() { # prints the infrastructure revision the template describes, or nothing
  awk '/^  InfraRevision:/ { f = 1 } f && /^    Value:/ { gsub(/[^0-9]/, "", $0); print; exit }' "$1"
}

# ---- the metadata service: read before it is switched off for the credentials ---------------
IMDS_ID="" IMDS_REGION="" IMDS_STACK=""
imds() { # PATH: prints the value, or fails
  local tok
  command -v curl >/dev/null 2>&1 || return 1
  tok=$(curl -fsS --connect-timeout 1 -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "$IMDS_BASE/latest/api/token" 2>/dev/null) || return 1
  curl -fsS --connect-timeout 1 -m 2 -H "X-aws-ec2-metadata-token: $tok" "$IMDS_BASE$1" 2>/dev/null
}
read_imds() {
  IMDS_ID=$(imds /latest/meta-data/instance-id) || IMDS_ID=""
  [[ -n $IMDS_ID ]] || return 0
  IMDS_REGION=$(imds /latest/meta-data/placement/region) || IMDS_REGION=""
  IMDS_STACK=$(imds /latest/meta-data/tags/instance/supavise:stack-name) || IMDS_STACK=""
}

# The stack step uses your credentials. The instance role of the node must never be the way a
# stack is changed: the metadata service is switched off for every aws call from here on, and a
# credential that is that role's is refused.
operator_credentials() {
  local arn
  export AWS_EC2_METADATA_DISABLED=true
  command -v aws >/dev/null 2>&1 || fail "the AWS CLI (aws) is not installed: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
  command -v python3 >/dev/null 2>&1 || fail "python3 is needed to read the change set (it is in CloudShell, macOS and Ubuntu)"
  mkwork
  if ! arn=$("${AWS[@]}" sts get-caller-identity --query Arn --output text 2>"$WORK/err"); then
    cat "$WORK/err" >&2
    fail "no usable AWS credentials. Use an administrator's: AWS CloudShell, or export AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN (sudo -E keeps them); the instance role of this node is not used"
  fi
  case $arn in
    *:assumed-role/*/i-[0-9a-f]*) die "these credentials belong to an instance role ($arn). A stack is changed with your own credentials, never the node's" ;;
  esac
  say "Credentials: $arn"
}

# ---- stack facts ---------------------------------------------------------------------------
load_facts() { # STACK REGION TAG: the stack as facts in $WORK/facts.TAG
  mkwork
  "${AWS[@]}" cloudformation describe-stacks --stack-name "$1" --output json >"$WORK/stack.$3.json" 2>"$WORK/err" \
    || { cat "$WORK/err" >&2; fail "cannot read the stack $1 in $2 (name, region, credentials?)"; }
  py facts "$WORK/stack.$3.json" >"$WORK/facts.$3" || fail "cannot read the description of stack $1"
}
fact() { # TAG KEY
  awk -F'\t' -v k="$2" '$1 == k { print substr($0, index($0, "\t") + 1); exit }' "$WORK/facts.$1"
}
stack_ready() { # TAG: refuses a stack that cannot be updated now
  case $(fact "$1" status) in
    CREATE_COMPLETE | UPDATE_COMPLETE | UPDATE_ROLLBACK_COMPLETE | IMPORT_COMPLETE | IMPORT_ROLLBACK_COMPLETE) ;;
    *) die "stack $STACK is $(fact "$1" status): wait until it is complete (or repair it) before changing it" ;;
  esac
}

# Is the service address on this stack's instance? 0 yes, 1 no or unknown (a failover moved it, or
# it cannot be told).
service_address_here() { # TAG
  local alloc ip inst holder
  alloc=$(fact "$1" output:ElasticIpAllocationId)
  ip=$(fact "$1" output:PublicIp)
  inst=$(fact "$1" output:InstanceId)
  if [[ -n $alloc ]]; then
    holder=$("${AWS[@]}" ec2 describe-addresses --allocation-ids "$alloc" --query 'Addresses[0].InstanceId' --output text 2>/dev/null) || return 1
  elif [[ -n $ip ]]; then
    holder=$("${AWS[@]}" ec2 describe-addresses --public-ips "$ip" --query 'Addresses[0].InstanceId' --output text 2>/dev/null) || return 1
  else
    return 1
  fi
  [[ -n $holder && $holder != None && $holder == "$inst" ]]
}

# ---- a template too large for the API goes through S3 ---------------------------------------
TEMPLATE_ARGS=()
ACCOUNT=""
need_account() { # sets ACCOUNT to the account of the credentials
  [[ -n $ACCOUNT ]] || ACCOUNT=$("${AWS[@]}" sts get-caller-identity --query Account --output text) || fail "cannot read the account id"
}
# Every call that names a bucket here passes the account as the expected owner: a bucket that
# someone else made under the predictable name, and opened to this account, is not trusted.
ensure_template_bucket() { # prints the bucket for a staged template of a new stack in $REGION
  local b
  if [[ -n $TBUCKET ]]; then printf '%s' "$TBUCKET"; return 0; fi
  need_account
  b=$NAME-templates-$ACCOUNT-$REGION
  if ! "${AWS[@]}" s3api head-bucket --bucket "$b" --expected-bucket-owner "$ACCOUNT" >/dev/null 2>&1; then
    say "Creating the bucket $b for the template (it stays in your account)" >&2
    "${AWS[@]}" s3 mb "s3://$b" >/dev/null || fail "cannot create the bucket $b (one of that name may belong to another account): give --template-bucket with a bucket of yours in $REGION"
    "${AWS[@]}" s3api put-public-access-block --bucket "$b" --expected-bucket-owner "$ACCOUNT" \
      --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true >/dev/null
  fi
  printf '%s' "$b"
}
stage_template() { # BUCKET: sets TEMPLATE_ARGS; BUCKET is only used when the template needs S3
  local size sum key suffix=amazonaws.com
  size=$(wc -c <"$TEMPLATE" | tr -d ' ')
  if [[ $size -le $INLINE_LIMIT ]]; then
    TEMPLATE_ARGS=(--template-body "file://$TEMPLATE")
    return 0
  fi
  [[ $REGION == cn-* ]] && suffix=amazonaws.com.cn
  sum=$(sha256_of "$TEMPLATE")
  key=_stack/$sum.yaml
  say "The template is $size bytes, over the $INLINE_LIMIT the API takes inline: staging it in s3://$1/$key"
  need_account
  "${AWS[@]}" s3api put-object --bucket "$1" --key "$key" --body "$TEMPLATE" --expected-bucket-owner "$ACCOUNT" >/dev/null \
    || fail "cannot upload the template to s3://$1 (the bucket must be in your account, and the credentials need s3:PutObject there)"
  TEMPLATE_ARGS=(--template-url "https://$1.s3.$REGION.$suffix/$key")
}

# ---- change sets ---------------------------------------------------------------------------
set_target() { # STACK REGION: the stack the next calls act on
  STACK=$1 REGION=$2
  AWS=(aws --region "$REGION")
  [[ -z $PROFILE ]] || AWS+=(--profile "$PROFILE")
}

CS=""
# Creates the change set CS for $STACK from $TEMPLATE_ARGS and $WORK/params.json. TYPE is UPDATE or
# CREATE. Returns 0 with the description in $WORK/cs.json, or 10 when there is nothing to change.
make_change_set() { # TYPE
  local reason tags=()
  CS=$NAME-$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')-$(date -u +%Y%m%dT%H%M%SZ)
  [[ $1 != CREATE ]] || tags=(--tags "Application=$NAME")
  "${AWS[@]}" cloudformation create-change-set --stack-name "$STACK" --change-set-name "$CS" --change-set-type "$1" \
    --capabilities CAPABILITY_IAM --description "Created by $SELF" ${TEMPLATE_ARGS[@]+"${TEMPLATE_ARGS[@]}"} \
    --parameters "file://$WORK/params.json" ${tags[@]+"${tags[@]}"} >/dev/null 2>"$WORK/err" \
    || { cat "$WORK/err" >&2; fail "cannot create the change set"; }
  if ! "${AWS[@]}" cloudformation wait change-set-create-complete --stack-name "$STACK" --change-set-name "$CS" 2>"$WORK/err"; then
    reason=$("${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name "$CS" --query StatusReason --output text 2>/dev/null || true)
    case $reason in
      *"didn't contain changes"* | *"No updates are to be performed"*)
        "${AWS[@]}" cloudformation delete-change-set --stack-name "$STACK" --change-set-name "$CS" >/dev/null 2>&1 || true
        return 10 ;;
    esac
    cat "$WORK/err" >&2
    fail "the change set failed: ${reason:-no reason given}"
  fi
  check_template_of_change_set
  describe_change_set || { drop_change_set; fail "cannot read the change set"; }
}
# The description of CS in $WORK/cs.json, with the values of the properties before and after
# (--include-property-values) when the AWS CLI knows that option. An older CLI refuses it as an
# unknown option, and the review then goes on without the values.
describe_change_set() {
  if "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name "$CS" --include-property-values \
    --output json >"$WORK/cs.json" 2>"$WORK/err"; then
    return 0
  fi
  if grep -q 'Unknown options.*--include-property-values' "$WORK/err"; then
    say "(This AWS CLI does not know --include-property-values: the review goes on without the values of the properties.)"
    "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name "$CS" --output json >"$WORK/cs.json"
    return $?
  fi
  cat "$WORK/err" >&2
  return 1
}
drop_change_set() {
  "${AWS[@]}" cloudformation delete-change-set --stack-name "$STACK" --change-set-name "$CS" >/dev/null 2>&1 || true
}
# CloudFormation keeps the template it read for the change set. A template staged in S3 is out of
# this script's hands between the upload and that read (the instance role may write to the backup
# bucket), so the template the change set holds is read back and must be the file that was verified.
check_template_of_change_set() {
  "${AWS[@]}" cloudformation get-template --stack-name "$STACK" --change-set-name "$CS" --template-stage Original \
    --query TemplateBody --output json >"$WORK/staged.json" 2>"$WORK/err" \
    || { cat "$WORK/err" >&2; drop_change_set; fail "cannot read back the template of the change set to compare it with $TEMPLATE"; }
  py same "$TEMPLATE" "$WORK/staged.json" || {
    drop_change_set
    die "the template in the change set is not the file that was verified ($TEMPLATE): it was changed on its way to CloudFormation. Nothing was changed"
  }
}

FAILED_OVER=0
# What the review of the change set found: ok, guarded (it runs under the guard) or risky (the
# person accepted a refused or guarded change set with --allow-risky, and no guard is set).
REVIEW=""
GUARD_NOTE="Guarded: CloudFormation decides only while the change set runs whether these resources are replaced.
The change set runs under a temporary stack policy (the guard) that denies the replacement or removal of
every resource but a security group rule, an IAM policy or a bucket policy. If CloudFormation decides to
replace or remove one, the update fails, CloudFormation rolls the stack back and nothing is replaced.
The stack's own policy is put back afterwards."
# Shows the change set CS and stops (deleting it) at anything that is not allowed. Without ASK it
# also asks the person to type "apply". FAILED_OVER (0/1) comes from service_address_here.
review_change_set() { # ASK(1/0)
  local rc=0 answer args=()
  [[ $FAILED_OVER -eq 0 ]] || args=(--failed-over)
  say ""
  say "Change set $CS for stack $STACK in $REGION:"
  py classify "$WORK/cs.json" ${args[@]+"${args[@]}"} || rc=$?
  REVIEW=ok
  case $rc in
    0) ;;
    11) drop_change_set; die "refused: the change set could take the service address back from the server it failed over to. Nothing was changed" ;;
    12)
      if [[ $ALLOW_RISKY -eq 1 ]]; then
        say "--allow-risky: no guard is set for this run, so CloudFormation replaces a guarded resource if it decides to."
        confirm_risky
        REVIEW=risky
      else
        say "$GUARD_NOTE"
        REVIEW=guarded
      fi ;;
    10)
      if [[ $ALLOW_RISKY -eq 0 ]]; then
        drop_change_set
        die "refused: the change set replaces or removes something, or changes it in a way this script does not know to be safe. Nothing was changed. A person who has read it can pass --allow-risky"
      fi
      say "--allow-risky: no guard is set for this run, so CloudFormation makes these changes as the change set says."
      confirm_risky
      REVIEW=risky ;;
    *) drop_change_set; fail "could not read the change set (the review exited $rc)" ;;
  esac
  [[ $1 -eq 1 && $YES -eq 0 ]] || return 0
  [[ -t 0 ]] || { drop_change_set; die "not a terminal: pass --yes to run the change set without asking"; }
  printf 'Type apply to run this change set: '
  read -r answer
  [[ $answer == apply ]] || { drop_change_set; die "not applied; the change set was deleted"; }
}
confirm_risky() { # --allow-risky: the person types the stack name
  local answer
  [[ -t 0 ]] || { drop_change_set; die "--allow-risky asks you to type the stack name, which needs a terminal"; }
  printf 'These changes can interrupt the node or lose data. Type the stack name (%s) to run them anyway: ' "$STACK"
  read -r answer
  [[ $answer == "$STACK" ]] || { drop_change_set; die "not confirmed; nothing was changed"; }
}
# The review and the question to the person take time. A failover in between moves the address to
# another server, and a change set that was fine before may take it back: judge it again.
recheck_failover() { # TAG
  local rc=0
  [[ $FAILED_OVER -eq 0 ]] || return 0
  if service_address_here "$1"; then return 0; fi
  FAILED_OVER=1
  py classify "$WORK/cs.json" --failed-over >/dev/null || rc=$?
  case $rc in
    0 | 10 | 12) say "WARNING: the service address moved to another server during the review; the change set does not touch it." ;;
    11) drop_change_set; die "refused: a failover moved the service address while the change set was under review, and the change set could take it back. Nothing was changed" ;;
    *) drop_change_set; fail "could not read the change set (the review exited $rc)" ;;
  esac
}
execute_change_set() {
  "${AWS[@]}" cloudformation execute-change-set --stack-name "$STACK" --change-set-name "$CS" || fail "cannot run the change set"
  say "Running; this takes a few minutes ..."
}
wait_stack() { # create|update
  "${AWS[@]}" cloudformation wait "stack-$1-complete" --stack-name "$STACK" \
    || fail "the stack did not finish; see the Events tab of $STACK in the CloudFormation console (CloudFormation puts a failed update back as it was)"
}

# ---- the guard: a temporary stack policy ----------------------------------------------------
# CloudFormation decides some replacements only while a change set runs: a value it resolves then
# (the {{resolve:ssm:...}} image of a stack made in the console, a Ref or Fn::GetAtt of a resource
# that it might replace) cannot be compared while the change set is made. The review calls those
# changes guarded. A guarded change set runs under a stack policy that denies the replacement and
# the removal of every resource type of the stack but the ones the review lets be replaced or
# removed, so that AWS itself refuses what the review could not rule out: the update then fails and
# CloudFormation rolls the stack back. Afterwards, whether the update succeeded or failed, the
# stack's own policy is put back, or one that allows every update when it had none (a stack policy
# cannot be deleted). That waits until the update has ended, also when the script is interrupted:
# the documentation does not say whether CloudFormation reads the policy once or as it goes.
# (GUARD, GUARD_RUNNING and the rest are set at the top, for on_exit.)

# An interrupt ends the script through on_exit, which takes the guard off.
exit_on_signals() {
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP
}
# Saves the stack's policy and sets the guard; stops (deleting the change set) when it cannot.
set_guard() {
  "${AWS[@]}" cloudformation get-stack-policy --stack-name "$STACK" --output json >"$WORK/policy-own.json" 2>"$WORK/err" \
    || { cat "$WORK/err" >&2; drop_change_set; fail "cannot read the stack policy of $STACK (the credentials need cloudformation:GetStackPolicy), so the guard cannot be set and the change set was deleted. Nothing was changed"; }
  py guard "$WORK/policy-own.json" "$TEMPLATE" "$WORK/cs.json" "$WORK/policy-restore.json" >"$WORK/policy-guard.json" \
    || { drop_change_set; fail "cannot build the guard from the stack policy of $STACK, so the change set was deleted. Nothing was changed"; }
  GUARD_HAD=""
  if grep -q '"StackPolicyBody"' "$WORK/policy-own.json"; then GUARD_HAD=1; fi
  GUARD_STACK=$STACK GUARD_FILE=$WORK/policy-restore.json
  GUARD_AWS=("${AWS[@]}")
  exit_on_signals
  GUARD=1
  if ! "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://$WORK/policy-guard.json" 2>"$WORK/err"; then
    GUARD=0
    cat "$WORK/err" >&2
    drop_change_set
    fail "cannot set the guard on $STACK (the credentials need cloudformation:SetStackPolicy), so the change set was deleted. Nothing was changed"
  fi
  if [[ -n $GUARD_HAD ]]; then
    say "Guard set: the stack's own policy, plus a denial of any replacement or removal but of a rule or a policy."
  else
    say "Guard set: a stack policy that denies any replacement or removal but of a rule or a policy (the stack had no policy)."
  fi
}

# Waits until the update of the guarded stack has ended and sets SETTLED to the stack's status.
# Returns 1 when the status cannot be read, when the update still runs after three waits (each is the
# CLI waiter's, up to an hour), or when the person stops waiting (GIVE_UP, set by a trap). The caller
# sets that trap first, so that Ctrl-C stops the wait and not the script.
SETTLED=""
settle() {
  local i
  for i in 1 2 3 4; do
    [[ $GIVE_UP -eq 0 ]] || return 1
    SETTLED=$("${GUARD_AWS[@]}" cloudformation describe-stacks --stack-name "$GUARD_STACK" --query 'Stacks[0].StackStatus' --output text 2>/dev/null) || SETTLED=""
    case $SETTLED in
      '' | None) SETTLED=""; return 1 ;;
      *_IN_PROGRESS) ;;
      *) return 0 ;;
    esac
    [[ $i -lt 4 ]] || return 1
    printf '%s: the update of %s is still running (%s); the guard comes off when it ends. Waiting (Ctrl-C leaves the guard on) ...\n' "$SELF" "$GUARD_STACK" "$SETTLED" >&2
    "${GUARD_AWS[@]}" cloudformation wait stack-update-complete --stack-name "$GUARD_STACK" >/dev/null 2>&1 || true
  done
  return 1
}

# Puts the stack's own policy back. 0 done, 1 it could not, which it says loudly.
restore_guard() {
  [[ $GUARD -eq 1 ]] || return 0
  if "${GUARD_AWS[@]}" cloudformation set-stack-policy --stack-name "$GUARD_STACK" --stack-policy-body "file://$GUARD_FILE" 2>"$WORK/err"; then
    GUARD=0
    if [[ -n $GUARD_HAD ]]; then
      say "Guard taken off: the stack's own policy is back."
    else
      say "Guard taken off: the stack's policy allows every update again (it had none, and a stack policy cannot be deleted)."
    fi
    return 0
  fi
  cat "$WORK/err" >&2
  guard_left_on "it could not be put back"
  return 1
}
guard_left_on() { # REASON
  GUARD=0
  {
    printf '\n%s: WARNING: THE GUARD IS STILL ON STACK %s (%s).\n' "$SELF" "$GUARD_STACK" "$1"
    printf 'While it is on, CloudFormation refuses to replace or remove most resources of the stack. Once the stack\n'
    printf 'is no longer updating, put its own policy back with:\n'
    printf '  %s\n\n' "$(show "${GUARD_AWS[@]}" cloudformation set-stack-policy --stack-name "$GUARD_STACK" --stack-policy-body "$(cat "$GUARD_FILE")")"
  } >&2
}
# The script ends with the guard on (an interrupt, a failure): the guard comes off once the update
# has ended. A second interrupt stops the wait and leaves it on, with the command to take it off.
guard_off_at_exit() {
  trap 'GIVE_UP=1' INT TERM HUP
  if [[ $GUARD_RUNNING -eq 1 ]] && ! settle; then
    guard_left_on "the update may still be running, or its state could not be read"
    return 0
  fi
  restore_guard || true
}

# Runs the change set CS under the guard, waits for it, and takes the guard off.
run_guarded() {
  local status
  GUARD_RUNNING=1
  execute_change_set
  if "${AWS[@]}" cloudformation wait stack-update-complete --stack-name "$STACK"; then
    GUARD_RUNNING=0
    restore_guard || true
    return 0
  fi
  # The waiter stops at a failure, and after an hour. From here Ctrl-C stops a wait, not the script.
  trap 'GIVE_UP=1' INT TERM HUP
  if ! settle; then
    guard_left_on "the update may still be running, or its state could not be read"
    fail "cannot tell whether the update of $STACK has ended; see its Events tab in the CloudFormation console"
  fi
  status=$SETTLED
  GUARD_RUNNING=0
  if [[ $status == UPDATE_COMPLETE ]]; then
    restore_guard || true
    exit_on_signals
    return 0
  fi
  say ""
  case $status in
    UPDATE_ROLLBACK_COMPLETE)
      say "The update failed, and CloudFormation rolled the stack back: it is as it was before the update." ;;
    UPDATE_ROLLBACK_FAILED)
      say "The update failed, and CloudFormation could not finish rolling the stack back (UPDATE_ROLLBACK_FAILED)."
      say "See the Events tab of $STACK; aws cloudformation continue-update-rollback --stack-name $STACK resumes the rollback." ;;
    *)
      say "The update did not complete: the stack is $status." ;;
  esac
  say "Nothing was replaced: the guard let CloudFormation replace or remove nothing but a security group rule, an IAM policy or a bucket policy."
  guard_refusals
  restore_guard || true
  fail "the update of $STACK did not complete ($status); see the Events tab of $STACK in the CloudFormation console"
}
# Names what CloudFormation wanted to replace or remove and the guard denied, from the events of the
# update (the credentials need cloudformation:DescribeStackEvents; without it the events are skipped).
guard_refusals() {
  : >"$WORK/failures"
  if "${AWS[@]}" cloudformation describe-stack-events --stack-name "$STACK" --max-items 100 --output json >"$WORK/events.json" 2>/dev/null; then
    py failures "$WORK/events.json" "$STACK" >"$WORK/failures" 2>/dev/null || : >"$WORK/failures"
  fi
  if grep -qi 'stack policy' "$WORK/failures"; then
    say "CloudFormation decided to replace or remove these while the change set ran, and the guard denied it:"
    grep -i 'stack policy' "$WORK/failures" | awk -F'\t' '{ printf "  %s: %s\n", $1, $2 }'
  elif [[ -s $WORK/failures ]]; then
    say "What failed:"
    awk -F'\t' '{ printf "  %s: %s\n", $1, $2 }' "$WORK/failures"
  fi
}

REPAIR_RULE="Before you replace an instance of this stack (a changed image or user data does), set SupaviseVersion to
the release the node runs now: a replacement installs SupaviseVersion over the data volume, and the version the
stack was made with is older than the node. $SELF update never changes SupaviseVersion."

# ---- update --------------------------------------------------------------------------------
UPDATE_SETS=()
# The bucket a large template is staged in: the person's, else the stack's backup bucket.
STAGE_BUCKET=""
# Updates $STACK, read as facts TAG, with the parameters it has and UPDATE_SETS. HOW is run, or
# check: check makes and reviews the change set and drops it.
run_update() { # TAG HOW
  local tag=$1 how=$2 ami inst image have want bucket rc=0 sets=()
  # (bash 3.2 fails on the expansion of an empty array; a value may hold spaces, so no unquoted copy)
  if [[ ${#UPDATE_SETS[@]} -gt 0 ]]; then sets=("${UPDATE_SETS[@]}"); fi
  inst=$(fact "$tag" output:InstanceId)
  # A stack made in the console with an empty AmiId would look the image up again; give it the one
  # its instance runs, so that the update cannot replace the instance for a newer image.
  ami=$(fact "$tag" param:AmiId)
  if [[ -z $ami || $ami == None ]] && grep -q '^  AmiId:' "$TEMPLATE" && [[ -n $inst ]]; then
    image=$("${AWS[@]}" ec2 describe-instances --instance-ids "$inst" --query 'Reservations[0].Instances[0].ImageId' --output text) \
      || fail "cannot read the image of $inst"
    [[ $image =~ $re_ami ]] || fail "did not get an image ID for $inst (got: $image)"
    say "The stack has no AmiId: keeping the image its instance runs, $image"
    sets+=("AmiId=$image")
  fi
  # (exit 2 is a refusal of a --set; anything else is the helper failing, which is a failure)
  py params "$WORK/stack.$tag.json" "$TEMPLATE" ${sets[@]+"${sets[@]}"} >"$WORK/params.json" \
    || { [[ $? -ne 2 ]] || exit 2; fail "cannot build the parameters of the update from the description of stack $STACK"; }

  if service_address_here "$tag"; then
    FAILED_OVER=0
  else
    FAILED_OVER=1
    say "WARNING: the service address of $STACK is not on its instance ${inst:-?} (a failover moved it, or it cannot be read)."
    say "         The change set is refused if it touches the address association or updates the instance."
  fi
  have=$(fact "$tag" output:InfraRevision); have=${have:-1}
  want=$(template_revision "$TEMPLATE")
  say "Stack $STACK is at infrastructure revision $have; the template is at revision $want."
  if [[ $want -lt $have ]]; then
    die "an update does not move a stack back to an older template (the stack is at revision $have, this template at $want); use the template of the release the stack runs, or a newer one"
  fi

  bucket=$STAGE_BUCKET
  if [[ -z $bucket ]] && [[ $(wc -c <"$TEMPLATE" | tr -d ' ') -gt $INLINE_LIMIT ]]; then
    bucket=$(fact "$tag" output:BackupBucket)
    # The stack of a replica server shows its leader's bucket. CloudFormation reads a staged template
    # with this stack's region, which fails for a bucket of the leader's other region.
    if [[ -n $(fact "$tag" param:JoinLeader) && $(fact "$tag" param:BackupBucketRegion) != "$REGION" ]]; then bucket=""; fi
    [[ -n $bucket ]] || bucket=$(ensure_template_bucket)
  fi
  stage_template "$bucket"
  make_change_set UPDATE || rc=$?
  if [[ $rc -eq 10 ]]; then
    say "Nothing to change: the stack already matches this template."
    return 0
  fi
  if [[ $how == check ]]; then
    review_change_set 0
    drop_change_set
    return 0
  fi
  review_change_set 1
  recheck_failover "$tag"
  if [[ $REVIEW == guarded ]]; then
    set_guard
    run_guarded
  else
    execute_change_set
    wait_stack update
  fi
  load_facts "$STACK" "$REGION" "$tag"
  have=$(fact "$tag" output:InfraRevision)
  say ""
  say "Updated. Stack $STACK${have:+ is at infrastructure revision $have}."
}

# Where the stack is: --stack and --region, else the node's own instance tags and region.
locate_stack() {
  read_imds
  if [[ -z $STACK_GIVEN ]]; then
    [[ -n $IMDS_STACK ]] || die "--stack is required (this host's instance tags do not name a stack; a stack from before revision 2 has none)"
    STACK=$IMDS_STACK
  elif [[ -n $IMDS_STACK && $IMDS_STACK != "$STACK" ]]; then
    die "this instance's tags say it belongs to stack $IMDS_STACK, not $STACK"
  fi
  if [[ -z $REGION ]]; then
    REGION=$IMDS_REGION
    [[ -n $REGION ]] || die "--region is required (or set AWS_REGION)"
  elif [[ -z $REGION_GIVEN && -n $IMDS_REGION && $IMDS_REGION != "$REGION" && $IMDS_STACK == "$STACK" ]]; then
    # The region came from the environment, and `sudo -E` on the node keeps whatever the operator has
    # exported. This node's own tags say the stack is the one it belongs to, and an instance belongs
    # to a stack of its own region; a region given with --region is taken as it is.
    say "Note: the environment says region $REGION, but this node's stack $STACK is in $IMDS_REGION: using $IMDS_REGION (--region overrides)."
    REGION=$IMDS_REGION
  fi
  [[ $REGION =~ $re_region ]] || die "--region $REGION is not a region name such as us-east-1"
  set_target "$STACK" "$REGION"
}

mode_update() {
  local inst
  if [[ $DRY -eq 1 ]]; then
    note "dry run: nothing is sent to AWS"
    note "the instance's id, region and stack tag are read from the metadata service first, which is then switched off for"
    note "every aws call: the stack is changed with your credentials, and the instance role is refused:"
    show "${AWS[@]}" sts get-caller-identity --query Arn --output text
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --output json
    note "the stack's parameters are kept (UsePreviousValue) and the --set ones given; SupaviseVersion is never set."
    note "Is the service address still on the stack's instance, or did a failover move it?"
    show "${AWS[@]}" ec2 describe-addresses --allocation-ids "<ElasticIpAllocationId>" --query 'Addresses[0].InstanceId' --output text
    resolve_template
    note "a template over $INLINE_LIMIT bytes is staged in the stack's backup bucket (or --template-bucket), which must be in your account;"
    note "the stack of a replica server in another region than its leader's bucket uses $NAME-templates-<account>-<region> instead:"
    show "${AWS[@]}" s3api put-object --bucket "<BackupBucket>" --key "_stack/<sha256>.yaml" --body "$TEMPLATE" --expected-bucket-owner "<account>"
    show "${AWS[@]}" cloudformation create-change-set --stack-name "$STACK" --change-set-name "$NAME-update-<time>" --change-set-type UPDATE --capabilities CAPABILITY_IAM --template-url "<staged template>" --parameters "file://<params.json>"
    show "${AWS[@]}" cloudformation wait change-set-create-complete --stack-name "$STACK" --change-set-name "$NAME-update-<time>"
    note "the template the change set holds is read back and must be the file that was verified; if it is not, the change set is deleted:"
    show "${AWS[@]}" cloudformation get-template --stack-name "$STACK" --change-set-name "$NAME-update-<time>" --template-stage Original --query TemplateBody --output json
    note "the change set is read with the values of its properties (an AWS CLI that does not know --include-property-values reads it without them):"
    show "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name "$NAME-update-<time>" --include-property-values --output json
    note "every resource change is reviewed; a replacement or removal (but of a security group rule or an IAM policy) is refused and the change set deleted. Otherwise, after you type apply:"
    show "${AWS[@]}" cloudformation execute-change-set --stack-name "$STACK" --change-set-name "$NAME-update-<time>"
    show "${AWS[@]}" cloudformation wait stack-update-complete --stack-name "$STACK"
    note "a guarded change set (CloudFormation decides only while it runs whether a resource is replaced) runs under a temporary stack policy"
    note "that denies the replacement or removal of every resource but a security group rule, an IAM policy or a bucket policy; the stack's"
    note "own policy is saved first and put back once the update has ended (one that allows every update when it had none), also after a failure:"
    show "${AWS[@]}" cloudformation get-stack-policy --stack-name "$STACK" --output json
    show "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://<the guard>"
    show "${AWS[@]}" cloudformation execute-change-set --stack-name "$STACK" --change-set-name "$NAME-update-<time>"
    show "${AWS[@]}" cloudformation wait stack-update-complete --stack-name "$STACK"
    show "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://<the stack's own policy>"
    note "with --allow-risky no guard is set"
    return 0
  fi
  FAIL_CODE=3
  locate_stack
  operator_credentials
  load_facts "$STACK" "$REGION" s
  stack_ready s
  inst=$(fact s output:InstanceId)
  # A stack from before revision 2 has no instance tags to name it, so on a node its InstanceId must
  # be this instance's. A stack at revision 2 is told apart by the node's tags (locate_stack), and a
  # host that is not a node of the stack (a jump host, another server of the cluster) may update it.
  if [[ -z $(fact s output:InfraRevision) && -n $IMDS_ID && -n $inst && $IMDS_ID != "$inst" ]]; then
    die "stack $STACK belongs to instance $inst, and this is $IMDS_ID: --stack names another node's stack (a stack from before revision 2 is updated from the node it belongs to, or from AWS CloudShell)"
  fi
  resolve_template
  [[ -n $(template_revision "$TEMPLATE") ]] || die "$TEMPLATE has no InfraRevision output: it is not a Supavise template of this release line"
  UPDATE_SETS=()
  if [[ ${#SETS[@]} -gt 0 ]]; then UPDATE_SETS=("${SETS[@]}"); fi
  STAGE_BUCKET=$TBUCKET
  run_update s run
  say "The node reads its stack from its instance tags: run  supavise status  on it."
}

# ---- status --------------------------------------------------------------------------------
mode_status() {
  local inst alloc ip rev want holder
  if [[ $DRY -eq 1 ]]; then
    note "dry run: nothing is sent to AWS"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --output json
    show "${AWS[@]}" ec2 describe-addresses --allocation-ids "<ElasticIpAllocationId>" --query 'Addresses[0].InstanceId' --output text
    return 0
  fi
  FAIL_CODE=3
  locate_stack
  operator_credentials
  load_facts "$STACK" "$REGION" s
  inst=$(fact s output:InstanceId)
  alloc=$(fact s output:ElasticIpAllocationId)
  ip=$(fact s output:PublicIp)
  rev=$(fact s output:InfraRevision)
  say "Stack:            $STACK ($REGION) $(fact s status)"
  say "Instance:         ${inst:-none}"
  say "Release:          SupaviseVersion=$(fact s param:SupaviseVersion), what the stack installs on a new instance ($SELF update never changes it)"
  if [[ -n $rev ]]; then say "Infrastructure:   revision $rev"; else say "Infrastructure:   revision 1 (a stack from before revisions)"; fi
  rev=${rev:-1}
  if [[ -n $TEMPLATE && -f $TEMPLATE ]]; then
    want=$(template_revision "$TEMPLATE")
    if [[ -z $want ]]; then
      say "Template:         $TEMPLATE has no InfraRevision output"
    elif [[ $want -gt $rev ]]; then
      say "Template:         revision $want: run  $SELF update --stack $STACK --region $REGION"
    else
      say "Template:         revision $want: the stack is up to date"
    fi
  else
    say "Template:         pass --template FILE to compare, or run update"
  fi
  if [[ -n $alloc || -n $ip ]]; then
    if [[ -n $alloc ]]; then
      holder=$("${AWS[@]}" ec2 describe-addresses --allocation-ids "$alloc" --query 'Addresses[0].InstanceId' --output text 2>/dev/null) || holder="?"
    else
      holder=$("${AWS[@]}" ec2 describe-addresses --public-ips "$ip" --query 'Addresses[0].InstanceId' --output text 2>/dev/null) || holder="?"
    fi
    if [[ $holder == "$inst" ]]; then
      say "Service address:  ${ip:-$alloc} is on this stack's instance"
    elif [[ $holder == "?" ]]; then
      say "Service address:  ${ip:-$alloc} (its association could not be read)"
    else
      say "Service address:  ${ip:-$alloc} is NOT on this stack's instance but on ${holder/None/no instance}: a failover moved it. update leaves the association alone"
    fi
  fi
  say ""
  say "$REPAIR_RULE"
}

# ---- replica -------------------------------------------------------------------------------
# The new server's Elastic IP, as soon as CloudFormation has made it (long before the instance
# has booted): the leader must let that address in before the new server tries to join.
wait_for_elastic_ip() {
  local i st status
  for i in $(seq 1 180); do
    st=$("${AWS[@]}" cloudformation describe-stack-resource --stack-name "$STACK" --logical-resource-id ElasticIp \
      --query 'StackResourceDetail.[ResourceStatus,PhysicalResourceId]' --output text 2>/dev/null) || st=""
    case $st in CREATE_COMPLETE*) printf '%s' "${st##*[[:space:]]}"; return 0 ;; esac
    status=$("${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text 2>/dev/null) || status=""
    case $status in *FAILED* | ROLLBACK* | DELETE*) return 1 ;; esac
    sleep 5
  done
  return 1
}

mode_replica() {
  local lip bkt obj role cluster email lrev token_arn secret_name arch ssm_name image bucket slot i v mode ip yes p=()
  if [[ $DRY -eq 1 ]]; then
    note "dry run: nothing is sent to AWS"
    note "reads the leader stack's outputs (it needs infrastructure revision 2: run update first):"
    show aws --region "$LEADER_REGION" cloudformation describe-stacks --stack-name "$LEADER_STACK" --output json
    resolve_template
    note "checks that the leader's security group can be opened to the new server (a change set of the leader stack that is reviewed and deleted):"
    show "$SELF" update --stack "$LEADER_STACK" --region "$LEADER_REGION" --set "PeerCidr<n>=<new Elastic IP>/32"
    note "puts the join token in a secret of the new server's region (the file is read by the CLI, never put on a command line):"
    show "${AWS[@]}" secretsmanager create-secret --name "$NAME-join/$STACK/<time>" --secret-string "file://$TOKEN_FILE" --query ARN --output text
    note "creates the new server's stack from the same template (a change set, reviewed like an update):"
    show "${AWS[@]}" cloudformation create-change-set --stack-name "$STACK" --change-set-name "$NAME-create-<time>" --change-set-type CREATE --capabilities CAPABILITY_IAM --template-url "<staged template>" --parameters "file://<params.json>"
    show "${AWS[@]}" cloudformation get-template --stack-name "$STACK" --change-set-name "$NAME-create-<time>" --template-stage Original --query TemplateBody --output json
    show "${AWS[@]}" cloudformation execute-change-set --stack-name "$STACK" --change-set-name "$NAME-create-<time>"
    note "as soon as the new stack has its Elastic IP, the leader's security group is opened to it on port 7443:"
    show "${AWS[@]}" cloudformation describe-stack-resource --stack-name "$STACK" --logical-resource-id ElasticIp --query 'StackResourceDetail.[ResourceStatus,PhysicalResourceId]' --output text
    show "$SELF" update --stack "$LEADER_STACK" --region "$LEADER_REGION" --set "PeerCidr<n>=<new Elastic IP>/32"
    show "${AWS[@]}" cloudformation wait stack-create-complete --stack-name "$STACK"
    return 0
  fi
  FAIL_CODE=3
  [[ -s $TOKEN_FILE && -r $TOKEN_FILE ]] || die "--token-file $TOKEN_FILE is empty or unreadable"
  mode=$(stat -c %a "$TOKEN_FILE" 2>/dev/null || stat -f %Lp "$TOKEN_FILE" 2>/dev/null || echo 600)
  case $mode in *[1-7][0-7] | *[0-7][1-7]) die "--token-file $TOKEN_FILE can be read by other users (mode $mode): chmod 600 it" ;; esac
  operator_credentials

  # The leader: its outputs say where the new server joins and what it uses.
  local new_stack=$STACK new_region=$REGION
  set_target "$LEADER_STACK" "$LEADER_REGION"
  load_facts "$LEADER_STACK" "$LEADER_REGION" l
  stack_ready l
  lrev=$(fact l output:InfraRevision)
  [[ -n $lrev && $lrev -ge 2 ]] || die "stack $LEADER_STACK is at infrastructure revision ${lrev:-1}: run  $SELF update --stack $LEADER_STACK --region $LEADER_REGION  first"
  lip=$(fact l output:PublicIp); bkt=$(fact l output:BackupBucket); obj=$(fact l output:ObjectsBucket)
  role=$(fact l output:StorageRoleArn); cluster=$(fact l output:ClusterName); email=${EMAIL:-$(fact l param:AdminEmail)}
  [[ -n $lip && -n $bkt && -n $obj && -n $role && -n $cluster ]] \
    || die "stack $LEADER_STACK lacks an output a replica server needs (PublicIp, BackupBucket, ObjectsBucket, StorageRoleArn, ClusterName)"
  [[ $email =~ $re_email ]] || die "cannot tell the contact address: give --email"
  resolve_template
  [[ -n $(template_revision "$TEMPLATE") ]] || die "$TEMPLATE has no InfraRevision output: it is not a Supavise template of this release line"

  # Which PeerCidr of the leader the new server takes, and whether the leader's change is allowed,
  # are settled before anything exists: a refusal there must stop everything here.
  slot=""
  for i in 1 2 3; do
    v=$(fact l "param:PeerCidr$i")
    if [[ -z $v && -z $slot ]]; then slot=$i; fi
  done
  [[ -n $slot ]] || die "stack $LEADER_STACK uses all three PeerCidr parameters: widen one to a range that covers the new server, then run this again"
  UPDATE_SETS=("PeerCidr$slot=192.0.2.1/32")
  STAGE_BUCKET=""
  say "Checking that stack $LEADER_STACK can be opened to the new server ..."
  run_update l check

  # The new server's stack: the join token in a secret of its region, the image, the parameters.
  set_target "$new_stack" "$new_region"
  secret_name=$NAME-join/$STACK/$(date -u +%Y%m%dT%H%M%SZ)
  arch=amd64
  if [[ ${ITYPE:-t4g.large} =~ ^[a-z]+[0-9]+g[a-z]*\. ]]; then arch=arm64; fi
  ssm_name=/aws/service/canonical/ubuntu/server/24.04/stable/current/$arch/hvm/ebs-gp3/ami-id
  image=$AMI
  if [[ -z $image ]]; then
    image=$("${AWS[@]}" ssm get-parameter --name "$ssm_name" --query Parameter.Value --output text) \
      || fail "cannot look up the Ubuntu image in $REGION; pass --ami-id"
  fi
  [[ $image =~ $re_ami ]] || fail "did not get an image ID (got: $image); pass --ami-id"
  p=("AdminEmail=$email" "AmiId=$image" "JoinLeader=$lip:7443" "BackupBucketName=$bkt" "BackupBucketRegion=$LEADER_REGION"
    "ObjectsBucketName=$obj" "StorageRoleArn=$role" "ClusterName=$cluster" "PeerCidr1=$lip/32")
  [[ -z $ITYPE ]] || p+=("InstanceType=$ITYPE")
  [[ -z $VOLSIZE ]] || p+=("DataVolumeSize=$VOLSIZE")
  [[ -z $SNAPS ]] || p+=("DailySnapshotsKept=$SNAPS")
  [[ -z $VERSION ]] || p+=("SupaviseVersion=$VERSION")
  [[ -z $ACCESS ]] || p+=("AccessCidr=$ACCESS")
  [[ -z $SSH ]] || p+=("SshCidr=$SSH")
  [[ -z $KEY ]] || p+=("KeyName=$KEY")
  [[ -z $SSM ]] || p+=("EnableSessionManager=$SSM")
  if [[ -n $VPC ]]; then p+=("VpcId=$VPC" "SubnetId=$SUBNET"); else p+=("AvailabilityZone=$AZ"); fi

  if [[ -n $TBUCKET ]]; then bucket=$TBUCKET
  elif [[ $REGION == "$LEADER_REGION" ]]; then bucket=$bkt
  else bucket=$(ensure_template_bucket); fi
  stage_template "$bucket"
  say ""
  say "This creates stack $STACK in $REGION (a server in $AZ that joins $lip:7443) and, once its Elastic IP exists,"
  say "opens stack $LEADER_STACK to that address on port 7443 (PeerCidr$slot)."
  token_arn=$("${AWS[@]}" secretsmanager create-secret --name "$secret_name" --description "Supavise join token for $STACK (single use)" \
    --secret-string "file://$TOKEN_FILE" --tags "Key=Application,Value=$NAME" --query ARN --output text) \
    || fail "cannot store the join token in Secrets Manager"
  # The token's ARN is a parameter of the stack, so the secret comes first. It is removed again if
  # the script stops before the stack exists.
  EARLY_SECRET=$token_arn
  say "Join token stored as $secret_name (single use; deleted once the server has joined)"
  p+=("JoinTokenSecretArn=$token_arn")
  python3 -c 'import json,sys; print(json.dumps([{"ParameterKey": a.split("=", 1)[0], "ParameterValue": a.split("=", 1)[1]} for a in sys.argv[1:]]))' "${p[@]}" >"$WORK/params.json"
  FAILED_OVER=0
  make_change_set CREATE || true
  # The change set made the stack, empty, in REVIEW_IN_PROGRESS: it stays if the change set is not run.
  LEFTOVER="Stack $STACK in $REGION exists but is empty, because its change set was not run. Delete it with:
  aws cloudformation delete-stack --region $REGION --stack-name $STACK"
  review_change_set 1
  execute_change_set
  EARLY_SECRET=""
  LEFTOVER="Stack $STACK in $REGION is being created and its server cannot join until stack $LEADER_STACK lets it in on port 7443.
To open the leader, once the Elastic IP of $STACK is known:  $SELF update --stack $LEADER_STACK --region $LEADER_REGION --set PeerCidr$slot=ADDRESS/32
To take the new server away again:
  aws cloudformation delete-stack --region $REGION --stack-name $STACK
  aws secretsmanager delete-secret --region $REGION --force-delete-without-recovery --secret-id $token_arn"

  # The leader lets the new address in as soon as it exists.
  ip=$(wait_for_elastic_ip) || fail "stack $STACK did not get its Elastic IP; see its Events tab. The leader was not changed"
  say "The new server's address is $ip"
  set_target "$LEADER_STACK" "$LEADER_REGION"
  load_facts "$LEADER_STACK" "$LEADER_REGION" l
  UPDATE_SETS=("PeerCidr$slot=$ip/32")
  yes=$YES; YES=1
  run_update l run
  YES=$yes
  set_target "$new_stack" "$new_region"
  say "Waiting for the new server to install and join ..."
  LEFTOVER="Stack $STACK in $REGION did not finish creating (stack $LEADER_STACK was opened to $ip/32 as PeerCidr$slot): see its Events tab.
The join token secret stays until you delete it:
  aws secretsmanager delete-secret --region $REGION --force-delete-without-recovery --secret-id $token_arn"
  wait_stack create
  LEFTOVER=""
  "${AWS[@]}" secretsmanager delete-secret --secret-id "$token_arn" --force-delete-without-recovery >/dev/null 2>&1 \
    || say "Delete the spent join token yourself: aws secretsmanager delete-secret --force-delete-without-recovery --secret-id $token_arn"
  load_facts "$STACK" "$REGION" n
  say ""
  say "The new server is up: stack $STACK ($REGION), Elastic IP $ip."
  say "On the leader:  supavise node ls     (the new server shows as active once its system cluster streams)"
  say "Shell on it (Session Manager): $(fact n output:ConnectCommand)"
}

# ---- purge ---------------------------------------------------------------------------------
# What a deleted stack leaves in the account: the backup and objects buckets (they are retained
# and versioned) and the snapshots of the data volume (the final one CloudFormation takes, and the
# daily ones). purge finds them by the marks the stack put on them and never by a name alone, lists
# them, and destroys them after one confirmation. It needs the AWS CLI and nothing else: the
# batches of an object delete are written by the CLI's own --query.
STACK_ID=""
PB_NAME=() PB_WHAT=() PB_VERS=() PB_MARKS=() PB_BYTES=() PB_USERS=()  # the buckets: name, role, versions, delete markers, bytes, other stacks that use it
PS_ID=() PS_VOL=() PS_SIZE=() PS_WHEN=() PS_STATE=()      # the snapshots
re_vol='^vol-[0-9a-f]{8,17}$'

# A volume ID that names a data volume. EC2 reports vol-ffffffff as the volume of every snapshot that
# is a copy (of a snapshot or of an image) and of the ones an image holds: one value for unrelated
# snapshots of the whole account, so it identifies nothing and a filter on it would find them all.
real_vol() { # ID
  [[ $1 =~ $re_vol && ! $1 =~ $re_allf ]]
}

# The queries below are JMESPath, which the AWS CLI evaluates; its literals are written in backticks,
# which are not command substitutions here.
# shellcheck disable=SC2016
Q_COUNT='[length(Versions || `[]`), length(DeleteMarkers || `[]`), sum(Versions[].Size || `[0]`)]'
# The delete-objects body of one page of a bucket (at most 1000 entries): its object versions and its
# delete markers together, in the order S3 lists them. The two kinds are mixed in key order, so a run
# of one kind can fill the whole page; taking both from every page is what keeps a bucket whose first
# 1000 entries are all delete markers (the ones left when old versions expire) from stopping the loop
# with versions behind them. The CLI leaves "Objects": [] when the page has nothing.
# shellcheck disable=SC2016
Q_BATCH='{Objects: [Versions, DeleteMarkers][].{Key: Key, VersionId: VersionId}, Quiet: `true`}'

human_bytes() { # BYTES
  awk -v b="$1" 'BEGIN { split("B KiB MiB GiB TiB PiB", u, " "); i = 1; while (b >= 1024 && i < 6) { b /= 1024; i++ } if (i == 1) printf "%d B", b; else printf "%.1f %s", b, u[i] }'
}

# The CloudFormation tags of a bucket, as "stack-name<TAB>stack-id<TAB>logical-id" (empty fields when
# the bucket has none of them).
bucket_origin() { # BUCKET
  local k v tn="" ti="" tl="" tags
  tags=$("${AWS[@]}" s3api get-bucket-tagging --bucket "$1" --expected-bucket-owner "$ACCOUNT" --query 'TagSet[].[Key,Value]' --output text 2>/dev/null) || tags=""
  while IFS=$'\t' read -r k v; do
    case $k in
      aws:cloudformation:stack-name) tn=$v ;;
      aws:cloudformation:stack-id) ti=$v ;;
      aws:cloudformation:logical-id) tl=$v ;;
    esac
  done <<<"$tags"
  printf '%s\t%s\t%s' "$tn" "$ti" "$tl"
}

# Counts the object versions, delete markers and bytes of a bucket and adds it to the list.
add_bucket() { # NAME ROLE
  local out v m b
  out=$("${AWS[@]}" s3api list-object-versions --bucket "$1" --expected-bucket-owner "$ACCOUNT" \
    --query "$Q_COUNT" --output text) \
    || fail "cannot list the object versions of $1 (the credentials need s3:ListBucketVersions on it)"
  # (the CLI prints one line per page of a large bucket)
  read -r v m b <<<"$(printf '%s\n' "$out" | awk '{ v += $1; m += $2; s += $3 } END { printf "%d %d %.0f", v, m, s }')"
  PB_NAME+=("$1")
  PB_WHAT+=("$2")
  PB_VERS+=("$v")
  PB_MARKS+=("$m")
  PB_BYTES+=("$b")
  PB_USERS+=("$(bucket_users "$1")")
}

# The other stacks of this region that name the bucket as their BackupBucketName or ObjectsBucketName:
# the stacks of replica servers, which keep their backups and Storage files in their leader's buckets.
# Stacks of other regions are not looked at. Prints their names, nothing when there are none, or ?
# when the stacks cannot be listed (the list says so then).
bucket_users() { # BUCKET
  local names
  [[ $1 =~ $re_bucket ]] || return 0
  names=$("${AWS[@]}" cloudformation describe-stacks \
    --query "Stacks[?StackName!='$STACK' && Parameters[?(ParameterKey=='BackupBucketName' || ParameterKey=='ObjectsBucketName') && ParameterValue=='$1']].StackName" \
    --output text 2>/dev/null) || { printf '?'; return 0; }
  # (the CLI prints the names of each page of a long list on a line of their own)
  printf '%s' "$names" | tr -s '\t\n' '  ' | sed 's/^ //; s/ $//; s/^None$//'
}

describe_snapshots() { # FILTER: one "id<TAB>volume<TAB>GiB<TAB>started<TAB>state" line per snapshot
  "${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "$1" \
    --query 'Snapshots[].[SnapshotId,VolumeId,VolumeSize,StartTime,State]' --output text
}

# Fills PS_* with the snapshots of a stack: those that carry its supavise:stack tag (TAGVALUE; the
# daily ones do, the volume's tags are copied to them), and every other snapshot of the volumes those
# were taken from and of the volumes given (the final snapshot CloudFormation takes when the stack
# deletes its data volume names no stack, but it names the volume).
collect_snapshots() { # TAGVALUE [VOLUME...]
  local tagval=$1 vols="" v lines more id vol size when state
  shift
  for v in "$@"; do
    if real_vol "$v"; then vols="$vols $v"; fi
  done
  lines=$(describe_snapshots "Name=tag:supavise:stack,Values=$tagval") \
    || fail "cannot list the snapshots (the credentials need ec2:DescribeSnapshots)"
  while IFS=$'\t' read -r id vol _; do
    [[ -n $id ]] || continue
    if real_vol "$vol"; then
      case "$vols " in *" $vol "*) ;; *) vols="$vols $vol" ;; esac
    fi
  done <<<"$lines"
  if [[ -n $vols ]]; then
    vols=${vols# }
    more=$(describe_snapshots "Name=volume-id,Values=${vols// /,}") \
      || fail "cannot list the snapshots of the data volume (the credentials need ec2:DescribeSnapshots)"
    lines=$(printf '%s\n%s\n' "$lines" "$more")
  fi
  PS_ID=() PS_VOL=() PS_SIZE=() PS_WHEN=() PS_STATE=()
  while IFS=$'\t' read -r id vol size when state; do
    [[ $id =~ $re_snap ]] || continue
    PS_ID+=("$id")
    PS_VOL+=("$vol")
    PS_SIZE+=("$size")
    PS_WHEN+=("$when")
    PS_STATE+=("$state")
  done <<<"$(printf '%s\n' "$lines" | awk -F'\t' 'NF && !seen[$1]++' | sort -t"$(printf '\t')" -k4,4)"
}

print_purge_list() { # what purge will destroy
  local i
  if [[ ${#PB_NAME[@]} -gt 0 ]]; then
    say "S3 buckets (every object version and delete marker, then the bucket itself):"
    for ((i = 0; i < ${#PB_NAME[@]}; i++)); do
      say "  ${PB_NAME[$i]}   ${PB_WHAT[$i]}: ${PB_VERS[$i]} object version(s), ${PB_MARKS[$i]} delete marker(s), $(human_bytes "${PB_BYTES[$i]}")"
      case "${PB_USERS[$i]}" in
        '') ;;
        '?') say "    (the other stacks of $REGION could not be listed, so it is not known whether a replica server still uses it)" ;;
        *) say "    WARNING: stack(s) ${PB_USERS[$i]} in $REGION use it as their BackupBucketName or ObjectsBucketName; their servers lose their backups or Storage files" ;;
      esac
    done
    say "  (A replica server keeps its backups and Storage files in its leader's buckets. Stacks of $REGION that name one are shown above; servers in other regions are not looked at.)"
  fi
  if [[ ${#PS_ID[@]} -gt 0 ]]; then
    say "EBS snapshots (the data volume: the master key, the registry and every project):"
    for ((i = 0; i < ${#PS_ID[@]}; i++)); do
      say "  ${PS_ID[$i]}   ${PS_SIZE[$i]} GiB   ${PS_WHEN[$i]}   ${PS_STATE[$i]}   volume ${PS_VOL[$i]}"
    done
  fi
}

# Aborts the uploads that were never completed, then deletes every version and delete marker in
# batches of up to 1000 (what one request takes; each batch is one page of the listing, of whichever
# kind it holds), then the bucket. The batch is written by the CLI:
# a key can hold any character, which a shell could not pass on safely. Returns 1 at the first
# thing S3 refuses (Object Lock, a missing permission), with the reason on stderr.
empty_bucket() { # BUCKET
  local b=$1 key uid n=0 out
  mkwork
  while [[ $n -lt 100000 ]]; do
    n=$((n + 1))
    "${AWS[@]}" s3api list-multipart-uploads --bucket "$b" --expected-bucket-owner "$ACCOUNT" --no-paginate \
      --query 'Uploads[0].[Key,UploadId]' --output text >"$WORK/upload" 2>"$WORK/err" || { cat "$WORK/err" >&2; return 1; }
    key="" uid=""
    IFS=$'\t' read -r key uid <"$WORK/upload" || true
    [[ -n $uid && $uid != None ]] || break
    "${AWS[@]}" s3api abort-multipart-upload --bucket "$b" --key="$key" --upload-id "$uid" --expected-bucket-owner "$ACCOUNT" >/dev/null 2>"$WORK/err" \
      || { cat "$WORK/err" >&2; return 1; }
  done
  rm -f "$WORK/previous.json"
  while :; do
    "${AWS[@]}" s3api list-object-versions --bucket "$b" --expected-bucket-owner "$ACCOUNT" --no-paginate --output json \
      --query "$Q_BATCH" >"$WORK/batch.json" 2>"$WORK/err" \
      || { cat "$WORK/err" >&2; return 1; }
    # An empty page prints "Objects": []; one with an entry always has a "VersionId".
    grep -q '"VersionId"' "$WORK/batch.json" || break
    if [[ -f $WORK/previous.json ]] && cmp -s "$WORK/batch.json" "$WORK/previous.json"; then
      echo "the same object versions came back after they were deleted: S3 is not deleting them" >&2
      return 1
    fi
    "${AWS[@]}" s3api delete-objects --bucket "$b" --expected-bucket-owner "$ACCOUNT" --delete "file://$WORK/batch.json" >"$WORK/deleted.json" 2>"$WORK/err" \
      || { cat "$WORK/err" >&2; return 1; }
    if grep -q '"Errors"' "$WORK/deleted.json"; then cat "$WORK/deleted.json" >&2; return 1; fi
    cp "$WORK/batch.json" "$WORK/previous.json"
  done
  out=$("${AWS[@]}" s3api delete-bucket --bucket "$b" --expected-bucket-owner "$ACCOUNT" 2>&1) || { printf '%s\n' "$out" >&2; return 1; }
}

# Destroys what print_purge_list showed. A failure is reported, the rest still goes, and the script
# ends with a failure so that a person runs it again after fixing the reason.
destroy_purged() {
  local i failed=0 err
  mkwork
  for ((i = 0; i < ${#PB_NAME[@]}; i++)); do
    say "Emptying ${PB_NAME[$i]} (${PB_VERS[$i]} object version(s)) ..."
    if empty_bucket "${PB_NAME[$i]}"; then
      say "  deleted the bucket ${PB_NAME[$i]}"
    else
      failed=1
      printf '%s: could not empty and delete %s\n' "$SELF" "${PB_NAME[$i]}" >&2
    fi
  done
  for ((i = 0; i < ${#PS_ID[@]}; i++)); do
    if "${AWS[@]}" ec2 delete-snapshot --snapshot-id "${PS_ID[$i]}" 2>"$WORK/err"; then
      say "Deleted ${PS_ID[$i]}"
    else
      failed=1
      err=$(cat "$WORK/err")
      printf '%s: could not delete %s: %s\n' "$SELF" "${PS_ID[$i]}" "$err" >&2
    fi
  done
  [[ $failed -eq 0 ]] || FAIL_CODE=3 fail "some of it is still there (see above): fix the reason, then run  $SELF purge --region $REGION --stack-name $STACK"
}

# `--delete --purge`: finds, before anything is stopped, what the stack will leave: its own buckets
# (named by its outputs, and refused when their CloudFormation tags name another stack) and the
# snapshots that carry its id or were taken from its data volume. A replica server's stack has no
# buckets of its own: the ones its outputs show are its leader's and are never touched.
purge_inventory_before_delete() {
  local b origin tn ti
  need_account
  STACK_ID=$(describe 'Stacks[0].StackId') || fail "cannot read the stack's id"
  for b in "$BUCKET:backup bucket" "$OBJECTS:objects bucket"; do
    [[ -n ${b%%:*} ]] || continue
    origin=$(bucket_origin "${b%%:*}")
    IFS=$'\t' read -r tn ti _ <<<"$origin"
    if [[ -z $tn$ti ]]; then
      die "the bucket ${b%%:*} shows no CloudFormation tags (it has none, or the credentials cannot read them: s3:GetBucketTagging), so it is not provably this stack's and is not purged"
    fi
    if [[ $tn != "$STACK" || $ti != "$STACK_ID" ]]; then
      die "the bucket ${b%%:*} carries the CloudFormation tags of another stack ($tn, $ti), so it is not purged"
    fi
    add_bucket "${b%%:*}" "${b#*:}"
  done
  if [[ -n $REPLICA_OF ]]; then
    say "Replica server of $REPLICA_OF: the leader's buckets are not touched."
  fi
  # shellcheck disable=SC2086
  collect_snapshots "$STACK_ID" $VOLUME
}

mode_purge() {
  local status ids id v vols="" cand prefix origin tn ti tl what b answer
  if [[ $DRY -eq 1 ]]; then
    note "dry run: nothing is sent to AWS"
    note "purge refuses while the stack exists:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text
    note "the account, for the expected-bucket-owner checks:"
    show "${AWS[@]}" sts get-caller-identity --query Account --output text
    note "the ids of the deleted stack (CloudFormation lists a deleted stack for 90 days) and the data volume each one held:"
    show "${AWS[@]}" cloudformation list-stacks --stack-status-filter DELETE_COMPLETE --query "StackSummaries[?StackName=='$STACK'].StackId" --output text
    show "${AWS[@]}" cloudformation describe-stack-resource --stack-name "<StackId>" --logical-resource-id DataVolume --query StackResourceDetail.PhysicalResourceId --output text
    note "the buckets whose CloudFormation tags name this stack (aws:cloudformation:stack-name, stack-id in this region and account, logical-id BackupBucket or ObjectsBucket):"
    show "${AWS[@]}" s3api list-buckets --query "Buckets[?starts_with(Name, '$(printf '%s' "$STACK" | tr '[:upper:]' '[:lower:]' | cut -c1-16)')].Name" --output text
    show "${AWS[@]}" s3api get-bucket-tagging --bucket "<bucket>" --expected-bucket-owner "<account>" --query 'TagSet[].[Key,Value]' --output text
    show "${AWS[@]}" s3api list-object-versions --bucket "<bucket>" --expected-bucket-owner "<account>" --query "$Q_COUNT" --output text
    note "and the other stacks of the region that use the bucket as their BackupBucketName or ObjectsBucketName (replica servers), which the list warns about:"
    show "${AWS[@]}" cloudformation describe-stacks --query "Stacks[?StackName!='$STACK' && Parameters[?(ParameterKey=='BackupBucketName' || ParameterKey=='ObjectsBucketName') && ParameterValue=='<bucket>']].StackName" --output text
    note "the snapshots tagged with the stack's id, and every other snapshot of the volumes those came from:"
    show "${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "Name=tag:supavise:stack,Values=arn:*:cloudformation:$REGION:<account>:stack/$STACK/*" --query 'Snapshots[].[SnapshotId,VolumeId,VolumeSize,StartTime,State]' --output text
    show "${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "Name=volume-id,Values=<volume>" --query 'Snapshots[].[SnapshotId,VolumeId,VolumeSize,StartTime,State]' --output text
    note "then, once you type the stack name (or with --yes), for each bucket, until a page comes back empty (versions and delete markers together):"
    show "${AWS[@]}" s3api list-object-versions --bucket "<bucket>" --expected-bucket-owner "<account>" --no-paginate --output json --query "$Q_BATCH"
    show "${AWS[@]}" s3api delete-objects --bucket "<bucket>" --expected-bucket-owner "<account>" --delete "file://<batch of up to 1000>"
    show "${AWS[@]}" s3api delete-bucket --bucket "<bucket>" --expected-bucket-owner "<account>"
    show "${AWS[@]}" ec2 delete-snapshot --snapshot-id "<snapshot>"
    return 0
  fi
  FAIL_CODE=3
  command -v aws >/dev/null 2>&1 || fail "the AWS CLI (aws) is not installed: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
  export AWS_PAGER=""
  need_account
  status=$(stack_status)
  [[ $status == none ]] || die "stack $STACK still exists in $REGION ($status), and purge is for a stack that is already deleted. To delete it and destroy what it leaves in one go: $SELF --region $REGION --stack-name $STACK --delete --purge"

  # Where the stack's own records still exist (CloudFormation keeps a deleted stack for 90 days),
  # they name its data volume.
  ids=$("${AWS[@]}" cloudformation list-stacks --stack-status-filter DELETE_COMPLETE --query "StackSummaries[?StackName=='$STACK'].StackId" --output text) \
    || fail "cannot list the deleted stacks (the credentials need cloudformation:ListStacks)"
  # shellcheck disable=SC2086
  for id in $ids; do
    [[ $id == arn:* ]] || continue
    v=$("${AWS[@]}" cloudformation describe-stack-resource --stack-name "$id" --logical-resource-id DataVolume --query StackResourceDetail.PhysicalResourceId --output text 2>/dev/null) || v=""
    if real_vol "$v"; then vols="$vols $v"; fi
  done

  # Buckets: the name only narrows the search; what proves a bucket is the tags CloudFormation put
  # on it, and a bucket of this region and account.
  prefix=$(printf '%s' "$STACK" | tr '[:upper:]' '[:lower:]' | cut -c1-16)
  cand=$("${AWS[@]}" s3api list-buckets --query "Buckets[?starts_with(Name, '$prefix')].Name" --output text) \
    || fail "cannot list the buckets (the credentials need s3:ListAllMyBuckets)"
  # shellcheck disable=SC2086
  for b in $cand; do
    origin=$(bucket_origin "$b")
    IFS=$'\t' read -r tn ti tl <<<"$origin"
    [[ $tn == "$STACK" && $ti == arn:*:cloudformation:"$REGION":"$ACCOUNT":stack/"$STACK"/* ]] || continue
    case $tl in
      BackupBucket) what="backup bucket" ;;
      ObjectsBucket) what="objects bucket" ;;
      *) continue ;;
    esac
    add_bucket "$b" "$what"
  done

  # shellcheck disable=SC2086
  collect_snapshots "arn:*:cloudformation:$REGION:$ACCOUNT:stack/$STACK/*" $vols
  if [[ ${#PB_NAME[@]} -eq 0 && ${#PS_ID[@]} -eq 0 ]]; then
    say "Nothing of stack $STACK is left in $REGION: no bucket or snapshot carries its marks."
    say "(A final snapshot of a stack that was deleted more than 90 days ago, and that never made daily snapshots, carries no mark this script can match. Find it by its volume: aws ec2 describe-snapshots --owner-ids self --region $REGION)"
    return 0
  fi
  say "Stack $STACK is deleted. What it left in $REGION, found by its marks:"
  say ""
  print_purge_list
  say ""
  say "All of it will be destroyed, and none of it can be brought back."
  if [[ $YES -eq 0 ]]; then
    [[ -t 0 ]] || die "not a terminal: pass --yes to destroy these without asking"
    printf 'Type the stack name (%s) to destroy everything above: ' "$STACK"
    read -r answer
    [[ $answer == "$STACK" ]] || die "not confirmed; nothing was destroyed"
  fi
  destroy_purged
  say ""
  say "Purged. Nothing of stack $STACK is left in $REGION."
}

case $MODE in
  update) mode_update; exit $? ;;
  purge) mode_purge; exit $? ;;
  status) mode_status; exit $? ;;
  replica) mode_replica; exit $? ;;
esac

if [[ $DRY -eq 0 ]]; then
  command -v aws >/dev/null 2>&1 || fail "the AWS CLI (aws) is not installed: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
fi

# ---- delete --------------------------------------------------------------------------------
if [[ $DELETE -eq 1 ]]; then
  cat <<WARN
About to delete the CloudFormation stack "$STACK" in $REGION.

What goes: the instance (and with it every project that is running), the Elastic IP, the
security group, the instance role, the claim-token secret and, when the stack made them, the
VPC and the DNS records.

What stays in your account, and keeps costing money until you delete it yourself:
  - the S3 backup bucket (WAL archives and base backups; the stack never deletes it)
  - the S3 objects bucket of a stack at infrastructure revision 2 or later (versioned; it holds
    Storage files once Storage runs on S3; the stack never deletes it)
  - a final EBS snapshot of the data volume (it holds the master key, the registry and every
    project; encrypted, and restorable with --data-snapshot-id)
  - the daily snapshots of the data volume that the stack made (nothing deletes old ones once
    the stack is gone)
(The stack of a replica server makes no buckets: it uses its leader's, which stay as they are.)

A running instance is stopped first, so that Postgres and the other services can shut down in
order before the final snapshot is taken, which then is not a crash image. The node is offline
from that moment. (A stack whose first launch failed has no instance; nothing is stopped.)
WARN
  if [[ $PURGE -eq 1 ]]; then
    cat <<WARN
--purge: once the stack is deleted, what stays is destroyed too: the buckets named below with every
object version and delete marker, and every snapshot of the data volume, the final one included.
Nothing of the stack is left in your account, and none of it can be brought back.

WARN
  fi
  if [[ $DRY -eq 1 ]]; then
    note "dry run: nothing is sent to AWS"
    note "the commands --delete would run:"
    note "checks that the stack exists:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text
    note "reads BackupBucket, ObjectsBucket, DataVolumeId and InstanceId from the stack outputs, to name what stays and what to stop:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$Q_OUTPUTS" --output text
    note "and JoinLeader from its parameters: the stack of a replica server shows its leader's buckets, which are not its to keep:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$Q_PARAMS" --output text
    note "stops the instance, so that the final snapshot is taken from a cleanly shut-down node"
    note "(skipped when the stack has no InstanceId output or is ROLLBACK_COMPLETE, CREATE_FAILED or DELETE_FAILED, as after a failed first launch):"
    show "${AWS[@]}" ec2 stop-instances --instance-ids "<InstanceId>"
    show "${AWS[@]}" ec2 wait instance-stopped --instance-ids "<InstanceId>"
    show "${AWS[@]}" cloudformation delete-stack --stack-name "$STACK"
    show "${AWS[@]}" cloudformation wait stack-delete-complete --stack-name "$STACK"
    show "${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "Name=volume-id,Values=<DataVolumeId>" --query 'Snapshots[].[SnapshotId,StartTime,State]' --output text
    if [[ $PURGE -eq 1 ]]; then
      note "--purge: before anything is stopped, what will be destroyed is found and listed, and you type the stack name once."
      note "the stack's id, to match the marks on its snapshots, and the account for the expected-bucket-owner checks:"
      show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackId' --output text
      show "${AWS[@]}" sts get-caller-identity --query Account --output text
      note "the backup and objects buckets of the stack (the stack of a replica server has none of its own) must carry the stack's id, and are counted:"
      show "${AWS[@]}" s3api get-bucket-tagging --bucket "<BackupBucket>" --expected-bucket-owner "<account>" --query 'TagSet[].[Key,Value]' --output text
      show "${AWS[@]}" s3api list-object-versions --bucket "<BackupBucket>" --expected-bucket-owner "<account>" --query "$Q_COUNT" --output text
      note "and the other stacks of the region that use a bucket as their BackupBucketName or ObjectsBucketName (replica servers), which the list warns about:"
      show "${AWS[@]}" cloudformation describe-stacks --query "Stacks[?StackName!='$STACK' && Parameters[?(ParameterKey=='BackupBucketName' || ParameterKey=='ObjectsBucketName') && ParameterValue=='<BackupBucket>']].StackName" --output text
      note "the snapshots tagged with the stack's id, and every other snapshot of the data volume:"
      show "${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "Name=tag:supavise:stack,Values=<StackId>" --query 'Snapshots[].[SnapshotId,VolumeId,VolumeSize,StartTime,State]' --output text
      show "${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "Name=volume-id,Values=<DataVolumeId>" --query 'Snapshots[].[SnapshotId,VolumeId,VolumeSize,StartTime,State]' --output text
      note "after the stack is deleted (the final snapshot exists by then, so the snapshots are listed again), for each bucket, until a page comes back empty (versions and delete markers together):"
      show "${AWS[@]}" s3api list-object-versions --bucket "<BackupBucket>" --expected-bucket-owner "<account>" --no-paginate --output json --query "$Q_BATCH"
      show "${AWS[@]}" s3api delete-objects --bucket "<BackupBucket>" --expected-bucket-owner "<account>" --delete "file://<batch of up to 1000>"
      show "${AWS[@]}" s3api delete-bucket --bucket "<BackupBucket>" --expected-bucket-owner "<account>"
      show "${AWS[@]}" ec2 delete-snapshot --snapshot-id "<snapshot>"
    fi
    exit 0
  fi
  status=$(stack_status)
  [[ $status != none ]] || fail "no stack named $STACK in $REGION"
  outs=$(describe "$Q_OUTPUTS")
  BUCKET=$(pick BackupBucket <<<"$outs")
  OBJECTS=$(pick ObjectsBucket <<<"$outs")
  # The stack of a replica server only shows its leader's buckets: they are not this stack's to keep or empty.
  REPLICA_OF=$(describe "$Q_PARAMS" | pick JoinLeader)
  [[ $REPLICA_OF != None ]] || REPLICA_OF=""
  [[ -z $REPLICA_OF ]] || BUCKET="" OBJECTS=""
  VOLUME=$(pick DataVolumeId <<<"$outs")
  INSTANCE=$(pick InstanceId <<<"$outs")
  # A stack whose creation failed has no outputs and no instance: there is nothing to stop, and
  # the delete is the way out (deploy cannot update a ROLLBACK_COMPLETE stack).
  STOP=1
  case $status in ROLLBACK_COMPLETE|CREATE_FAILED|DELETE_FAILED) STOP=0 ;; esac
  [[ $INSTANCE =~ ^i-[0-9a-f]{8,17}$ ]] || STOP=0
  say ""
  [[ -z $REPLICA_OF ]] || say "Replica server of $REPLICA_OF: the buckets are the leader's and stay as they are."
  [[ -z $BUCKET ]] || say "Backup bucket:  $BUCKET"
  [[ -z $OBJECTS ]] || say "Objects bucket: $OBJECTS"
  [[ -z $VOLUME ]] || say "Data volume:    $VOLUME"
  if [[ $STOP -eq 1 ]]; then
    say "Instance:       $INSTANCE (stopped first)"
  else
    say "Instance:       none to stop (stack status $status)"
  fi
  if [[ $PURGE -eq 1 ]]; then
    purge_inventory_before_delete
    say ""
    say "Destroyed once the stack is deleted, none of it to be brought back:"
    print_purge_list
    if [[ ${#PB_NAME[@]} -eq 0 && ${#PS_ID[@]} -eq 0 ]]; then
      say "  (no bucket and no snapshot of the stack exists yet)"
    fi
    if [[ -n $VOLUME ]]; then
      say "  and the final snapshot of $VOLUME that CloudFormation takes when the stack deletes it"
    fi
    say ""
  fi
  if [[ $YES -eq 0 ]]; then
    [[ -t 0 ]] || fail "not a terminal: pass --yes to delete without asking"
    if [[ $PURGE -eq 1 ]]; then
      printf 'Type the stack name (%s) to delete it and destroy everything listed above: ' "$STACK"
    else
      printf 'Type the stack name (%s) to delete it: ' "$STACK"
    fi
    read -r answer
    [[ $answer == "$STACK" ]] || fail "not deleted"
  fi
  # The final snapshot is taken when the volume is deleted. Stopping first lets the services shut
  # down and the file system flush; a snapshot of a running node is crash-consistent only.
  if [[ $STOP -eq 1 ]]; then
    say "Stopping $INSTANCE ..."
    if ! "${AWS[@]}" ec2 stop-instances --instance-ids "$INSTANCE" >/dev/null \
      || ! "${AWS[@]}" ec2 wait instance-stopped --instance-ids "$INSTANCE"; then
      fail "could not stop $INSTANCE, so the stack was not deleted (stop it in the EC2 console and run this again; if the instance no longer exists, delete the stack in the CloudFormation console)"
    fi
  fi
  "${AWS[@]}" cloudformation delete-stack --stack-name "$STACK"
  say "Deleting; this takes a few minutes ..."
  "${AWS[@]}" cloudformation wait stack-delete-complete --stack-name "$STACK" \
    || fail "the stack did not delete cleanly; see the Events tab of the stack in the CloudFormation console"
  say ""
  if [[ $PURGE -eq 1 ]]; then
    # The final snapshot exists now: list the snapshots again so that it is among them.
    # shellcheck disable=SC2086
    collect_snapshots "$STACK_ID" $VOLUME
    destroy_purged
    say ""
    say "Deleted and purged. Nothing of stack $STACK is left in $REGION."
    exit 0
  fi
  if [[ -z $BUCKET && -z $OBJECTS && -z $VOLUME ]]; then
    say "Deleted. The stack reported no backup bucket, objects bucket or data volume."
    exit 0
  fi
  say "Deleted. Kept for you:"
  if [[ -n $BUCKET ]]; then
    say "  backup bucket  $BUCKET   (empty and delete it yourself when you no longer need the backups)"
  fi
  if [[ -n $OBJECTS ]]; then
    say "  objects bucket $OBJECTS   (versioned; empty every version and delete it yourself when Storage no longer needs it)"
  fi
  if [[ -n $VOLUME ]]; then
    say "  data snapshots (the final one, and the daily ones when the stack made them); find them with:"
    say "    aws ec2 describe-snapshots --region $REGION --owner-ids self --filters Name=volume-id,Values=$VOLUME --query 'Snapshots[].[SnapshotId,StartTime,State]' --output text"
  fi
  exit 0
fi

# ---- template ------------------------------------------------------------------------------
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [[ -z $TEMPLATE ]]; then
  if [[ -f $here/supavise.yaml ]]; then
    TEMPLATE=$here/supavise.yaml
  elif [[ -f $here/../cloudformation/supavise.yaml ]]; then
    TEMPLATE=$here/../cloudformation/supavise.yaml
  else
    if [[ -z $VERSION || $VERSION == latest ]]; then
      url=$BASE_URL/latest/download/supavise.yaml
    else
      url=$BASE_URL/download/$VERSION/supavise.yaml
    fi
    if [[ $DRY -eq 1 ]]; then
      TEMPLATE=${TMPDIR:-/tmp}/supavise.yaml
      note "no template next to this script: download it from the release"
      show curl -fsSL -o "$TEMPLATE" "$url"
    else
      mkwork
      TEMPLATE=$WORK/supavise.yaml
      say "Downloading $url"
      curl -fsSL -o "$TEMPLATE" "$url" || fail "cannot download the template; pass --template FILE"
    fi
  fi
fi
[[ $DRY -eq 1 || -f $TEMPLATE ]] || fail "template not found: $TEMPLATE"

# ---- image ---------------------------------------------------------------------------------
# The image ID is always passed explicitly, so an update cannot replace the instance because
# Canonical published a newer image. A stack that exists keeps the image it runs.
if [[ -z $AMI ]]; then
  # Graviton families end their generation digit with a g (t4g, m7g, r7g, c7gn ...).
  if [[ ${ITYPE:-t4g.large} =~ ^[a-z]+[0-9]+g[a-z]*\. ]]; then ARCH=arm64; else ARCH=amd64; fi
  ssm_name=/aws/service/canonical/ubuntu/server/24.04/stable/current/$ARCH/hvm/ebs-gp3/ami-id
  if [[ $DRY -eq 1 ]]; then
    note "image: an existing stack keeps its image; a new stack gets the current Ubuntu 24.04 image"
    note "does the stack exist?"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text
    note "if it does: the image it was given (AmiId parameter) ..."
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$Q_PARAMS" --output text
    note "... and, when that is empty (a stack made in the console), the image its instance runs:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$Q_OUTPUTS" --output text
    show "${AWS[@]}" ec2 describe-instances --instance-ids "<InstanceId>" --query 'Reservations[0].Instances[0].ImageId' --output text
    note "if it does not: the current Canonical image:"
    show "${AWS[@]}" ssm get-parameter --name "$ssm_name" --query Parameter.Value --output text
    AMI="<image-id>"
  else
    status=$(stack_status)
    if [[ $status != none ]]; then
      AMI=$(describe "$Q_PARAMS" | pick AmiId)
      if [[ -z $AMI || $AMI == None ]]; then
        inst=$(describe "$Q_OUTPUTS" | pick InstanceId)
        AMI=$("${AWS[@]}" ec2 describe-instances --instance-ids "$inst" --query 'Reservations[0].Instances[0].ImageId' --output text)
      fi
      say "Stack $STACK exists ($status): keeping its image $AMI"
    else
      AMI=$("${AWS[@]}" ssm get-parameter --name "$ssm_name" --query Parameter.Value --output text) \
        || fail "cannot look up the Ubuntu image; pass --ami-id"
      say "Ubuntu 24.04 ($ARCH) image: $AMI"
    fi
    [[ $AMI =~ $re_ami ]] || fail "did not get an image ID (got: $AMI); pass --ami-id"
  fi
fi

# ---- deploy --------------------------------------------------------------------------------
params=("AdminEmail=$EMAIL" "AmiId=$AMI")
[[ -z $ITYPE ]] || params+=("InstanceType=$ITYPE")
[[ -z $VOLSIZE ]] || params+=("DataVolumeSize=$VOLSIZE")
[[ -z $SNAPS ]] || params+=("DailySnapshotsKept=$SNAPS")
[[ -z $VERSION ]] || params+=("SupaviseVersion=$VERSION")
[[ -z $DOMAIN ]] || params+=("DomainName=$DOMAIN")
[[ -z $ZONE ]] || params+=("HostedZoneId=$ZONE")
[[ -z $ACCESS ]] || params+=("AccessCidr=$ACCESS")
[[ -z $SSH ]] || params+=("SshCidr=$SSH")
[[ -z $KEY ]] || params+=("KeyName=$KEY")
[[ -z $SSM ]] || params+=("EnableSessionManager=$SSM")
[[ -z $SNAP ]] || params+=("DataSnapshotId=$SNAP")

# The API takes a template inline up to $INLINE_LIMIT bytes; above that the CLI stages it in a bucket.
DEPLOY_S3=()
if [[ -f $TEMPLATE && $(wc -c <"$TEMPLATE" | tr -d ' ') -gt $INLINE_LIMIT ]]; then
  if [[ $DRY -eq 1 ]]; then
    bucket=${TBUCKET:-$NAME-templates-<account>-$REGION}
    note "the template is over $INLINE_LIMIT bytes, so it is staged in the bucket $bucket (created when it does not exist; it stays in your account):"
    [[ -n $TBUCKET ]] || show "${AWS[@]}" s3 mb "s3://$bucket"
  else
    bucket=$(ensure_template_bucket)
  fi
  DEPLOY_S3=(--s3-bucket "$bucket" --s3-prefix _stack)
fi

if [[ $DRY -eq 1 ]]; then
  note "dry run: nothing is sent to AWS"
else
  say "Deploying stack $STACK to $REGION (about 10 minutes) ..."
fi
# CAPABILITY_IAM: the stack creates an instance role (it has no custom name).
run "${AWS[@]}" cloudformation deploy --stack-name "$STACK" --template-file "$TEMPLATE" \
  --capabilities CAPABILITY_IAM --no-fail-on-empty-changeset --tags "Application=$NAME" \
  --parameter-overrides "${params[@]}" ${DEPLOY_S3[@]+"${DEPLOY_S3[@]}"}

# ---- result --------------------------------------------------------------------------------
if [[ $DRY -eq 1 ]]; then
  note "then it reads the stack outputs (DashboardUrl, ClaimUrl, ClaimTokenCommand, DnsRecordsNeeded, ConnectCommand):"
  show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$Q_OUTPUTS" --output text
  exit 0
fi
outs=$(describe "$Q_OUTPUTS")
get() { pick "$1" <<<"$outs"; }
say ""
say "$NAME is running."
say "  Dashboard:  $(get DashboardUrl)"
say "  Claim page: $(get ClaimUrl)"
say ""
say "Get the one-time claim token (it creates the first administrator):"
say "  $(get ClaimTokenCommand)"
dns=$(get DnsRecordsNeeded)
case $dns in
  A\ *) say ""; say "Create these DNS records, then open the dashboard: $dns" ;;
esac
say ""
say "Shell on the instance (Session Manager): $(get ConnectCommand)"
say "Delete the stack later with: $SELF --region $REGION --stack-name $STACK --delete"
