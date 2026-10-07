#!/usr/bin/env bash
# One-command deploy of an sbctl node to AWS with the CloudFormation template.
#
#   deploy.sh --region us-east-1 --email you@example.com
#   deploy.sh --region us-east-1 --email you@example.com --domain example.com --hosted-zone-id Z0123456789ABCDEFGHIJ
#   deploy.sh --region us-east-1 --stack-name sbctl --delete
#
# Needs the AWS CLI (v2) with credentials that may create CloudFormation, EC2, IAM, S3, Route 53
# and Secrets Manager resources. --dry-run prints every aws command it would run and runs none.
# Runs on bash 3.2 (macOS) and later.
#
# Where the template comes from: --template FILE, else sbctl.yaml next to this script (a release
# download), else ../cloudformation/sbctl.yaml (a checkout), else the release asset of the version.
set -euo pipefail

NAME=sbctl
REPO=jsmillerdev/sbctl
SELF=$(basename "$0")

usage() {
  cat <<USAGE
Usage: $SELF --region REGION --email ADDRESS [options]
       $SELF --region REGION --stack-name NAME --delete [--yes]

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
  --profile NAME           AWS CLI profile
  --dry-run                print the aws commands, run nothing
  --delete                 stop the instance, then delete the stack (the backup bucket, a final
                           snapshot of the data volume and any daily snapshots stay in your
                           account); asks for confirmation
  --yes                    with --delete: do not ask
  -h, --help
USAGE
}

die() { printf '%s: %s\n' "$SELF" "$*" >&2; exit 2; }
fail() { printf '%s: %s\n' "$SELF" "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

# Quote an argument for display only when the shell would need it.
q() {
  case $1 in
    '' | *[!A-Za-z0-9_./:=@%+,-]*) printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
    *) printf '%s' "$1" ;;
  esac
}
show() { local a out=""; for a in "$@"; do out="$out $(q "$a")"; done; printf '%s\n' "${out# }"; }

# ---- arguments -----------------------------------------------------------------------------
REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-}}
EMAIL="" DOMAIN="" ZONE="" ITYPE="" STACK=$NAME VOLSIZE="" SNAPS="" VERSION="" ACCESS="" SSH="" KEY=""
SSM="" AMI="" SNAP="" TEMPLATE="" PROFILE="" DRY=0 DELETE=0 YES=0

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
    --stack-name) need "$@"; STACK=$val ;;
    --volume-size) need "$@"; VOLSIZE=$val ;;
    --daily-snapshots) need "$@"; SNAPS=$val ;;
    --version) need "$@"; VERSION=$val ;;
    --access-cidr) need "$@"; ACCESS=$val ;;
    --ssh-cidr) need "$@"; SSH=$val ;;
    --key-name) need "$@"; KEY=$val ;;
    --ami-id) need "$@"; AMI=$val ;;
    --data-snapshot-id) need "$@"; SNAP=$val ;;
    --template) need "$@"; TEMPLATE=$val ;;
    --profile) need "$@"; PROFILE=$val ;;
    --no-session-manager) SSM=false ;;
    --dry-run) DRY=1 ;;
    --delete) DELETE=1 ;;
    --yes | -y) YES=1 ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
  shift
  if [[ $shift_extra -eq 1 ]]; then shift; fi
done

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

[[ -n $REGION ]] || die "--region is required (or set AWS_REGION)"
[[ $REGION =~ $re_region ]] || die "--region $REGION is not a region name such as us-east-1"
[[ $STACK =~ $re_stack ]] || die "--stack-name must start with a letter and hold only letters, digits and hyphens"
if [[ $DELETE -eq 1 ]]; then
  [[ -z $EMAIL$DOMAIN$ZONE$ITYPE$VOLSIZE$SNAPS$VERSION$ACCESS$SSH$KEY$SSM$AMI$SNAP$TEMPLATE ]] \
    || die "--delete takes only --region, --stack-name, --profile, --yes and --dry-run"
else
  [[ $YES -eq 0 ]] || die "--yes belongs to --delete"
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
fi

AWS=(aws --region "$REGION")
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
  - a final EBS snapshot of the data volume (it holds the master key, the registry and every
    project; encrypted, and restorable with --data-snapshot-id)
  - the daily snapshots of the data volume that the stack made (nothing deletes old ones once
    the stack is gone)

