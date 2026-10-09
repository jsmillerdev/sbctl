#!/usr/bin/env bash
# Refuses a release unless the tagged commit has already passed its tests.
#
#   deploy/release-gate.sh COMMIT_SHA
#
# release.yml runs it before it builds anything. It asks the GitHub API (through `gh`, with
# GH_TOKEN and GITHUB_REPOSITORY set) for the runs of the workflows below on COMMIT_SHA and
# succeeds only when the newest run of each ended in success. A run that is still going is waited
# for. Runs from pull requests do not count (see newest_run). A workflow with no run on the commit fails the gate: push the commit to main (or dispatch the
# workflow on the branch) and tag again.
#
# REQUIRED is a space-separated list of WORKFLOW-FILE or WORKFLOW-FILE:JOB-PREFIX. A bare file
# needs its newest run on the commit to succeed. With a prefix, at least one job of that run
# whose name starts with it (any case) must exist and every such job must have succeeded. The
# default list is the whole of ci.yml and linux.yml, the conformance suite (the job `suites` of
# conformance.yml, both architectures), the upgrade test (the jobs of linux.yml named
# `upgrade...`) and the two-server release test (the jobs `two-servers` of replication.yml). That
# workflow runs on pushes to integrate/** (both architectures) and ws/** branches listed in it (amd64
# only), not on main: dispatch it on the commit to tag, `gh workflow run replication.yml --ref <branch>`,
# which runs both architectures. Rename the upgrade test's job and this gate fails closed, with a message that names
# what it did not find: change the prefix here.
#
# GATE_TIMEOUT_SECONDS (default 10800: a replication run takes 35 to 75 minutes when it passes and is cut
# off at 150 when a wait times out) bounds the wait for runs in progress, GATE_POLL_SECONDS
# (60) is the pause between looks, GATE_GRACE_SECONDS (300) is how long a workflow with no
# run yet is waited for, because a tag pushed right after a merge can arrive before its runs start.
set -euo pipefail

sha=${1:-}
[[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "usage: $0 COMMIT_SHA (40 hex characters)" >&2; exit 2; }
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}
required=${REQUIRED:-"ci.yml linux.yml conformance.yml:suites linux.yml:upgrade replication.yml:two-servers"}
timeout=${GATE_TIMEOUT_SECONDS:-10800}
poll=${GATE_POLL_SECONDS:-60}
grace=${GATE_GRACE_SECONDS:-300}
begin=$(date +%s)

fail() { echo "::error::release gate: $*" >&2; exit 1; }

# newest_run WORKFLOW prints the newest run of the workflow on the commit as one JSON line, or nothing.
# Only runs that tested the commit itself count. A pull_request run reports the PR head as its
# head_sha but tests the merge of the PR into its base, so a green one proves nothing about the tag.
newest_run() {
  gh api "repos/$repo/actions/workflows/$1/runs?head_sha=$sha&per_page=100" |
    jq -c --arg sha "$sha" '[.workflow_runs[] | select(.head_sha == $sha and (.event | IN("push", "workflow_dispatch", "schedule")))] | sort_by(.created_at) | last // empty'
}

# settled_run WORKFLOW waits for the newest run to finish and prints it.
settled_run() {
  local wf=$1 run status
  while :; do
    run=$(newest_run "$wf")
    now=$(date +%s)
    if [[ -z $run ]]; then
      (( now - begin < grace )) || fail "$wf has no run on $sha. Push the commit to main, or run: gh workflow run $wf --ref <branch>; then tag again."
    else
      status=$(jq -r .status <<<"$run")
      [[ $status == completed ]] && { printf '%s' "$run"; return; }
      echo "$wf: run $(jq -r .id <<<"$run") is $status" >&2
    fi
    (( now - begin < timeout )) || fail "gave up waiting for $wf on $sha after ${timeout}s"
    sleep "$poll"
  done
}

# One look per workflow, however many entries name it (plain files, so that bash 3.2 runs this too).
cache=$(mktemp -d)
trap 'rm -rf "$cache"' EXIT
for entry in $required; do
  wf=${entry%%:*}
  [[ -s $cache/$wf ]] || settled_run "$wf" >"$cache/$wf"
done

for entry in $required; do
  wf=${entry%%:*}
  run=$(cat "$cache/$wf")
  id=$(jq -r .id <<<"$run")
  conclusion=$(jq -r .conclusion <<<"$run")
  if [[ $entry != *:* ]]; then
    [[ $conclusion == success ]] || fail "$wf run $id on $sha ended $conclusion"
    echo "ok: $wf run $id succeeded"
    continue
  fi
  prefix=${entry#*:}
  jobs=$(gh api "repos/$repo/actions/runs/$id/jobs?per_page=100" |
    jq -c --arg p "$prefix" '[.jobs[] | select((.name | ascii_downcase) | startswith($p | ascii_downcase))]')
  n=$(jq 'length' <<<"$jobs")
  (( n > 0 )) || fail "$wf run $id has no job named $prefix*: the gate cannot see that test (rename it back, or change REQUIRED in deploy/release-gate.sh)"
  bad=$(jq -r '[.[] | select(.conclusion != "success") | "\(.name) (\(.conclusion))"] | join(", ")' <<<"$jobs")
  [[ -z $bad ]] || fail "$wf run $id: $bad on $sha"
  echo "ok: $n job(s) of $wf named $prefix* succeeded (run $id)"
done
echo "release gate passed for $sha"
