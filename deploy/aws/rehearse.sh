#!/usr/bin/env bash
# Rehearses the stack update on a throwaway stack, before the live node is touched.
#
#   deploy/aws/rehearse.sh --region us-east-1 --email you@example.com [--profile NAME]
#   deploy/aws/rehearse.sh --region us-east-1 --email you@example.com --purge --yes
#
# What it does, in order:
#   1. creates a stack from the template of v0.1.1 (deploy/cloudformation/testdata/supavise-v0.1.1.yaml,
#      the file of the tag) with SupaviseVersion=v0.1.1: a node as the live one was made
#   2. notes the instance id, its launch time and its state
#   3. updates the stack to the template of this checkout with `deploy.sh update`, turning on the
#      failover permissions and one peer rule, so that the new resources are made too
#   4. checks that the instance was not replaced or interrupted (same id, same launch time, running,
#      no create or delete event for it), that the stack is at the new infrastructure revision, and that
#      a second update finds nothing to change
#   5. prints what to check on the node itself (the instance tags in the metadata service, the dry-run
#      calls of the failover permissions) and waits for you
#   6. deletes the stack (the instance is stopped first, as `deploy.sh --delete` does)
#
# It creates real resources and costs money: a t4g.medium and a 20 GiB volume for the time it runs
# (about 30 minutes), then storage in the two buckets and the snapshots until --purge removes them. Run it
# with credentials of an account you may spend in. Exit status: 0 every check passed, 1 a check failed
# (the stack is still deleted unless --keep), 2 bad arguments or a refused step.
#
# --keep leaves the stack; --purge empties and deletes the two buckets and the snapshots the rehearsal
# made (otherwise it prints what remains and the commands); --yes skips the prompt before the deletion;
# --dry-run prints the commands and runs none. SUPAVISE_REHEARSE_DEPLOY names another deploy.sh.
set -euo pipefail

SELF=$(basename "$0")
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=${SUPAVISE_REHEARSE_DEPLOY:-$here/deploy.sh}
OLD_TEMPLATE=$here/../cloudformation/testdata/supavise-v0.1.1.yaml
NEW_TEMPLATE=$here/../cloudformation/supavise.yaml

die() { printf '%s: %s\n' "$SELF" "$*" >&2; exit 2; }
say() { printf '%s\n' "$*"; }
q() {
  case $1 in
    '' | *[!A-Za-z0-9_./:=@%+,-]*) printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
    *) printf '%s' "$1" ;;
  esac
}
show() { local a out=""; for a in "$@"; do out="$out $(q "$a")"; done; printf '%s\n' "${out# }"; }

REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-}} EMAIL="" PROFILE="" KEEP=0 PURGE=0 YES=0 DRY=0
while [[ $# -gt 0 ]]; do
  case $1 in
    --region) [[ $# -ge 2 ]] || die "--region needs a value"; REGION=$2; shift 2 ;;
    --email) [[ $# -ge 2 ]] || die "--email needs a value"; EMAIL=$2; shift 2 ;;
    --profile) [[ $# -ge 2 ]] || die "--profile needs a value"; PROFILE=$2; shift 2 ;;
    --keep) KEEP=1; shift ;;
    --purge) PURGE=1; shift ;;
    --yes | -y) YES=1; shift ;;
    --dry-run) DRY=1; shift ;;
    -h | --help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done
[[ -n $REGION ]] || die "--region is required (or set AWS_REGION)"
[[ $REGION =~ ^[a-z]{2}(-[a-z]+)+-[0-9]+$ ]] || die "--region $REGION is not a region name such as us-east-1"
[[ -n $EMAIL ]] || die "--email is required"
[[ $EMAIL =~ ^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$ ]] || die "--email $EMAIL is not an email address"
[[ $KEEP -eq 0 || $PURGE -eq 0 ]] || die "--keep and --purge contradict each other"
[[ -f $OLD_TEMPLATE ]] || die "the v0.1.1 template is not at $OLD_TEMPLATE: run this from a checkout"
[[ -f $NEW_TEMPLATE ]] || die "the template of this checkout is not at $NEW_TEMPLATE"

STACK=supavise-rehearsal-$(date -u +%m%d%H%M%S)
AWS=(aws --region "$REGION")
[[ -z $PROFILE ]] || AWS+=(--profile "$PROFILE")
DEPLOY_ARGS=(--region "$REGION")
[[ -z $PROFILE ]] || DEPLOY_ARGS+=(--profile "$PROFILE")

if [[ $DRY -eq 1 ]]; then
  say "# dry run: nothing is sent to AWS. The rehearsal stack would be $STACK in $REGION."
  say "# 1. a stack from the v0.1.1 template, installing v0.1.1:"
  show "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --email "$EMAIL" --version v0.1.1 --template "$OLD_TEMPLATE" --instance-type t4g.medium --volume-size 20 --daily-snapshots 0
  say "# 2. the instance, before:"
  show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "Stacks[0].Outputs[?OutputKey=='InstanceId'].OutputValue" --output text
  show "${AWS[@]}" ec2 describe-instances --instance-ids "<InstanceId>" --query 'Reservations[0].Instances[0].[LaunchTime,State.Name]' --output text
  say "# 3. the update to this checkout's template, with the failover permissions and a peer rule on:"
  show "$DEPLOY" update "${DEPLOY_ARGS[@]}" --stack "$STACK" --template "$NEW_TEMPLATE" --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes
  say "# 4. the checks: same instance, same launch time, running; no create or delete event for the Instance; revision 2; a second update changes nothing:"
  show "${AWS[@]}" ec2 describe-instances --instance-ids "<InstanceId>" --query 'Reservations[0].Instances[0].[LaunchTime,State.Name]' --output text
  show "${AWS[@]}" cloudformation describe-stack-events --stack-name "$STACK" --query "StackEvents[?LogicalResourceId=='Instance'].[Timestamp,ResourceStatus]" --output text
  show "$DEPLOY" update "${DEPLOY_ARGS[@]}" --stack "$STACK" --template "$NEW_TEMPLATE" --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes
  say "# 5. the checklist for the node, then you press Enter"
  say "# 6. the deletion:"
  show "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --delete --yes
  [[ $PURGE -eq 0 ]] || say "# and with --purge: both buckets are emptied (every version) and deleted, and the snapshots of the data volume are deleted"
  exit 0
fi

command -v aws >/dev/null 2>&1 || die "the AWS CLI (aws) is not installed"
command -v python3 >/dev/null 2>&1 || die "python3 is needed (it is in CloudShell, macOS and Ubuntu)"
WORK=$(mktemp -d)
FAILED=0
report() { # DESCRIPTION STATUS: one check; a status other than 0 is a failure
  if [[ $2 -eq 0 ]]; then say "PASS  $1"; else say "FAIL  $1"; FAILED=1; fi
}
out_of() { # KEY: an output of the rehearsal stack
  "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text
}
instance_facts() { # prints "LaunchTime State" of the instance
  "${AWS[@]}" ec2 describe-instances --instance-ids "$1" --query 'Reservations[0].Instances[0].[LaunchTime,State.Name]' --output text | tr '\t' ' '
}

say "== 1. creating $STACK from the v0.1.1 template in $REGION (about 10 minutes)"
"$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --email "$EMAIL" --version v0.1.1 --template "$OLD_TEMPLATE" \
  --instance-type t4g.medium --volume-size 20 --daily-snapshots 0 || die "the v0.1.1 stack could not be created; look in the CloudFormation console, delete the stack $STACK if it exists"
INSTANCE=$(out_of InstanceId); BACKUP=$(out_of BackupBucket); VOLUME=$(out_of DataVolumeId)
[[ -n $INSTANCE && $INSTANCE != None ]] || die "the stack has no InstanceId output"
BEFORE=$(instance_facts "$INSTANCE")
say "== 2. the instance before: $INSTANCE $BEFORE"

# shellcheck disable=SC2329  # run by the EXIT trap
cleanup() {
  local rc=$?
  trap - EXIT
  if [[ $KEEP -eq 1 ]]; then
    say "== the stack $STACK is kept; delete it with:  $DEPLOY ${DEPLOY_ARGS[*]} --stack-name $STACK --delete --yes"
    rm -rf "$WORK"
    exit "$rc"
  fi
  if [[ $YES -eq 0 && -t 0 ]]; then
    printf 'Press Enter to delete the rehearsal stack %s (Ctrl-C keeps it): ' "$STACK"
    read -r _ || true
  fi
  say "== 6. deleting $STACK"
  "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --delete --yes || say "WARNING: the stack was not deleted: $DEPLOY ${DEPLOY_ARGS[*]} --stack-name $STACK --delete --yes"
  leftovers
  rm -rf "$WORK"
  exit "$rc"
}

# shellcheck disable=SC2329  # run by cleanup
purge_bucket() { # BUCKET: every object version and delete marker, then the bucket
  local out n
  while :; do
    out=$("${AWS[@]}" s3api list-object-versions --bucket "$1" --max-items 500 --output json) || return 1
    n=$(printf '%s' "$out" | python3 -c '
import json, sys
d = json.load(sys.stdin)
items = [{"Key": v["Key"], "VersionId": v["VersionId"]} for k in ("Versions", "DeleteMarkers") for v in (d.get(k) or [])]
json.dump({"Objects": items, "Quiet": True}, open(sys.argv[1], "w"))
print(len(items))' "$WORK/delete.json")
    [[ $n -gt 0 ]] || break
    "${AWS[@]}" s3api delete-objects --bucket "$1" --delete "file://$WORK/delete.json" >/dev/null
  done
  "${AWS[@]}" s3api delete-bucket --bucket "$1"
}
# shellcheck disable=SC2329  # run by cleanup
leftovers() {
  local objects="" snaps s
  objects=$(printf '%s\n' "$OBJECTS" | sed '/^$/d')
  if [[ $PURGE -eq 1 ]]; then
    for b in $BACKUP $objects; do
      [[ -n $b && $b != None ]] || continue
      say "purging bucket $b"; purge_bucket "$b" || say "WARNING: bucket $b was not purged"
    done
    if [[ -n $VOLUME && $VOLUME != None ]]; then
      snaps=$("${AWS[@]}" ec2 describe-snapshots --owner-ids self --filters "Name=volume-id,Values=$VOLUME" --query 'Snapshots[].SnapshotId' --output text || true)
      for s in $snaps; do say "deleting snapshot $s"; "${AWS[@]}" ec2 delete-snapshot --snapshot-id "$s" || true; done
    fi
    return
  fi
  say "Left in your account, and billed until you delete them:"
  say "  buckets  $BACKUP $objects  (empty every version, then:  aws s3 rb s3://BUCKET)  or run this again with --purge"
  say "  snapshots of volume $VOLUME  (aws ec2 describe-snapshots --owner-ids self --filters Name=volume-id,Values=$VOLUME)"
}

OBJECTS="" ALLOCATION=""
trap cleanup EXIT

START=$(date -u +%Y-%m-%dT%H:%M:%S)
say "== 3. updating to the template of this checkout (failover permissions and one peer rule on)"
UPDATE_LOG=$WORK/update.log
set +e
"$DEPLOY" update "${DEPLOY_ARGS[@]}" --stack "$STACK" --template "$NEW_TEMPLATE" --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes 2>&1 | tee "$UPDATE_LOG"
UPDATE_RC=${PIPESTATUS[0]}
set -e

say "== 4. checks"
report "the update ran (exit status 0)" "$UPDATE_RC"
rc=0; ! grep -q 'REPLACE  Instance ' "$UPDATE_LOG" || rc=1
report "the change set holds no replacement of the Instance" "$rc"
rc=0; ! grep -q 'REMOVE ' "$UPDATE_LOG" || rc=1
report "the change set holds no removal" "$rc"
rc=0; [[ $(out_of InstanceId) == "$INSTANCE" ]] || rc=1
report "the instance is the same" "$rc"
AFTER=$(instance_facts "$INSTANCE" || true)
rc=0; [[ $AFTER == "$BEFORE" ]] || rc=1
report "the launch time is the same and the instance is running ($BEFORE -> $AFTER)" "$rc"
EVENTS=$("${AWS[@]}" cloudformation describe-stack-events --stack-name "$STACK" --query "StackEvents[?LogicalResourceId=='Instance'].[Timestamp,ResourceStatus]" --output text || true)
NEWEVENTS=$(printf '%s\n' "$EVENTS" | awk -v s="$START" '$1 >= s { print $2 }')
rc=0; ! printf '%s\n' "$NEWEVENTS" | grep -Eq '^(CREATE|DELETE)_' || rc=1
report "no create or delete event for the Instance since the update began (saw: $(printf '%s' "$NEWEVENTS" | tr '\n' ' '))" "$rc"
rc=0; [[ $(out_of InfraRevision) == 2 ]] || rc=1
report "the stack is at infrastructure revision 2" "$rc"
OBJECTS=$(out_of ObjectsBucket || true)
[[ $OBJECTS != None ]] || OBJECTS=""
ALLOCATION=$(out_of ElasticIpAllocationId || true)
[[ $ALLOCATION != None ]] || ALLOCATION=""
SECOND=$WORK/second.log
set +e
"$DEPLOY" update "${DEPLOY_ARGS[@]}" --stack "$STACK" --template "$NEW_TEMPLATE" --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes >"$SECOND" 2>&1
SECOND_RC=$?
set -e
rc=0; [[ $SECOND_RC -eq 0 ]] && grep -q 'Nothing to change' "$SECOND" || rc=1
report "a second update finds nothing to change" "$rc"

cat <<LIST

== 5. on the node (aws ssm start-session --region $REGION --target $INSTANCE), by hand:
  T=\$(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
  for k in supavise:infra supavise:caps supavise:cluster supavise:stack-name supavise:eip supavise:storage-role; do
    echo "\$k=\$(curl -s -H "X-aws-ec2-metadata-token: \$T" http://169.254.169.254/latest/meta-data/tags/instance/\$k)"; done
      expect supavise:infra=2 and supavise:caps=tags,storage-role,fencing,peer-rule, with no restart of the instance
  The failover permissions, with the instance role (--dry-run asks whether the call would be allowed):
    aws ec2 stop-instances --dry-run --instance-ids $INSTANCE --region $REGION
      expect DryRunOperation (the instance carries supavise:cluster)
    aws ec2 associate-address --dry-run --allocation-id ${ALLOCATION:-ALLOCATION_ID} --instance-id $INSTANCE --allow-reassociation --region $REGION
      expect DryRunOperation; note whether a call that names a private address (--private-ip-address) also passes
  Then run  $DEPLOY replica --leader-stack $STACK ...  against this stack to see the second server join (see deploy/aws/README.md).
LIST
if [[ $YES -eq 0 && -t 0 ]]; then
  printf 'Check the node, then press Enter to go on to the deletion: '
  read -r _ || true
fi
if [[ $FAILED -eq 0 ]]; then say "== every check passed"; else say "== at least one check FAILED"; fi
exit "$FAILED"
