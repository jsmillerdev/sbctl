#!/usr/bin/env bash
# Rehearses the stack update on a throwaway stack, before the live node is touched.
#
#   deploy/aws/rehearse.sh --region us-east-1 --email you@example.com [--profile NAME]
#   deploy/aws/rehearse.sh --region us-east-1 --email you@example.com --purge --yes
#   deploy/aws/rehearse.sh --region us-east-1 --email you@example.com --console-stack [--force-denial]
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
#   5. prints what to check on the node itself and waits for you: the instance tags in the metadata
#      service, the dry-run calls of the failover permissions, the boot time (no reboot) and whether
#      the service address stays where it was put after an update of the instance's tags
#   6. deletes the stack (the instance is stopped first, as `deploy.sh --delete` does)
#
# It creates real resources and costs money: a t4g.medium and a 20 GiB volume for the time it runs
# (about 30 minutes), then storage in the two buckets and the snapshots until --purge removes them. Run it
# with credentials of an account you may spend in. Exit status: 0 every check passed, 1 a check failed
# (the stack is still deleted unless --keep), 2 bad arguments or a refused step.
#
# --console-stack makes the stack of step 1 the way the CloudFormation console and the Launch Stack
# button do (create-stack with AmiId left empty, so that the image comes from the template's
# {{resolve:ssm:...}} reference) instead of with deploy.sh, which always passes an image ID. The update
# then pins AmiId to the image the instance runs, CloudFormation cannot tell before the change set
# runs whether that is a new image, and deploy.sh runs the change set under its guard (a temporary
# stack policy). Step 4 also checks that the stack's AmiId is the instance's image afterwards and that
# the stack policy allows every update again (the stack had none), and shows how the review called
# the instance. Before step 3 it makes the change set of that update itself, saves it as AWS gives it
# (describe-change-set --include-property-values) to rehearsal-changeset-<stack>.json in the current
# directory, and deletes it: that file is what the review's test fixture of a console-made stack
# (testdata/changeset-console-stack.json) is modelled on, and can replace it.
#
# --force-denial (with --console-stack) also rehearses the path the guard exists for, before step 3:
# it sets the guard by hand (deploy.sh builds it), runs an update to this checkout's template with
# another Ubuntu image (--denial-ami AMI, or an older build of the same release), which adds the new
# resources and needs a replacement of the instance, and checks that the update fails, that the stack
# rolls back completely (UPDATE_ROLLBACK_COMPLETE, with the guard still on while it does), that an event
# names the stack policy, and that the instance is the same. It prints the failure events, whose
# wording the script's message relies on. Then the stack's policy is put back and the rehearsal goes
# on. If the rollback stops (UPDATE_ROLLBACK_FAILED), the policy is put back and the rollback resumed.
#
# --keep leaves the stack; --purge also destroys the two buckets and the snapshots the rehearsal
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

REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-}} EMAIL="" PROFILE="" KEEP=0 PURGE=0 YES=0 DRY=0 CONSOLE=0 DENIAL=0 DENIAL_AMI=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --region) [[ $# -ge 2 ]] || die "--region needs a value"; REGION=$2; shift 2 ;;
    --email) [[ $# -ge 2 ]] || die "--email needs a value"; EMAIL=$2; shift 2 ;;
    --profile) [[ $# -ge 2 ]] || die "--profile needs a value"; PROFILE=$2; shift 2 ;;
    --keep) KEEP=1; shift ;;
    --console-stack) CONSOLE=1; shift ;;
    --force-denial) DENIAL=1; shift ;;
    --denial-ami) [[ $# -ge 2 ]] || die "--denial-ami needs a value"; DENIAL_AMI=$2; shift 2 ;;
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
[[ $DENIAL -eq 0 || $CONSOLE -eq 1 ]] || die "--force-denial needs --console-stack"
[[ -z $DENIAL_AMI || $DENIAL_AMI =~ ^ami-[0-9a-f]{8,17}$ ]] || die "--denial-ami $DENIAL_AMI is not an image ID"
[[ -f $OLD_TEMPLATE ]] || die "the v0.1.1 template is not at $OLD_TEMPLATE: run this from a checkout"
[[ -f $NEW_TEMPLATE ]] || die "the template of this checkout is not at $NEW_TEMPLATE"

# The rehearsal stack is not this host's. On an EC2 host (a jump host, say) deploy.sh would read the
# host's instance id from the metadata service and refuse a stack that is not its own.
export SUPAVISE_IMDS_ENDPOINT=http://127.0.0.1:1

STACK=supavise-rehearsal-$(date -u +%m%d%H%M%S)
AWS=(aws --region "$REGION")
[[ -z $PROFILE ]] || AWS+=(--profile "$PROFILE")
PURGE_ARG=()
[[ $PURGE -eq 0 ]] || PURGE_ARG=(--purge)
DEPLOY_ARGS=(--region "$REGION")
[[ -z $PROFILE ]] || DEPLOY_ARGS+=(--profile "$PROFILE")

# The parameters of the stack of step 1 when the console makes it: AmiId is not given, so it is empty.
console_params() {
  python3 -c 'import json, sys; print(json.dumps([{"ParameterKey": k, "ParameterValue": v} for k, v in zip(sys.argv[1::2], sys.argv[2::2])]))' \
    AdminEmail "$EMAIL" SupaviseVersion v0.1.1 InstanceType t4g.medium DataVolumeSize 20 DailySnapshotsKept 0
}

if [[ $DRY -eq 1 ]]; then
  say "# dry run: nothing is sent to AWS. The rehearsal stack would be $STACK in $REGION."
  if [[ $CONSOLE -eq 1 ]]; then
    say "# 1. a stack from the v0.1.1 template, installing v0.1.1, made the console's way: AmiId is left empty"
    say "#    (the parameters: AdminEmail, SupaviseVersion=v0.1.1, InstanceType=t4g.medium, DataVolumeSize=20, DailySnapshotsKept=0):"
    show "${AWS[@]}" cloudformation create-stack --stack-name "$STACK" --template-body "file://$OLD_TEMPLATE" --capabilities CAPABILITY_IAM --tags Key=Application,Value=supavise --parameters "file://<parameters without AmiId>"
    show "${AWS[@]}" cloudformation wait stack-create-complete --stack-name "$STACK"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "Stacks[0].Parameters[?ParameterKey=='AmiId'].ParameterValue" --output text
  else
    say "# 1. a stack from the v0.1.1 template, installing v0.1.1:"
    show "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --email "$EMAIL" --version v0.1.1 --template "$OLD_TEMPLATE" --instance-type t4g.medium --volume-size 20 --daily-snapshots 0
  fi
  say "# 2. the instance, before:"
  show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "Stacks[0].Outputs[?OutputKey=='InstanceId'].OutputValue" --output text
  show "${AWS[@]}" ec2 describe-instances --instance-ids "<InstanceId>" --query 'Reservations[0].Instances[0].[LaunchTime,State.Name]' --output text
  if [[ $CONSOLE -eq 1 ]]; then
    say "# the change set of the update, as AWS gives it, saved to rehearsal-changeset-$STACK.json, then deleted (the template goes through the backup bucket):"
    show "$DEPLOY" __params "<the stack.json>" "$NEW_TEMPLATE" "AmiId=<the instance's image>" Failover=on PeerCidr1=198.51.100.0/24
    show "${AWS[@]}" s3 cp "$NEW_TEMPLATE" "s3://<BackupBucket>/_rehearsal/supavise.yaml"
    show "${AWS[@]}" cloudformation create-change-set --stack-name "$STACK" --change-set-name rehearsal-capture --change-set-type UPDATE --capabilities CAPABILITY_IAM --template-url "<the staged template>" --parameters "file://<params.json>"
    show "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name rehearsal-capture --include-property-values --output json
    show "${AWS[@]}" cloudformation delete-change-set --stack-name "$STACK" --change-set-name rehearsal-capture
  fi
  if [[ $DENIAL -eq 1 ]]; then
    say "# the denial: the guard set by hand, then an update to this template with another image, which must fail and roll back:"
    show "$DEPLOY" __guard "<the stack's own policy>" "$NEW_TEMPLATE" "<no change set>" "<the policy to put back>"
    show "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://<the guard>"
    show "${AWS[@]}" cloudformation update-stack --stack-name "$STACK" --template-url "<the staged template>" --capabilities CAPABILITY_IAM --parameters "file://<params with AmiId=${DENIAL_AMI:-<an older image>}>"
    show "${AWS[@]}" cloudformation wait stack-update-complete --stack-name "$STACK"
    show "${AWS[@]}" cloudformation describe-stack-events --stack-name "$STACK" --query "StackEvents[?ResourceStatus=='UPDATE_FAILED'].[LogicalResourceId,ResourceStatusReason]" --output text
    show "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://<the policy to put back>"
  fi
  say "# 3. the update to this checkout's template, with the failover permissions and a peer rule on:"
  show "$DEPLOY" update "${DEPLOY_ARGS[@]}" --stack "$STACK" --template "$NEW_TEMPLATE" --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes
  say "# 4. the checks: same instance, same launch time, running; no create or delete event for the Instance; revision 2; a second update changes nothing:"
  show "${AWS[@]}" ec2 describe-instances --instance-ids "<InstanceId>" --query 'Reservations[0].Instances[0].[LaunchTime,State.Name]' --output text
  show "${AWS[@]}" cloudformation describe-stack-events --stack-name "$STACK" --query "StackEvents[?LogicalResourceId=='Instance'].[Timestamp,ResourceStatus]" --output text
  show "$DEPLOY" update "${DEPLOY_ARGS[@]}" --stack "$STACK" --template "$NEW_TEMPLATE" --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes
  if [[ $CONSOLE -eq 1 ]]; then
    say "# and, for the console's stack: AmiId is now the image the instance runs, and the stack policy allows every update again:"
    show "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "Stacks[0].Parameters[?ParameterKey=='AmiId'].ParameterValue" --output text
    show "${AWS[@]}" ec2 describe-instances --instance-ids "<InstanceId>" --query 'Reservations[0].Instances[0].ImageId' --output text
    show "${AWS[@]}" cloudformation get-stack-policy --stack-name "$STACK" --query StackPolicyBody --output text
  fi
  say "# 5. the checklist for the node, then you press Enter"
  say "# 6. the deletion:"
  show "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --delete --yes ${PURGE_ARG[@]+"${PURGE_ARG[@]}"}
  [[ $PURGE -eq 0 ]] || say "# with --purge: both buckets are emptied (every version) and deleted, and the snapshots of the data volume are deleted, by deploy.sh --delete --purge"
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

ami_param() { # the stack's AmiId parameter
  "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query "Stacks[0].Parameters[?ParameterKey=='AmiId'].ParameterValue" --output text
}
if [[ $CONSOLE -eq 1 ]]; then
  say "== 1. creating $STACK from the v0.1.1 template in $REGION the console's way, AmiId empty (about 10 minutes)"
  console_params >"$WORK/params.json"
  { "${AWS[@]}" cloudformation create-stack --stack-name "$STACK" --template-body "file://$OLD_TEMPLATE" --capabilities CAPABILITY_IAM \
      --tags Key=Application,Value=supavise --parameters "file://$WORK/params.json" >/dev/null \
    && "${AWS[@]}" cloudformation wait stack-create-complete --stack-name "$STACK"; } \
    || die "the v0.1.1 stack could not be created; look in the CloudFormation console, delete the stack $STACK if it exists"
else
  say "== 1. creating $STACK from the v0.1.1 template in $REGION (about 10 minutes)"
  "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --email "$EMAIL" --version v0.1.1 --template "$OLD_TEMPLATE" \
    --instance-type t4g.medium --volume-size 20 --daily-snapshots 0 || die "the v0.1.1 stack could not be created; look in the CloudFormation console, delete the stack $STACK if it exists"
fi
INSTANCE=$(out_of InstanceId); BACKUP=$(out_of BackupBucket); VOLUME=$(out_of DataVolumeId)
[[ -n $INSTANCE && $INSTANCE != None ]] || die "the stack has no InstanceId output"
BEFORE=$(instance_facts "$INSTANCE")
say "== 2. the instance before: $INSTANCE $BEFORE"
AMI_BEFORE=""
if [[ $CONSOLE -eq 1 ]]; then AMI_BEFORE=$(ami_param || true); fi

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
  # With --purge the deletion is deploy.sh's own one-command cleanup (--delete --purge), which is
  # thereby tried against a real account: the stack, both buckets with every object version, and
  # every snapshot of the data volume.
  "$DEPLOY" "${DEPLOY_ARGS[@]}" --stack-name "$STACK" --delete --yes ${PURGE_ARG[@]+"${PURGE_ARG[@]}"} \
    || say "WARNING: the stack was not deleted: $DEPLOY ${DEPLOY_ARGS[*]} --stack-name $STACK --delete --yes ${PURGE_ARG[*]}"
  leftovers
  rm -rf "$WORK"
  exit "$rc"
}

# shellcheck disable=SC2329  # run by cleanup
leftovers() {
  local objects=""
  [[ $PURGE -eq 0 ]] || return 0 # deploy.sh --delete --purge destroyed them
  objects=$(printf '%s\n' "$OBJECTS" | sed '/^$/d')
  say "Left in your account, and billed until you delete them:"
  say "  buckets  $BACKUP $objects  (empty every version, then:  aws s3 rb s3://BUCKET)  or run this again with --purge"
  say "  snapshots of volume $VOLUME  (aws ec2 describe-snapshots --owner-ids self --filters Name=volume-id,Values=$VOLUME)"
}

OBJECTS="" ALLOCATION=""
trap cleanup EXIT

stack_status() {
  "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text
}
settled_status() { # waits up to 30 minutes for the stack to stop changing and prints its status
  local st="" i
  for i in $(seq 1 120); do
    st=$(stack_status || true)
    case $st in
      *_IN_PROGRESS) sleep 15 ;;
      *) break ;;
    esac
  done
  printf '%s' "$st"
}
# This checkout's template, staged in the stack's backup bucket (it is over the size the API takes inline).
stage_new_template() {
  "${AWS[@]}" s3 cp "$NEW_TEMPLATE" "s3://$BACKUP/_rehearsal/supavise.yaml" >/dev/null || return 1
  printf 'https://%s.s3.%s.amazonaws.com/_rehearsal/supavise.yaml\n' "$BACKUP" "$REGION"
}

