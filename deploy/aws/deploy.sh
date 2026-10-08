#!/usr/bin/env bash
# One-command deploy of a Supavise node to AWS with the CloudFormation template, and the guarded
# update of a stack that exists.
#
#   deploy.sh --region us-east-1 --email you@example.com
#   deploy.sh --region us-east-1 --email you@example.com --domain example.com --hosted-zone-id Z0123456789ABCDEFGHIJ
#   deploy.sh --region us-east-1 --stack-name supavise --delete
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
       $SELF --region REGION --stack-name NAME --delete [--yes]
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
  --yes                    with --delete, update or replica: do not ask
  -h, --help

update: brings the stack of a node forward to the template of this release. It shows a change
set, refuses any that would replace or remove a resource, and never changes SupaviseVersion. It
uses your credentials, never the instance role of the node it runs on.
  --stack NAME             the stack (default: the one the node's instance tags name)
  --params-from-stack      keep the stack's parameters (this is what update always does)
  --set NAME=VALUE         set a parameter, for example Failover=on or PeerCidr1=203.0.113.4/32;
                           repeat for more (SupaviseVersion cannot be set)
  --allow-risky            allow changes that replace or remove a resource, after you type the
                           stack name (a change that would move the service address back from
                           a failover is never allowed)

status: shows the stack, its infrastructure revision, whether the service address is on its
instance and the repair rule for replacing an instance.

replica: creates a second server (a stack from the same template that joins the leader) and
opens the leader's security group to it.
  --leader-stack NAME      the leader's stack (needs infrastructure revision 2: run update first)
  --leader-region REGION   the leader's region (default: --region)
  --az ZONE                availability zone of the new server (for example eu-west-1b)
  --token-file FILE        the join token that "supavise node token" printed on the leader
  --vpc-id ID, --subnet-id ID   an existing network for the new server (both, in --region)

Exit status of update, status and replica: 0 done or nothing to do, 2 refused (arguments, a
change that is not allowed, a signature that does not verify), 3 failed.
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
on_exit() {
  local rc=$?
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
  update | status | replica | __classify | __params) MODE=$1; shift ;;
esac

REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-}}
EMAIL="" DOMAIN="" ZONE="" ITYPE="" STACK=$NAME VOLSIZE="" SNAPS="" VERSION="" ACCESS="" SSH="" KEY=""
SSM="" AMI="" SNAP="" TEMPLATE="" PROFILE="" DRY=0 DELETE=0 YES=0
STACK_GIVEN="" TBUCKET="" ALLOW_RISKY=0 SETS=() LEADER_STACK="" LEADER_REGION="" AZ="" TOKEN_FILE="" VPC="" SUBNET=""
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
    --region) need "$@"; REGION=$val ;;
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
#   classify CHANGESET.json [--failed-over]   the review of a change set; exit 0 allowed, 10 refused, 11 blocked
#   same   TEMPLATE STAGED.json           is the template CloudFormation holds the verified file? exit 0 yes, 1 no
IFS= read -r -d '' PYHELPER <<'PY' || true
import io, json, re, sys

# Resource types whose replacement does not interrupt the node: a rule and two policies. A change
# set that replaces any other type is refused.
REPLACE_OK = set([
    "AWS::EC2::SecurityGroupIngress", "AWS::IAM::Policy", "AWS::S3::BucketPolicy",
])
# Resource types whose removal is allowed: turning Failover off or clearing a PeerCidr takes a
# permission or a firewall rule away, which interrupts nothing. Any other removal is refused.
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


def judge(rc, failed_over):
    """One resource change: (verdict, words). Verdict is ok, refused or blocked."""
    typ = rc.get("ResourceType", "?")
    rid = rc.get("LogicalResourceId", "?")
    action = rc.get("Action", "?")
    repl = rc.get("Replacement", "False")
    labels, unsafe, recreate = [], [], repl in ("True", "Conditional")
    for d in rc.get("Details") or []:
        t = d.get("Target") or {}
        attr, name = t.get("Attribute", ""), t.get("Name", "")
        if t.get("RequiresRecreation", "Never") in ("Always", "Conditionally"):
            recreate = True
        label = name or attr
        if label and label not in labels:
            labels.append(label)
        if attr == "Metadata":
            continue
        if label not in SAFE.get(typ, ()):
            unsafe.append(label)
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
    if recreate:
        if typ in REPLACE_OK:
            return "ok", "replace  %s%s" % (what, detail)
        word = "would be replaced" if repl == "True" else "may be replaced"
        return "refused", "REPLACE  %s %s%s" % (what, word, detail)
    if not labels:
        return "refused", "MODIFY   %s changes in a way the change set does not describe" % what
    if unsafe:
        return "refused", "MODIFY   %s: %s changes in place, which this script does not know to be safe" % (what, ", ".join(unsafe))
    return "ok", "modify   %s%s (no interruption)" % (what, detail)