A running instance is stopped first, so that Postgres and the other services can shut down in
order before the final snapshot is taken, which then is not a crash image. The node is offline
from that moment. (A stack whose first launch failed has no instance; nothing is stopped.)
WARN
  if [[ $DRY -eq 1 ]]; then
    note "dry run: nothing is sent to AWS"
    note "the commands --delete would run:"
    note "checks that the stack exists:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text
    note "reads BackupBucket, DataVolumeId and InstanceId from the stack outputs, to name what stays and what to stop:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "$Q_OUTPUTS" --output text
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
  VOLUME=$(pick DataVolumeId <<<"$outs")
  INSTANCE=$(pick InstanceId <<<"$outs")
  # A stack whose creation failed has no outputs and no instance: there is nothing to stop, and
  # the delete is the way out (deploy cannot update a ROLLBACK_COMPLETE stack).
  STOP=1
  case $status in ROLLBACK_COMPLETE|CREATE_FAILED|DELETE_FAILED) STOP=0 ;; esac
  [[ $INSTANCE =~ ^i-[0-9a-f]{8,17}$ ]] || STOP=0
  say ""
  [[ -z $BUCKET ]] || say "Backup bucket:  $BUCKET"
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
  if [[ -z $BUCKET && -z $VOLUME ]]; then
    say "Deleted. The stack reported no backup bucket or data volume."
    exit 0
  fi
  say "Deleted. Kept for you:"
  if [[ -n $BUCKET ]]; then
    say "  backup bucket  $BUCKET   (empty and delete it yourself when you no longer need the backups)"
  fi
  if [[ -n $VOLUME ]]; then
    say "  data snapshots (the final one, and the daily ones when the stack made them); find them with:"
    say "    aws ec2 describe-snapshots --region $REGION --owner-ids self --filters Name=volume-id,Values=$VOLUME --query 'Snapshots[].[SnapshotId,StartTime,State]' --output text"
  fi
  exit 0
fi

# ---- template ------------------------------------------------------------------------------
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CLEANUP=""
trap '[[ -z $CLEANUP ]] || rm -rf "$CLEANUP"' EXIT
if [[ -z $TEMPLATE ]]; then
  if [[ -f $here/sbctl.yaml ]]; then
    TEMPLATE=$here/sbctl.yaml
  elif [[ -f $here/../cloudformation/sbctl.yaml ]]; then
    TEMPLATE=$here/../cloudformation/sbctl.yaml
  else
    if [[ -z $VERSION || $VERSION == latest ]]; then
      url=https://github.com/$REPO/releases/latest/download/sbctl.yaml
    else
      url=https://github.com/$REPO/releases/download/$VERSION/sbctl.yaml
    fi
    if [[ $DRY -eq 1 ]]; then
      TEMPLATE=${TMPDIR:-/tmp}/sbctl.yaml
      note "no template next to this script: download it from the release"
      show curl -fsSL -o "$TEMPLATE" "$url"
    else
      CLEANUP=$(mktemp -d)
      TEMPLATE=$CLEANUP/sbctl.yaml
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
[[ -z $VERSION ]] || params+=("SbctlVersion=$VERSION")
[[ -z $DOMAIN ]] || params+=("DomainName=$DOMAIN")
[[ -z $ZONE ]] || params+=("HostedZoneId=$ZONE")
[[ -z $ACCESS ]] || params+=("AccessCidr=$ACCESS")
[[ -z $SSH ]] || params+=("SshCidr=$SSH")
[[ -z $KEY ]] || params+=("KeyName=$KEY")
[[ -z $SSM ]] || params+=("EnableSessionManager=$SSM")
[[ -z $SNAP ]] || params+=("DataSnapshotId=$SNAP")

if [[ $DRY -eq 1 ]]; then
  note "dry run: nothing is sent to AWS"
else
  say "Deploying stack $STACK to $REGION (about 10 minutes) ..."
fi
# CAPABILITY_IAM: the stack creates an instance role (it has no custom name).
run "${AWS[@]}" cloudformation deploy --stack-name "$STACK" --template-file "$TEMPLATE" \
  --capabilities CAPABILITY_IAM --no-fail-on-empty-changeset --tags "Application=$NAME" \
  --parameter-overrides "${params[@]}"

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