if [[ $CONSOLE -eq 1 ]]; then
  SAVED=$PWD/rehearsal-changeset-$STACK.json
  say "== 2b. the change set of the update, as AWS gives it for a stack made in the console"
  "${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" --output json >"$WORK/stack.json"
  IMAGE_BEFORE=$("${AWS[@]}" ec2 describe-instances --instance-ids "$INSTANCE" --query 'Reservations[0].Instances[0].ImageId' --output text)
  URL=$(stage_new_template) || die "cannot stage the template in s3://$BACKUP"
  "$DEPLOY" __params "$WORK/stack.json" "$NEW_TEMPLATE" "AmiId=$IMAGE_BEFORE" Failover=on PeerCidr1=198.51.100.0/24 >"$WORK/params-capture.json" \
    || die "deploy.sh could not build the parameters of the update"
  if "${AWS[@]}" cloudformation create-change-set --stack-name "$STACK" --change-set-name rehearsal-capture --change-set-type UPDATE \
      --capabilities CAPABILITY_IAM --template-url "$URL" --parameters "file://$WORK/params-capture.json" >/dev/null \
    && "${AWS[@]}" cloudformation wait change-set-create-complete --stack-name "$STACK" --change-set-name rehearsal-capture; then
    if "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name rehearsal-capture --include-property-values --output json >"$SAVED" \
      || "${AWS[@]}" cloudformation describe-change-set --stack-name "$STACK" --change-set-name rehearsal-capture --output json >"$SAVED"; then
      say "INFO  the change set is saved in $SAVED"
    else
      say "WARN  the change set could not be read, so none was saved"
    fi
  else
    say "WARN  the change set could not be made, so none was saved"
  fi
  "${AWS[@]}" cloudformation delete-change-set --stack-name "$STACK" --change-set-name rehearsal-capture >/dev/null 2>&1 || true