def cmd_classify(path, *flags):
    failed_over = "--failed-over" in flags
    cs = load(path)
    changes = [c["ResourceChange"] for c in cs.get("Changes", []) if c.get("Type") == "Resource" and "ResourceChange" in c]
    verdicts = {"ok": 0, "refused": 0, "blocked": 0}
    lines = []
    for rc in changes:
        v, text = judge(rc, failed_over)
        verdicts[v] += 1
        lines.append((v, text))
    order = {"blocked": 0, "refused": 1, "ok": 2}
    for v, text in sorted(lines, key=lambda x: order[x[0]]):
        print("  %s" % text)
    if not changes:
        print("  (no resource changes)")
    print("%d change(s): %d allowed, %d refused, %d blocked" % (len(changes), verdicts["ok"], verdicts["refused"], verdicts["blocked"]))
    if verdicts["blocked"]:
        sys.exit(11)
    if verdicts["refused"]:
        sys.exit(10)


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
    cmds = {"facts": cmd_facts, "params": cmd_params, "classify": cmd_classify, "same": cmd_same}
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
        || die "--delete takes only --region, --stack-name, --profile, --yes and --dry-run"
    else
      [[ $YES -eq 0 ]] || die "--yes belongs to --delete"
      validate_create_options
    fi ;;
  update)
    [[ $DELETE -eq 0 ]] || die "update does not take --delete"
    [[ -z $EMAIL$DOMAIN$ZONE$ITYPE$VOLSIZE$SNAPS$ACCESS$SSH$KEY$SSM$AMI$SNAP$LEADER_STACK$LEADER_REGION$AZ$TOKEN_FILE$VPC$SUBNET ]] \
      || die "update changes nothing but what --set names (use --set NAME=VALUE for a parameter)"
    [[ -z $VERSION || $VERSION =~ $re_version ]] || die "--version must be latest or a release tag such as v1.2.3" ;;
  status)
    [[ $DELETE -eq 0 && $ALLOW_RISKY -eq 0 && ${#SETS[@]} -eq 0 ]] || die "status takes only --stack, --region, --profile and --template"
    [[ -z $VERSION || $VERSION =~ $re_version ]] || die "--version must be latest or a release tag such as v1.2.3" ;;
  replica)
    [[ $DELETE -eq 0 ]] || die "replica does not take --delete"
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
  "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name "$CS" --output json >"$WORK/cs.json" \
    || fail "cannot read the change set"
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
# Shows the change set CS and stops (deleting it) at anything that is not allowed. Without ASK it
# also asks the person to type "apply". FAILED_OVER (0/1) comes from service_address_here.
review_change_set() { # ASK(1/0)
  local rc=0 answer args=()
  [[ $FAILED_OVER -eq 0 ]] || args=(--failed-over)
  say ""
  say "Change set $CS for stack $STACK in $REGION:"
  py classify "$WORK/cs.json" ${args[@]+"${args[@]}"} || rc=$?
  case $rc in
    0) ;;
    11) drop_change_set; die "refused: the change set could take the service address back from the server it failed over to. Nothing was changed" ;;
    10)
      if [[ $ALLOW_RISKY -eq 0 ]]; then
        drop_change_set
        die "refused: the change set replaces or removes something, or changes it in a way this script does not know to be safe. Nothing was changed. A person who has read it can pass --allow-risky"
      fi
      [[ -t 0 ]] || { drop_change_set; die "--allow-risky asks you to type the stack name, which needs a terminal"; }
      printf 'These changes can interrupt the node or lose data. Type the stack name (%s) to run them anyway: ' "$STACK"
      read -r answer
      [[ $answer == "$STACK" ]] || { drop_change_set; die "not confirmed; nothing was changed"; } ;;
    *) drop_change_set; fail "could not read the change set (the review exited $rc)" ;;
  esac
  [[ $1 -eq 1 && $YES -eq 0 ]] || return 0
  [[ -t 0 ]] || { drop_change_set; die "not a terminal: pass --yes to run the change set without asking"; }
  printf 'Type apply to run this change set: '
  read -r answer
  [[ $answer == apply ]] || { drop_change_set; die "not applied; the change set was deleted"; }
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
    0 | 10) say "WARNING: the service address moved to another server during the review; the change set does not touch it." ;;
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
  execute_change_set
  wait_stack update
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
    show "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name "$NAME-update-<time>" --output json
    note "every resource change is reviewed; a replacement or removal (but of a security group rule or an IAM policy) is refused and the change set deleted. Otherwise, after you type apply:"
    show "${AWS[@]}" cloudformation execute-change-set --stack-name "$STACK" --change-set-name "$NAME-update-<time>"
    show "${AWS[@]}" cloudformation wait stack-update-complete --stack-name "$STACK"
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

case $MODE in
  update) mode_update; exit $? ;;
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
  if [[ $YES -eq 0 ]]; then
    [[ -t 0 ]] || fail "not a terminal: pass --yes to delete without asking"
    printf 'Type the stack name (%s) to delete it: ' "$STACK"
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