fi

if [[ $DENIAL -eq 1 ]]; then
  say "== 2c. a replacement the guard denies: an update to this template with another image (about 10 minutes)"
  other=$DENIAL_AMI
  if [[ -z $other ]]; then
    # An older build of the image the template uses: neither the instance's nor the current one.
    latest=$("${AWS[@]}" ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text || true)
    other=$("${AWS[@]}" ec2 describe-images --owners 099720109477 --filters "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*" \
      --query 'reverse(sort_by(Images,&CreationDate))[].ImageId' --output text | tr '\t' '\n' | grep -v -x -e "$IMAGE_BEFORE" -e "${latest:-none}" | head -n 1 || true)
  fi
  [[ $other =~ ^ami-[0-9a-f]{8,17}$ ]] || die "found no other Ubuntu 24.04 arm64 image for the denial; pass --denial-ami"
  "${AWS[@]}" cloudformation get-stack-policy --stack-name "$STACK" --output json >"$WORK/policy-own.json"
  echo '{}' >"$WORK/no-changes.json"
  "$DEPLOY" __guard "$WORK/policy-own.json" "$NEW_TEMPLATE" "$WORK/no-changes.json" "$WORK/policy-back.json" >"$WORK/policy-guard.json" \
    || die "deploy.sh could not build the guard"
  "$DEPLOY" __params "$WORK/stack.json" "$NEW_TEMPLATE" "AmiId=$other" Failover=on PeerCidr1=198.51.100.0/24 >"$WORK/params-denial.json" \
    || die "deploy.sh could not build the parameters of the update"
  "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://$WORK/policy-guard.json"
  say "The guard is set. Updating to this template with image $other (the instance runs $IMAGE_BEFORE) ..."
  rc=0
  "${AWS[@]}" cloudformation update-stack --stack-name "$STACK" --template-url "$URL" --capabilities CAPABILITY_IAM \
    --parameters "file://$WORK/params-denial.json" >/dev/null || rc=1
  report "CloudFormation took the update that needs another image" "$rc"
  DSTATUS=$(settled_status)
  rc=0; [[ $DSTATUS == UPDATE_ROLLBACK_COMPLETE ]] || rc=1
  report "the update failed and rolled back completely with the guard on (UPDATE_ROLLBACK_COMPLETE; got: $DSTATUS)" "$rc"
  "${AWS[@]}" cloudformation describe-stack-events --stack-name "$STACK" --max-items 100 \
    --query "StackEvents[?ResourceStatus=='UPDATE_FAILED' || ResourceStatus=='DELETE_FAILED'].[LogicalResourceId,ResourceStatus,ResourceStatusReason]" --output text >"$WORK/denial-events" || true
  rc=0; grep -qi 'stack policy' "$WORK/denial-events" || rc=1
  report "an event of the update names the stack policy" "$rc"
  say "INFO  what failed in the update (the script's message relies on its wording):"
  sed 's/^/        /' "$WORK/denial-events"
  if [[ $DSTATUS == UPDATE_ROLLBACK_FAILED ]]; then
    say "The rollback stopped with the guard on: putting the policy back and resuming the rollback ..."
    "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://$WORK/policy-back.json" || true
    "${AWS[@]}" cloudformation continue-update-rollback --stack-name "$STACK" || true
    say "INFO  after the rollback was resumed without the guard: $(settled_status)"
  fi
  rc=0; [[ $(out_of InstanceId) == "$INSTANCE" && $(instance_facts "$INSTANCE" || true) == "$BEFORE" ]] || rc=1
  report "the instance is the same after the denial (same id and launch time, running)" "$rc"
  rc=0; "${AWS[@]}" cloudformation set-stack-policy --stack-name "$STACK" --stack-policy-body "file://$WORK/policy-back.json" || rc=1
  report "the stack's policy is put back after the denial" "$rc"
fi

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
if [[ $CONSOLE -eq 1 ]]; then
  rc=0; [[ -z $AMI_BEFORE || $AMI_BEFORE == None ]] || rc=1
  report "the stack was made with an empty AmiId, as the console makes it (it had: ${AMI_BEFORE:-nothing})" "$rc"
  IMAGE=$("${AWS[@]}" ec2 describe-instances --instance-ids "$INSTANCE" --query 'Reservations[0].Instances[0].ImageId' --output text || true)
  AMI_AFTER=$(ami_param || true)
  rc=0; [[ -n $IMAGE && $AMI_AFTER == "$IMAGE" ]] || rc=1
  report "the stack's AmiId is the image the instance runs ($AMI_AFTER, instance: $IMAGE)" "$rc"
  POLICY=$("${AWS[@]}" cloudformation get-stack-policy --stack-name "$STACK" --query StackPolicyBody --output text || true)
  rc=0; [[ $POLICY == *'"Allow"'* && $POLICY != *'"Deny"'* ]] || rc=1
  report "the stack policy allows every update again (the guard is off): $POLICY" "$rc"
  say "INFO  the review called the instance: $(grep -m1 'Instance (AWS::EC2::Instance)' "$UPDATE_LOG" | sed 's/^ *//' || true)"
fi

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
      expect DryRunOperation
    aws ec2 associate-address --dry-run --allocation-id ${ALLOCATION:-ALLOCATION_ID} --instance-id $INSTANCE --private-ip-address PRIVATE_IP --allow-reassociation --region $REGION
      PRIVATE_IP is a private address of the instance (aws ec2 describe-instances --instance-ids $INSTANCE --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text --region $REGION).
      expect DryRunOperation. UnauthorizedOperation means that FencingPolicy, which lists no network interface, does not cover
      the interface the address would go to: a failover onto a replica server, whose service address is a second private address,
      could not move it (the policy then needs a network-interface resource).
  The instance was not rebooted. The launch time stays the same across a reboot, so look at the boot time:
    uptime -s
      expect a time before $START (UTC, when the update began)
  The service address stays where a failover put it. Move it by hand to another instance of yours in $REGION:
    aws ec2 associate-address --allocation-id ${ALLOCATION:-ALLOCATION_ID} --instance-id OTHER_INSTANCE --allow-reassociation --region $REGION
  copy deploy/cloudformation/supavise.yaml and add { Key: rehearsal, Value: "1" } to the Tags of the resource Instance. While the address is on
  another server, $DEPLOY update holds back any update of the instance, so run it first to see that:
    $DEPLOY update --region $REGION --stack $STACK --template THE_COPY --set Failover=on --set PeerCidr1=198.51.100.0/24 --yes
      expect: blocked, "an update of the instance can move it back", the change set deleted, exit status 2
  Then make the same change without that check, to see what CloudFormation does (this stack is a throwaway):
    aws cloudformation deploy --region $REGION --stack-name $STACK --template-file THE_COPY --s3-bucket A_BUCKET_OF_YOURS_IN_$REGION --capabilities CAPABILITY_IAM \\
      --parameter-overrides Failover=on PeerCidr1=198.51.100.0/24
      (a parameter that is not named keeps the stack's value; the template is over the size the API takes inline, so it goes through a bucket)
    aws ec2 describe-addresses --allocation-ids ${ALLOCATION:-ALLOCATION_ID} --query 'Addresses[0].InstanceId' --output text --region $REGION
      expect OTHER_INSTANCE. If it prints $INSTANCE, CloudFormation attached the address again when it updated the instance: an update
      after a failover would take the service address back, and the check that holds such updates back is needed as it is. If it
      prints OTHER_INSTANCE, a change of tags leaves the address alone, and that check can be narrowed (judge() in deploy.sh).
  Then run  $DEPLOY replica --leader-stack $STACK ...  against this stack to see the second server join (see deploy/aws/README.md).
LIST
if [[ $YES -eq 0 && -t 0 ]]; then
  printf 'Check the node, then press Enter to go on to the deletion: '
  read -r _ || true
fi
if [[ $FAILED -eq 0 ]]; then say "== every check passed"; else say "== at least one check FAILED"; fi
exit "$FAILED"
